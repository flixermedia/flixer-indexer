package themes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseChecksum(t *testing.T) {
	manifest := `# SHA2-256SUMS for yt-dlp
e51e69e89489f199530f13d32a4342da3ddb2cf34842f364603a066db05afbf3  yt-dlp
767ba9dce7052aa196f8b12652f433d76de128ba168966fa84b6394d5e6c6795 *yt-dlp_linux
0f922c15ccc0b83eefa1c8bc9104e434318c4bf16e408e1ee93b7745a6efe0aa  yt-dlp_macos
f5389d3698ecde4ef06f4e6b1ce0294fe9ac1396228c8990b8d3416d6d177a7d  yt-dlp.exe
`

	got, err := parseChecksum(manifest, "yt-dlp_linux")
	if err != nil {
		t.Fatalf("parseChecksum failed: %v", err)
	}
	want := "767ba9dce7052aa196f8b12652f433d76de128ba168966fa84b6394d5e6c6795"
	if got != want {
		t.Fatalf("parseChecksum = %q, want %q", got, want)
	}

	gotMac, err := parseChecksum(manifest, "yt-dlp_macos")
	if err != nil {
		t.Fatalf("parseChecksum failed: %v", err)
	}
	wantMac := "0f922c15ccc0b83eefa1c8bc9104e434318c4bf16e408e1ee93b7745a6efe0aa"
	if gotMac != wantMac {
		t.Fatalf("parseChecksum = %q, want %q", gotMac, wantMac)
	}

	if _, err := parseChecksum(manifest, "yt-dlp_nonexistent"); err == nil {
		t.Fatal("expected error for nonexistent asset")
	}

	badManifest := "short_hash yt-dlp_linux\n"
	if _, err := parseChecksum(badManifest, "yt-dlp_linux"); err == nil {
		t.Fatal("expected error for invalid sha256 length")
	}
}

func TestYTDLPDownloadVerifiesChecksum(t *testing.T) {
	binaryContent := []byte("#!/bin/sh\necho yt-dlp-mock\n")
	hasher := sha256.New()
	hasher.Write(binaryContent)
	expectedHash := hex.EncodeToString(hasher.Sum(nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA2-256SUMS":
			_, _ = w.Write([]byte(expectedHash + "  yt-dlp_linux\n"))
		case "/yt-dlp_linux":
			_, _ = w.Write(binaryContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	y := NewYTDLP(dir, "test-agent")
	y.BaseURL = srv.URL + "/"

	dest := filepath.Join(dir, "yt-dlp")
	if err := y.download(context.Background(), "yt-dlp_linux", dest); err != nil {
		t.Fatalf("download failed: %v", err)
	}

	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read downloaded binary: %v", err)
	}
	if string(data) != string(binaryContent) {
		t.Fatalf("content mismatch: got %q, want %q", string(data), string(binaryContent))
	}
}

func TestYTDLPDownloadRejectsChecksumMismatch(t *testing.T) {
	binaryContent := []byte("tampered-content")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA2-256SUMS":
			// Correct length hash, but does not match binaryContent
			_, _ = w.Write([]byte("0000000000000000000000000000000000000000000000000000000000000000  yt-dlp_linux\n"))
		case "/yt-dlp_linux":
			_, _ = w.Write(binaryContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	y := NewYTDLP(dir, "test-agent")
	y.BaseURL = srv.URL + "/"

	dest := filepath.Join(dir, "yt-dlp")
	err := y.download(context.Background(), "yt-dlp_linux", dest)
	if err == nil {
		t.Fatal("expected download to fail on checksum mismatch")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("unexpected error message: %v", err)
	}

	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("dest file should not exist after failure")
	}

	// Verify temporary files are cleaned up
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".yt-dlp-") {
			t.Fatalf("temporary file %q was not removed", entry.Name())
		}
	}
}

func TestYTDLPDownloadFailsWhenChecksumMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA2-256SUMS":
			_, _ = w.Write([]byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa  other_asset\n"))
		case "/yt-dlp_linux":
			_, _ = w.Write([]byte("some-content"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	y := NewYTDLP(dir, "test-agent")
	y.BaseURL = srv.URL + "/"

	dest := filepath.Join(dir, "yt-dlp")
	err := y.download(context.Background(), "yt-dlp_linux", dest)
	if err == nil {
		t.Fatal("expected download to fail when asset is missing in SHA2-256SUMS")
	}
	if !strings.Contains(err.Error(), "not found in SHA2-256SUMS") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestYTDLPDownloadFailsOnChecksumHttpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dir := t.TempDir()
	y := NewYTDLP(dir, "test-agent")
	y.BaseURL = srv.URL + "/"

	dest := filepath.Join(dir, "yt-dlp")
	err := y.download(context.Background(), "yt-dlp_linux", dest)
	if err == nil {
		t.Fatal("expected download to fail on HTTP error")
	}
}
