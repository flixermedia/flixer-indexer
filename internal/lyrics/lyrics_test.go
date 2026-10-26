package lyrics

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchFallsBackToSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			_, _ = w.Write([]byte(`[
				{"trackName":"All at Once","artistName":"The Fray","albumName":"Other","duration":300,"syncedLyrics":"[00:01.00]far"},
				{"trackName":"All at Once","artistName":"Somebody Else","albumName":"X","duration":240,"syncedLyrics":"[00:01.00]wrong artist"},
				{"trackName":"All at Once","artistName":"The Fray","albumName":"The Fray","duration":241,"syncedLyrics":"[00:01.00]right"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := NewFetcher("test")
	f.BaseURL = srv.URL

	text, err := f.Fetch(context.Background(), Query{Artist: "The Fray", Title: "All at Once (Album Version)", Album: "The Fray", Duration: 240.4})
	if err != nil || text != "[00:01.00]right" {
		t.Fatalf("Fetch = %q, %v", text, err)
	}
	if _, err := f.Fetch(context.Background(), Query{Artist: "The Fray", Title: "All at Once", Duration: 100}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a different-length recording must not match, got %v", err)
	}
}

func TestCleanTitle(t *testing.T) {
	for in, want := range map[string]string{
		"All at Once (Album Version)": "all at once",
		"Song - Remastered 2011":      "song",
		"Song feat. Someone":          "song",
		"03 - Song":                   "song",
	} {
		if got := cleanTitle(in); got != want {
			t.Errorf("cleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnsureWritesBesideAudioThenFallsBackToStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("track_name") != "Song" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]Hello"}`))
	}))
	defer srv.Close()
	f := NewFetcher("test")
	f.BaseURL = srv.URL
	q := Query{Artist: "A", Title: "Song"}

	store := filepath.Join(t.TempDir(), "lyrics")
	audio := filepath.Join(t.TempDir(), "Song.wav")
	if ok, err := Ensure(context.Background(), f, audio, store, "abc-123", q); err != nil || !ok {
		t.Fatalf("Ensure = %v, %v", ok, err)
	}
	if data, _ := os.ReadFile(Path(audio)); string(data) != "[00:01.00]Hello" {
		t.Fatalf("expected a sidecar beside the audio, got %q", data)
	}
	if _, err := os.Stat(filepath.Join(store, "abc-123.lrc")); err == nil {
		t.Fatal("the store should be unused when the folder is writable")
	}

	// A folder that cannot be written (here: its parent is a file) falls back to the store.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(blocker, "Song.wav")
	if ok, err := Ensure(context.Background(), f, locked, store, "abc-123", q); err != nil || !ok {
		t.Fatalf("Ensure = %v, %v", ok, err)
	}
	if data, _ := os.ReadFile(filepath.Join(store, "abc-123.lrc")); string(data) != "[00:01.00]Hello" {
		t.Fatalf("unexpected stored lyrics %q", data)
	}

	if ok, err := Ensure(context.Background(), f, filepath.Join(t.TempDir(), "Other.wav"), store, "other", Query{Artist: "A", Title: "Other"}); ok || err != nil {
		t.Fatalf("missing lyrics should be (false, nil), got %v, %v", ok, err)
	}
	if ok, _ := Ensure(context.Background(), nil, locked, store, "../escape", Query{}); ok {
		t.Fatal("unsafe ids must be refused")
	}
}

func TestGetRetriesAfterRateLimit(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]Hi"}`))
	}))
	defer srv.Close()
	f := NewFetcher("test")
	f.BaseURL = srv.URL
	text, err := f.Fetch(context.Background(), Query{Artist: "A", Title: "Song"})
	if err != nil || text == "" || calls != 2 {
		t.Fatalf("Fetch = %q, %v after %d calls", text, err, calls)
	}
}
