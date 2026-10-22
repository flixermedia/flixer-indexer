package api

import (
	"crypto/sha1"
	"encoding/hex"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestPersonImageServedFromCacheAndRestrictedToProviders(t *testing.T) {
	cache := t.TempDir()
	photo := "https://image.tmdb.org/t/p/w185/abc.jpg"
	sum := sha1.Sum([]byte(photo))
	cached := filepath.Join(cache, personCacheDir, hex.EncodeToString(sum[:])+".jpg")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, []byte("jpeg bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := (&Server{Secret: secret, CachePath: cache, Log: slog.Default()}).Handler()

	rec := do(h, "POST", "/internal/people/image", personImageRequest{URL: photo}, true)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Body.String() != "jpeg bytes" {
		t.Fatalf("cached photo = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, bad := range []string{"http://image.tmdb.org/a.jpg", "https://evil.example/a.jpg", "https://user:pw@image.tmdb.org/a.jpg", ""} {
		if rec := do(h, "POST", "/internal/people/image", personImageRequest{URL: bad}, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%q = %d", bad, rec.Code)
		}
	}
	if rec := do(h, "POST", "/internal/people/image", personImageRequest{URL: photo}, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned = %d", rec.Code)
	}
}
