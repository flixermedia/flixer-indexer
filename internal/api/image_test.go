package api

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestImageServedFromCacheOnly(t *testing.T) {
	cache := t.TempDir()
	png := filepath.Join(cache, "poster.png")
	if err := os.WriteFile(png, []byte("not really a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := (&Server{Secret: secret, CachePath: cache, Log: slog.Default()}).Handler()

	rec := do(h, "POST", "/internal/image", imageRequest{Path: png}, true)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("cache image = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := do(h, "POST", "/internal/image", imageRequest{Path: png}, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned = %d", rec.Code)
	}
	if rec := do(h, "POST", "/internal/image", imageRequest{Path: filepath.Join(cache, "notes.txt")}, true); rec.Code != http.StatusBadRequest {
		t.Errorf("non-image extension = %d", rec.Code)
	}
	if rec := do(h, "POST", "/internal/image", imageRequest{Path: "relative/poster.png"}, true); rec.Code != http.StatusNotFound {
		t.Errorf("missing relative path = %d", rec.Code)
	}
}

func TestImageOutsideFoldersForbidden(t *testing.T) {
	h := newServer(t)
	outside := filepath.Join(t.TempDir(), "secret.png")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec := do(h, "POST", "/internal/image", imageRequest{Path: outside}, true); rec.Code != http.StatusForbidden {
		t.Errorf("outside path = %d", rec.Code)
	}
}
