package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestQbt(t *testing.T, baseURL, user, pass string) *Qbittorrent {
	t.Helper()
	q, err := NewQbittorrent(baseURL, user, pass, "")
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// A self-signed WebUI is trusted only through QBT_CA_FILE: without it the login fails the certificate check.
func TestQbittorrentCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestQbt(t, srv.URL, "admin", "secret").Login(); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("login without CA file: %v, want a certificate error", err)
	}

	caFile := filepath.Join(t.TempDir(), "qbt.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	q, err := NewQbittorrent(srv.URL, "admin", "secret", caFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Login(); err != nil {
		t.Fatalf("login with CA file: %v", err)
	}
}

func TestQbittorrentCAFileInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewQbittorrent("https://qbt", "", "", filepath.Join(dir, "missing.pem")); err == nil {
		t.Error("missing CA file accepted")
	}
	notPEM := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(notPEM, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewQbittorrent("https://qbt", "", "", notPEM); err == nil {
		t.Error("CA file without a certificate accepted")
	}
}
