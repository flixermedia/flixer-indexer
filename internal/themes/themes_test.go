package themes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/mediaid"
	"github.com/flixermedia/flixer-indexer/internal/models"
)

func TestNormalizeURL(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":          "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ":                         "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		"http://www.youtube.com/watch?v=dQw4w9WgXcQ":           "",
		"https://evil.example/watch?v=dQw4w9WgXcQ":             "",
		"https://www.youtube.com/watch?v=--exec=touch%20/x":    "",
		"https://www.youtube.com/playlist?list=PL123456789012": "",
		"": "",
	}
	for in, want := range cases {
		if got := NormalizeURL(in); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAsset(t *testing.T) {
	cases := []struct {
		os, arch string
		musl     bool
		want     string
	}{
		{"linux", "amd64", false, "yt-dlp_linux"},
		{"linux", "arm64", true, "yt-dlp_musllinux_aarch64"},
		{"darwin", "arm64", false, "yt-dlp_macos"},
		{"windows", "amd64", false, "yt-dlp.exe"},
	}
	for _, c := range cases {
		if got, err := Asset(c.os, c.arch, c.musl); err != nil || got != c.want {
			t.Errorf("Asset(%s,%s,%v) = %q, %v; want %q", c.os, c.arch, c.musl, got, err, c.want)
		}
	}
	if _, err := Asset("plan9", "386", false); err == nil {
		t.Error("expected an error for an unsupported platform")
	}
}

func fakeFetcher(t *testing.T, handler http.HandlerFunc, runs *int) *Fetcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	f := NewFetcher(filepath.Join(t.TempDir(), "cache"), "ffmpeg-missing", "test")
	f.DBURL = srv.URL
	f.YTDLP.Lookup = func(string) (string, error) { return "yt-dlp", nil }
	f.Run = func(_ context.Context, _ string, args []string) ([]byte, error) {
		*runs++
		for i, a := range args {
			if a == "-o" {
				return nil, os.WriteFile(strings.Replace(args[i+1], "%(ext)s", "mp3", 1), []byte("audio"), 0o644)
			}
		}
		return nil, nil
	}
	return f
}

func entry(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/movies/themoviedb/603.json" {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write([]byte(`{"youtube_theme_url":"https://www.youtube.com/watch?v=dQw4w9WgXcQ"}`))
}

func TestFetchStorage(t *testing.T) {
	media := t.TempDir()
	movieDir := filepath.Join(media, "The Matrix")
	task := Task{LibraryID: "lib", Path: filepath.Join(movieDir, "matrix.mkv"), RootID: "root", RelativePath: "The Matrix/matrix.mkv", ItemType: models.ItemMovie, TMDBID: "603"}
	fresh := func() {
		media = t.TempDir()
		movieDir = filepath.Join(media, "The Matrix")
		task.Path = filepath.Join(movieDir, "matrix.mkv")
	}

	t.Run("alongside", func(t *testing.T) {
		runs := 0
		f := fakeFetcher(t, entry, &runs)
		lib := models.Library{ID: "lib", ImageStorage: models.StorageAlongside}
		if ok, err := f.Fetch(context.Background(), lib, task, media); !ok || err != nil {
			t.Fatalf("Fetch = %v, %v", ok, err)
		}
		if got := f.Locate(lib, task, media); got != filepath.Join(movieDir, FileName) {
			t.Fatalf("theme at %q", got)
		}
		if ok, _ := f.Fetch(context.Background(), lib, task, media); ok || runs != 1 {
			t.Fatalf("existing theme must not be downloaded again (runs=%d)", runs)
		}
	})

	t.Run("cache", func(t *testing.T) {
		fresh()
		runs := 0
		f := fakeFetcher(t, entry, &runs)
		lib := models.Library{ID: "lib", ImageStorage: models.StorageCache}
		if ok, err := f.Fetch(context.Background(), lib, task, media); !ok || err != nil {
			t.Fatalf("Fetch = %v, %v", ok, err)
		}
		got := f.Locate(lib, task, media)
		key, _ := mediaid.Key(task.RootID, task.RelativePath)
		if got != filepath.Join(f.CacheDir, "theme", key, FileName) {
			t.Fatalf("theme at %q", got)
		}
	})

	t.Run("no entry is remembered", func(t *testing.T) {
		runs := 0
		f := fakeFetcher(t, entry, &runs)
		lib := models.Library{ID: "lib", ImageStorage: models.StorageCache}
		other := Task{LibraryID: "lib", Path: filepath.Join(media, "Other", "o.mkv"), RootID: "root", RelativePath: "Other/o.mkv", ItemType: models.ItemMovie, TMDBID: "1"}
		if ok, err := f.Fetch(context.Background(), lib, other, media); ok || err != nil {
			t.Fatalf("Fetch = %v, %v", ok, err)
		}
		f.DBURL = "http://127.0.0.1:1" // would fail if it were asked again
		if ok, err := f.Fetch(context.Background(), lib, other, media); ok || err != nil {
			t.Fatalf("second Fetch = %v, %v", ok, err)
		}
		if runs != 0 {
			t.Fatalf("yt-dlp ran %d times", runs)
		}
	})

	t.Run("movie in the library root stays out of the media folder", func(t *testing.T) {
		runs := 0
		f := fakeFetcher(t, entry, &runs)
		lib := models.Library{ID: "lib", ImageStorage: models.StorageAlongside}
		flat := Task{LibraryID: "lib", Path: filepath.Join(media, "flat.mkv"), RootID: "root", RelativePath: "flat.mkv", ItemType: models.ItemMovie, TMDBID: "603"}
		if ok, err := f.Fetch(context.Background(), lib, flat, media); !ok || err != nil {
			t.Fatalf("Fetch = %v, %v", ok, err)
		}
		if _, err := os.Stat(filepath.Join(media, FileName)); err == nil {
			t.Fatal("theme must not be written into the library root")
		}
	})
}
