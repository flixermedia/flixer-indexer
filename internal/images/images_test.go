package images

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/mediaid"
	"github.com/flixermedia/flixer-indexer/internal/models"
)

func pngBytes(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok.png":
			w.Write(pngBytes(200, 300))
		case "/tiny.png":
			w.Write(pngBytes(4, 4))
		default:
			w.Write([]byte("<html>not an image</html>"))
		}
	}))
}

func TestSaveAlongsideAndCache(t *testing.T) {
	srv := server()
	defer srv.Close()
	root, cache := t.TempDir(), t.TempDir()
	movieDir := filepath.Join(root, "Movie (2000)")
	_ = os.MkdirAll(movieDir, 0o755)
	item := models.Item{Type: models.ItemMovie, Path: filepath.Join(movieDir, "Movie (2000).mkv"), RootID: "root", RelativePath: "Movie (2000)/Movie (2000).mkv"}
	s := NewStore(cache, "ffmpeg", "test")

	p := Placement{Library: models.Library{ID: "l", ImageStorage: models.StorageAlongside}, Item: item, FolderRoot: root}
	img, err := s.Save(context.Background(), p, models.Image{Type: "poster", URL: srv.URL + "/ok.png"}, 0)
	if err != nil || img.Path != filepath.Join(movieDir, "poster.png") || img.Width != 200 {
		t.Fatalf("alongside: %+v, %v", img, err)
	}

	p.Library.ImageStorage = models.StorageCache
	img, err = s.Save(context.Background(), p, models.Image{Type: "backdrop", URL: srv.URL + "/ok.png"}, 1)
	key, _ := mediaid.Key(item.RootID, item.RelativePath)
	if err != nil || filepath.Dir(img.Path) != filepath.Join(cache, "images", key) || filepath.Base(img.Path) != "backdrop1.png" {
		t.Fatalf("cache: %+v, %v", img, err)
	}
}

func TestSaveReusesCacheAfterItemRescan(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Write(pngBytes(200, 300))
	}))
	defer srv.Close()
	item := models.Item{Type: models.ItemMovie, Path: "/media/movie.mkv", RootID: "root", RelativePath: "movie.mkv"}
	s := NewStore(t.TempDir(), "ffmpeg", "test")
	placement := Placement{Library: models.Library{ImageStorage: models.StorageCache}, Item: item}
	first, err := s.Save(context.Background(), placement, models.Image{Type: "poster", URL: srv.URL + "/poster.png"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Save(context.Background(), placement, models.Image{Type: "poster", URL: "http://unreachable.invalid/poster.png"}, 0)
	if err != nil || second.Path != first.Path || requests != 1 {
		t.Fatalf("reused image = %+v, requests = %d, error = %v", second, requests, err)
	}
}

func TestSaveRejectsBadImages(t *testing.T) {
	srv := server()
	defer srv.Close()
	s := NewStore(t.TempDir(), "ffmpeg", "test")
	p := Placement{Library: models.Library{ID: "l"}, Item: models.Item{Type: models.ItemMovie, Path: "x.mkv"}}
	for _, path := range []string{"/tiny.png", "/html"} {
		if _, err := s.Save(context.Background(), p, models.Image{Type: "poster", URL: srv.URL + path}, 0); err == nil {
			t.Errorf("%s should be rejected", path)
		}
	}
}

func TestFindLocal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Movie (2000)")
	_ = os.MkdirAll(dir, 0o755)
	for _, n := range []string{"Movie (2000).mkv", "poster.jpg", "fanart.jpg", "fanart1.jpg", "Movie (2000)-thumb.jpg", "random.jpg"} {
		_ = os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644)
	}
	got := FindLocal(models.Item{Type: models.ItemMovie, Path: filepath.Join(dir, "Movie (2000).mkv")}, root)
	counts := map[string]int{}
	for _, i := range got {
		counts[i.Type]++
	}
	if counts["poster"] != 1 || counts["backdrop"] != 2 || counts["thumb"] != 1 || len(got) != 4 {
		t.Errorf("found = %v", counts)
	}
}

func TestFindLocalSeason(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "season01-poster.jpg"), []byte("x"), 0o644)
	n := 1
	got := FindLocal(models.Item{Type: models.ItemSeason, Path: filepath.Join(root, "Season 1"), ParentPath: root, SeasonNumber: &n}, root)
	if len(got) != 1 || got[0].Type != "poster" {
		t.Errorf("found = %+v", got)
	}
}
