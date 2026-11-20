package lyrics

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	f.UnisonBaseURL = ""

	text, err := f.Fetch(context.Background(), Query{Artist: "The Fray", Title: "All at Once (Album Version)", Album: "The Fray", Duration: 240.4})
	if err != nil || text != "[00:01.00]right" {
		t.Fatalf("Fetch = %q, %v", text, err)
	}
	if _, err := f.Fetch(context.Background(), Query{Artist: "The Fray", Title: "All at Once", Duration: 100}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a different-length recording must not match, got %v", err)
	}
}

func TestFetcherSendsLRCLIBAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Lrclib-App-Id") != "app-id" || r.Header.Get("Lrclib-Api-Key") != "secret-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]Hello"}`))
	}))
	defer srv.Close()

	f := NewFetcher("test")
	f.BaseURL = srv.URL
	f.UnisonBaseURL = ""
	f.SetCredentials("app-id", "secret-key")
	text, err := f.Fetch(context.Background(), Query{Artist: "A", Title: "Song"})
	if err != nil || text != "[00:01.00]Hello" {
		t.Fatalf("Fetch = %q, %v", text, err)
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

func TestFetchUsesUnisonWhenLRCLIBHasNoLyrics(t *testing.T) {
	primary := httptest.NewServer(http.NotFoundHandler())
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lyrics" || r.URL.Query().Get("song") != "Song" || r.URL.Query().Get("artist") != "Artist" {
			t.Errorf("unexpected Unison lookup: %s", r.URL.String())
		}
		if r.Header.Get("Lrclib-Api-Key") != "" || r.Header.Get("Lrclib-App-Id") != "" {
			t.Error("LRCLIB credentials must not be sent to Unison")
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"videoId":"Ugr4KKK_SqY","format":"lrc","syncType":"linesync","lyrics":"[00:01.00]Fallback lyrics","submitter":{"displayName":"Curator","keyId":"curator-key"}}}`))
	}))
	defer fallback.Close()
	f := NewFetcher("test")
	f.BaseURL = primary.URL
	f.UnisonBaseURL = fallback.URL
	f.SetCredentials("private-app", "private-key")
	text, err := f.Fetch(context.Background(), Query{Artist: "Artist", Title: "Song"})
	if err != nil || !strings.Contains(text, "[00:01.00]Fallback lyrics") {
		t.Fatalf("Fetch = %q, %v", text, err)
	}
}

func TestFetchUsesUnisonImmediatelyOnRateLimit(t *testing.T) {
	primaryCalls := 0
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls++
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer primary.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"videoId":"Ugr4KKK_SqY","format":"lrc","syncType":"linesync","lyrics":"[00:01.00]Fallback","submitter":{"displayName":"Curator","keyId":"key"}}}`))
	}))
	defer fallback.Close()
	f := NewFetcher("test")
	f.BaseURL, f.UnisonBaseURL = primary.URL, fallback.URL
	for range 2 {
		if text, err := f.Fetch(context.Background(), Query{Artist: "Artist", Title: "Song"}); err != nil || !strings.Contains(text, "Fallback") {
			t.Fatalf("Fetch = %q, %v", text, err)
		}
	}
	if primaryCalls != 1 {
		t.Fatalf("LRCLIB called %d times during cooldown", primaryCalls)
	}
}

func TestRateLimitDelayHonorsLongRetryAfter(t *testing.T) {
	if delay := rateLimitDelay("120"); delay != 2*time.Minute {
		t.Fatalf("Retry-After was shortened to %s", delay)
	}
	if delay := rateLimitDelay(time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)); delay < time.Minute || delay > 2*time.Minute {
		t.Fatalf("HTTP-date Retry-After = %s", delay)
	}
}

func TestTTMLToLRC(t *testing.T) {
	source := `<tt xmlns="http://www.w3.org/ns/ttml"><body><div><p begin="00:10.000" end="00:12.500"><span begin="10s" end="11s">Hello </span><span begin="11000ms" end="12.5s">world &amp; friends</span></p><p begin="00:20.000" end="00:23.000">Next line</p></div></body></tt>`
	text, err := ttmlToLRC(source)
	if err != nil {
		t.Fatal(err)
	}
	want := "[00:10.000]<00:10.000>Hello <00:11.000>world & friends\n[00:12.500]\n[00:20.000]Next line\n[00:23.000]\n"
	if text != want {
		t.Fatalf("TTML = %q, want %q", text, want)
	}
	if _, err := ttmlToLRC(`<tt><p begin="invalid">Text</p></tt>`); err == nil {
		t.Fatal("invalid timestamp accepted")
	}
	if _, err := ttmlToLRC(`<tt><p begin="10s">Broken`); err == nil {
		t.Fatal("invalid XML accepted")
	}
}

func TestFetchHonorsProviderSettings(t *testing.T) {
	for _, test := range []struct {
		name              string
		primary, fallback bool
	}{
		{"primary only", true, false}, {"primary preferred", true, true}, {"fallback only", false, true}, {"both disabled", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			primaryCalls, fallbackCalls := 0, 0
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls++
				_, _ = w.Write([]byte(`{"syncedLyrics":"[00:01.00]Primary"}`))
			}))
			defer primary.Close()
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackCalls++
				_, _ = w.Write([]byte(`{"success":true,"data":{"videoId":"Ugr4KKK_SqY","format":"lrc","syncType":"linesync","lyrics":"[00:01.00]Fallback","submitter":{"displayName":"Curator","keyId":"key"}}}`))
			}))
			defer fallback.Close()
			f := NewFetcher("test")
			f.BaseURL, f.UnisonBaseURL = primary.URL, fallback.URL
			f.SetProviders(test.primary, test.fallback)
			_, err := f.Fetch(context.Background(), Query{Artist: "Artist", Title: "Song"})
			if !test.primary && primaryCalls != 0 {
				t.Fatal("disabled LRCLIB was called")
			}
			if !test.fallback && fallbackCalls != 0 {
				t.Fatal("disabled Unison was called")
			}
			if test.primary && fallbackCalls != 0 {
				t.Fatal("Unison was called after LRCLIB succeeded")
			}
			if test.primary || test.fallback {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrNotFound) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestUnisonFailuresRemainRetryableWhenLRCLIBIsRateLimited(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer primary.Close()
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer fallback.Close()
			f := NewFetcher("test")
			f.BaseURL, f.UnisonBaseURL = primary.URL, fallback.URL
			_, err := f.Fetch(context.Background(), Query{Artist: "Artist", Title: "Song"})
			if err == nil || errors.Is(err, ErrNotFound) {
				t.Fatalf("rate limits and temporary failures must not become permanent misses: %v", err)
			}
		})
	}
}

func TestEnsureCachesUnisonTTMLWithAttribution(t *testing.T) {
	calls := 0
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("album") != "Album" || r.URL.Query().Get("duration") != "30.000" {
			t.Error("recording metadata was not sent to Unison")
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"videoId":"Ugr4KKK_SqY","format":"ttml","syncType":"richsync","lyrics":"<tt><body><p begin=\"10s\" end=\"12s\"><span begin=\"10s\">Hello </span><span begin=\"11s\">world</span></p></body></tt>","submitter":{"displayName":"Curator","keyId":"curator-key"}}}`))
	}))
	defer fallback.Close()
	f := NewFetcher("test")
	f.UnisonBaseURL = fallback.URL
	f.SetProviders(false, true)
	audio := filepath.Join(t.TempDir(), "Song.flac")
	query := Query{Artist: "Artist", Title: "Song", Album: "Album", Duration: 30}
	for range 2 {
		if found, err := Ensure(context.Background(), f, audio, "", "track", query); err != nil || !found {
			t.Fatalf("Ensure = %v, %v", found, err)
		}
	}
	if calls != 1 {
		t.Fatalf("cached lyrics fetched %d times", calls)
	}
	stored, err := os.ReadFile(Path(audio))
	if err != nil {
		t.Fatal(err)
	}
	text := string(stored)
	if !strings.Contains(text, "[00:10.000]<00:10.000>Hello <00:11.000>world") {
		t.Fatalf("word timing lost: %q", text)
	}
	metadata := strings.SplitN(text, "\n", 2)[0]
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(metadata, "[unison:"), "]"))
	if err != nil {
		t.Fatal(err)
	}
	var credit unisonAttribution
	if err := json.Unmarshal(decoded, &credit); err != nil {
		t.Fatal(err)
	}
	if credit.SourceURL != "https://unison.betterlyrics.org/song/Ugr4KKK_SqY" || credit.SubmitterName != "Curator" || credit.SubmitterURL != "https://unison.betterlyrics.org/curator/curator-key" {
		t.Fatalf("attribution lost: %+v", credit)
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
	f.UnisonBaseURL = ""
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
	f.UnisonBaseURL = ""
	text, err := f.Fetch(context.Background(), Query{Artist: "A", Title: "Song"})
	if err != nil || text == "" || calls != 2 {
		t.Fatalf("Fetch = %q, %v after %d calls", text, err, calls)
	}
}
