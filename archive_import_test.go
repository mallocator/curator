package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCopyFileNeverReplacesArchive(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "source"), filepath.Join(dir, "archive")
	os.WriteFile(src, []byte("new-content"), 0600)
	os.WriteFile(dst, []byte("original"), 0600)
	if err := copyFile(src, dst, 11); err == nil {
		t.Fatal("copy replaced an existing archive file")
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "original" {
		t.Fatalf("archive changed to %q", got)
	}
}

func TestArchiveImporterBoundary(t *testing.T) {
	dir := t.TempDir()
	source, dest, outside := filepath.Join(dir, "downloads"), filepath.Join(dir, "archive"), filepath.Join(dir, "outside")
	for _, d := range []string{source, dest, outside} {
		os.Mkdir(d, 0700)
	}
	os.WriteFile(filepath.Join(source, "movie"), []byte("abc"), 0600)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600)
	os.Symlink(outside, filepath.Join(source, "escape"))
	os.Symlink(outside, filepath.Join(dest, "escape"))
	importer, err := newArchiveImporter(source, []string{dest})
	if err != nil {
		t.Fatal(err)
	}
	defer importer.Close()
	for _, tc := range []struct {
		name, src, dst string
		size           int64
		ok             bool
	}{
		{"copy", filepath.Join(source, "movie"), filepath.Join(dest, "show", "episode"), 3, true},
		{"idempotent", filepath.Join(source, "movie"), filepath.Join(dest, "show", "episode"), 3, true},
		{"outside source", filepath.Join(outside, "secret"), filepath.Join(dest, "secret"), 6, false},
		{"source symlink", filepath.Join(source, "escape", "secret"), filepath.Join(dest, "secret"), 6, false},
		{"outside destination", filepath.Join(source, "movie"), filepath.Join(outside, "movie"), 3, false},
		{"destination symlink", filepath.Join(source, "movie"), filepath.Join(dest, "escape", "movie"), 3, false},
		{"size mismatch", filepath.Join(source, "movie"), filepath.Join(dest, "bad"), 99, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := importer.Copy(tc.src, tc.dst, tc.size)
			if (err == nil) != tc.ok {
				t.Fatalf("Copy error %v, want success %v", err, tc.ok)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(outside, "movie")); !os.IsNotExist(err) {
		t.Fatal("escaped archive root")
	}
	if _, err := os.Stat(filepath.Join(dest, "bad")); !os.IsNotExist(err) {
		t.Fatal("published incomplete copy")
	}
	os.WriteFile(filepath.Join(source, "larger"), []byte("changed"), 0600)
	if err := importer.Copy(filepath.Join(source, "larger"), filepath.Join(dest, "show", "episode"), 7); err == nil {
		t.Fatal("overwrote archive")
	}
	got, _ := os.ReadFile(filepath.Join(dest, "show", "episode"))
	if string(got) != "abc" {
		t.Fatal("existing archive changed")
	}
}

func TestArchiveImportSocket(t *testing.T) {
	dir := t.TempDir()
	source, dest := filepath.Join(dir, "downloads"), filepath.Join(dir, "archive")
	os.Mkdir(source, 0700)
	os.Mkdir(dest, 0700)
	src := filepath.Join(source, "movie")
	os.WriteFile(src, []byte("abc"), 0600)
	a, err := newArchiveImporter(source, []string{dest})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// macOS's UNIX socket path limit is shorter than Go's test temp paths.
	socketDir, err := os.MkdirTemp("/tmp", "curator-socket-") //nolint:usetesting // macOS socket paths must fit sockaddr_un; t.TempDir paths exceed it.
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "import.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: archiveImportHandler(a), ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	t.Setenv("ARCHIVE_IMPORT_SOCKET", socket)
	if err := copyFile(src, filepath.Join(dest, "movie"), 3); err != nil {
		t.Fatal(err)
	}
	// Equal size is insufficient to delete a download: require identical archived bytes.
	os.WriteFile(src, []byte("xyz"), 0600)
	if err := copyFile(src, filepath.Join(dest, "movie"), 3); err == nil {
		t.Fatal("accepted different content as already archived")
	}
	client := archiveHTTPClient(socket)
	defer client.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodDelete, "http://archive/copy", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE: %s", resp.Status)
	}
}

func TestRemoveStaleStaging(t *testing.T) {
	dir := t.TempDir()
	source, dest, outside := filepath.Join(dir, "downloads"), filepath.Join(dir, "archive"), filepath.Join(dir, "outside")
	for _, d := range []string{source, filepath.Join(dest, "show"), outside} {
		os.MkdirAll(d, 0700)
	}
	stale := filepath.Join(dest, "show", ".curator-import-H4HLMONQ62LHUL6OXJDDRQX3TD")
	linked := filepath.Join(dest, ".curator-import-AAAAAAAAAAAAAAAAAAAAAAAAAA")
	published := filepath.Join(dest, "movie.mkv")
	lookalike := filepath.Join(dest, ".curator-import-notmine")
	symlink := filepath.Join(dest, ".curator-import-BBBBBBBBBBBBBBBBBBBBBBBBBB")
	staleDir := filepath.Join(dest, ".curator-import-CCCCCCCCCCCCCCCCCCCCCCCCCC")
	victim := filepath.Join(outside, ".curator-import-DDDDDDDDDDDDDDDDDDDDDDDDDD")
	for _, f := range []string{stale, published, lookalike, victim} {
		os.WriteFile(f, []byte("abc"), 0600)
	}
	os.Link(published, linked) // killed after publishing, before the deferred Remove
	os.Symlink(victim, symlink)
	os.Mkdir(staleDir, 0700)
	os.Symlink(outside, filepath.Join(dest, "escape"))
	importer, err := newArchiveImporter(source, []string{dest})
	if err != nil {
		t.Fatal(err)
	}
	defer importer.Close()
	importer.RemoveStaleStaging()
	for _, f := range []string{stale, linked} {
		if _, err := os.Lstat(f); !os.IsNotExist(err) {
			t.Errorf("%s not removed", f)
		}
	}
	for _, f := range []string{published, lookalike, symlink, staleDir, victim} {
		if _, err := os.Lstat(f); err != nil {
			t.Errorf("%s removed: %v", f, err)
		}
	}
}
