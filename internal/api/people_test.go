package api

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestPersonImageServedFromCacheAndRestrictedToProviders(t *testing.T) {
	cache := t.TempDir()
	personID := "01234567-89ab-cdef-0123-456789abcdef"
	photo := "https://image.tmdb.org/t/p/w185/abc.jpg"
	cached := filepath.Join(cache, personCacheDir, personID+".jpg")
	if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(cached, []byte("jpeg bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := (&Server{Secret: secret, CachePath: cache, Log: slog.Default()}).Handler()

	rec := do(h, "POST", "/internal/people/image", personImageRequest{ID: personID, URL: photo}, true)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || rec.Body.String() != "jpeg bytes" {
		t.Fatalf("cached photo = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, bad := range []string{"http://image.tmdb.org/a.jpg", "https://evil.example/a.jpg", "https://user:pw@image.tmdb.org/a.jpg", ""} {
		if rec := do(h, "POST", "/internal/people/image", personImageRequest{ID: personID, URL: bad}, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%q = %d", bad, rec.Code)
		}
	}

	if rec := do(h, "POST", "/internal/people/image", personImageRequest{ID: personID, URL: photo}, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned = %d", rec.Code)
	}
}

func TestDownloadedPersonPhotoIsStoredAsJPEGByPersonID(t *testing.T) {
	var pngData bytes.Buffer
	source := image.NewRGBA(image.Rect(0, 0, 2, 2))
	source.Set(0, 0, color.RGBA{R: 20, G: 50, B: 90, A: 255})
	if err := png.Encode(&pngData, source); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngData.Bytes())
	}))
	defer server.Close()
	originalHTTP := personHTTP
	personHTTP = server.Client()
	t.Cleanup(func() {
		personHTTP = originalHTTP
	})

	base := filepath.Join(t.TempDir(), "people", "01234567-89ab-cdef-0123-456789abcdef.jpg")
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	path, mime, err := downloadPersonPhoto(request, server.URL, base)
	if err != nil || path != base || mime != "image/jpeg" {
		t.Fatalf("download = %q, %q, %v", path, mime, err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, format, err := image.DecodeConfig(file); err != nil || format != "jpeg" {
		t.Fatalf("stored image format = %q, %v", format, err)
	}
}
