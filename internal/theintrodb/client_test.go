package theintrodb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

func TestGetMediaPrefersTVDBAndNormalizesV4Media(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4/media" || r.URL.Query().Get("tvdb_id") != "169" || r.URL.Query().Get("tmdb_id") != "" {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.URL.Query().Get("duration_ms") != "7200000" {
			t.Errorf("duration_ms = %q", r.URL.Query().Get("duration_ms"))
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("public lookup unexpectedly sent credentials")
		}
		_, _ = w.Write([]byte(`{"kind":"media","type":"movie","intro":[{"startMs":30000,"endMs":90000}],"credits":[{"start_ms":null,"end_ms":null}]}`))
	}))
	defer server.Close()

	duration := int64(7_200_000)
	client := New(server.URL + "/v4")
	segments, err := client.GetMedia(context.Background(), models.IntroSyncCandidate{
		Type: models.ItemMovie, ProviderIDs: map[string]string{"tvdb": "169", "tmdb": "42"}, DurationMs: duration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if segments["intro"] != (models.IntroTimeRange{StartMs: 30_000, EndMs: 90_000}) {
		t.Errorf("intro = %+v", segments["intro"])
	}
	if segments["credits"] != (models.IntroTimeRange{StartMs: 0, EndMs: duration}) {
		t.Errorf("credits = %+v", segments["credits"])
	}
}

func TestGetMediaUsesTVDBSeriesIDAndEpisodeNumbers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tvdb_id") != "71663" || r.URL.Query().Get("season") != "0" || r.URL.Query().Get("episode") != "1" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"type":"tv","intro":[{"start_ms":0,"end_ms":20000}],"credits":[]}`))
	}))
	defer server.Close()
	season, episode := 0, 1
	segments, err := New(server.URL).GetMedia(context.Background(), models.IntroSyncCandidate{
		Type: models.ItemEpisode, ProviderIDs: map[string]string{"tvdb": "71663"}, Season: &season, Episode: &episode,
	})
	if err != nil || segments["intro"].EndMs != 20_000 {
		t.Fatalf("segments=%+v err=%v", segments, err)
	}
}

func TestGetMediaSendsOptionalAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("apiKey"); got != "intro-secret" {
			t.Errorf("apiKey header = %q", got)
		}
		_, _ = w.Write([]byte(`{"type":"movie","intro":[],"credits":[]}`))
	}))
	defer server.Close()

	client := New(server.URL)
	client.SetAPIKey(" intro-secret ")
	_, err := client.GetMedia(context.Background(), models.IntroSyncCandidate{
		Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGetMediaHandlesMissingIDsAndUnusableResponse(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"kind":"versions","versions":[]}`))
	}))
	defer server.Close()

	result, err := New(server.URL).GetMedia(context.Background(), models.IntroSyncCandidate{Type: models.ItemMovie})
	if err != nil || len(result) != 0 || requests != 0 {
		t.Fatalf("missing ID result=%v requests=%d err=%v", result, requests, err)
	}
	result, err = New(server.URL).GetMedia(context.Background(), models.IntroSyncCandidate{
		Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	})
	if err != nil || len(result) != 0 || requests != 1 {
		t.Fatalf("non-media result=%v requests=%d err=%v", result, requests, err)
	}
}

func TestGetMediaReportsRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	_, err := New(server.URL).GetMedia(context.Background(), models.IntroSyncCandidate{
		Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want rate-limit error", err)
	}
}
