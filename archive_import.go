package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// archiveImporter has no deletion or replacement operation. Its roots are opened once,
// so symlinks and concurrent path changes cannot escape the configured mounts.
type archiveImporter struct {
	sourcePath   string
	source       *os.Root
	destinations map[string]*os.Root
	mu           sync.Mutex
}

func newArchiveImporter(source string, destinations []string) (*archiveImporter, error) {
	source = filepath.Clean(source)
	if !filepath.IsAbs(source) {
		return nil, errors.New("import source must be absolute")
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return nil, err
	}
	a := &archiveImporter{sourcePath: source, source: root, destinations: map[string]*os.Root{}}
	for _, path := range destinations {
		path = filepath.Clean(path)
		if !filepath.IsAbs(path) || path == source || strings.HasPrefix(path, source+string(os.PathSeparator)) {
			a.Close()
			return nil, errors.New("archive roots must be absolute and separate from downloads")
		}
		r, err := os.OpenRoot(path)
		if err != nil {
			a.Close()
			return nil, err
		}
		a.destinations[path] = r
	}
	if len(a.destinations) == 0 {
		a.Close()
		return nil, errors.New("no archive roots")
	}
	return a, nil
}

func (a *archiveImporter) Close() {
	a.source.Close()
	for _, root := range a.destinations {
		root.Close()
	}
}

func below(root, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("path must be absolute")
	}
	rel, err := filepath.Rel(root, filepath.Clean(path))
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "", errors.New("path outside allowed root")
	}
	return rel, nil
}

func (a *archiveImporter) Copy(src, dst string, wantSize int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	rel, err := below(a.sourcePath, src)
	if err != nil {
		return err
	}
	// Nonblocking open prevents an attacker swapping a regular file for a FIFO from hanging the importer.
	in, err := a.source.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || wantSize < 0 || st.Size() != wantSize {
		return errors.New("source must be a regular file of the expected size")
	}
	for path, root := range a.destinations {
		name, err := below(path, dst)
		if err != nil {
			continue
		}
		return publishArchive(in, root, name, wantSize)
	}
	return errors.New("destination outside archive roots")
}

func sameArchive(in *os.File, root *os.Root, name string, size int64) error {
	st, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() != size {
		return errors.New("destination already exists; replacement forbidden")
	}
	existing, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer existing.Close()
	st, err = existing.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("destination is not a regular file")
	}
	if _, err = in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	left, right := sha256.New(), sha256.New()
	if _, err = io.Copy(left, in); err != nil {
		return err
	}
	if _, err = io.Copy(right, existing); err != nil {
		return err
	}
	if !bytes.Equal(left.Sum(nil), right.Sum(nil)) {
		return errors.New("destination content differs; replacement forbidden")
	}
	return syncArchiveDirectory(root)
}

const stagingPrefix = ".curator-import-"

// isStagingName matches only names publishArchive generates: the prefix and a 26-character rand.Text.
func isStagingName(name string) bool {
	suffix, ok := strings.CutPrefix(name, stagingPrefix)
	if !ok || len(suffix) != 26 {
		return false
	}
	for _, c := range suffix {
		if (c < 'A' || c > 'Z') && (c < '2' || c > '7') {
			return false
		}
	}
	return true
}

// RemoveStaleStaging removes staging files left by a copy that was killed mid-way (the deferred
// Remove doesn't run then). It runs before the importer accepts requests, so no copy is in flight.
func (a *archiveImporter) RemoveStaleStaging() {
	for path, root := range a.destinations {
		_ = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				log.Printf("Staging cleanup: %s: %v", filepath.Join(path, name), err)
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || !isStagingName(d.Name()) {
				return nil
			}
			if err := root.Remove(name); err != nil {
				log.Printf("Staging cleanup: %s: %v", filepath.Join(path, name), err)
			} else {
				log.Printf("Staging cleanup: removed %s", filepath.Join(path, name))
			}
			return nil
		})
	}
}

func syncArchiveDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func publishArchive(in *os.File, root *os.Root, name string, size int64) error {
	if err := root.MkdirAll(filepath.Dir(name), 0775); err != nil {
		return err
	}
	parent, err := root.OpenRoot(filepath.Dir(name))
	if err != nil {
		return err
	}
	defer parent.Close()
	name = filepath.Base(name)
	if _, err := parent.Lstat(name); err == nil {
		return sameArchive(in, parent, name, size)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	temp := stagingPrefix + rand.Text()
	out, err := parent.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0664)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Remove(temp) }() // only this request's unpublished staging file
	n, err := io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("copied %d bytes, expected %d", n, size)
	}
	// A hard link publishes atomically and refuses an existing name; Rename would overwrite it.
	if err := parent.Link(temp, filepath.Base(name)); err != nil {
		if errors.Is(err, os.ErrExist) {
			return sameArchive(in, parent, filepath.Base(name), size)
		}
		return err
	}
	return syncArchiveDirectory(parent)
}

type archiveCopyRequest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Size        int64  `json:"size"`
}

func archiveImportHandler(a *archiveImporter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /copy", func(w http.ResponseWriter, r *http.Request) {
		var req archiveCopyRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil {
			http.Error(w, "invalid copy request", 400)
			return
		}
		if err := a.Copy(req.Source, req.Destination, req.Size); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func archiveHTTPClient(socket string) *http.Client {
	return &http.Client{Timeout: 45 * time.Minute, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}}
}

func requestArchiveCopy(socket, src, dst string, size int64) error {
	body, err := json.Marshal(archiveCopyRequest{src, dst, size})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://archive/copy", bytes.NewReader(body))
	if err != nil {
		return err
	}
	client := archiveHTTPClient(socket)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("archive importer: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func runArchiveImporter() error {
	cfg := loadConfig()
	roots := []string{cfg.ArchiveDir}
	for _, root := range []string{cfg.AnimeArchiveDir, cfg.MovieArchiveDir} {
		if root != "" {
			roots = append(roots, root)
		}
	}
	a, err := newArchiveImporter(getEnv("ARCHIVE_SOURCE_DIR", "/downloads"), roots)
	if err != nil {
		return err
	}
	defer a.Close()
	a.RemoveStaleStaging()
	socket := os.Getenv("ARCHIVE_IMPORT_SOCKET")
	if socket == "" {
		return errors.New("ARCHIVE_IMPORT_SOCKET required")
	}
	// A stale socket can remain after container termination. Its directory is private to these two services.
	if st, err := os.Lstat(socket); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return errors.New("socket path is not a socket")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0660); err != nil {
		return err
	}
	server := &http.Server{Handler: archiveImportHandler(a), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 45 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	return server.Serve(listener)
}

func archiveImportHealthcheck() int {
	client := archiveHTTPClient(os.Getenv("ARCHIVE_IMPORT_SOCKET"))
	client.Timeout = 5 * time.Second
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://archive/health", nil)
	if err != nil {
		return 1
	}
	resp, err := client.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 1
	}
	return 0
}
