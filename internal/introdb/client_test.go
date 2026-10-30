package introdb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

func TestGetMediaReadsMovieSegments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/segments" || r.URL.Query().Get("imdb_id") != "tt0111161" || r.URL.Query().Get("is_movie") != "true" {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"intro":{"start_ms":30000,"end_ms":90000},"outro":{"start_ms":6000000,"end_ms":6300000}}`))
	}))
	defer server.Close()

	segments, err := New(server.URL).GetMedia(context.Background(), models.IntroSyncCandidate{
		Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if segments["intro"] != (models.IntroTimeRange{StartMs: 30_000, EndMs: 90_000}) {
		t.Errorf("intro = %+v", segments["intro"])
	}
	if segments["credits"] != (models.IntroTimeRange{StartMs: 6_000_000, EndMs: 6_300_000}) {
		t.Errorf("credits = %+v", segments["credits"])
	}
}

func TestGetMediaReadsEpisodesAndSkipsUnsupportedIDs(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("season") != "2" || r.URL.Query().Get("episode") != "3" || r.URL.Query().Has("is_movie") {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"intro":{"start_ms":10000,"end_ms":50000},"outro":null}`))
	}))
	defer server.Close()

	season, episode := 2, 3
	segments, err := New(server.URL).GetMedia(context.Background(), models.IntroSyncCandidate{
		Type: models.ItemEpisode, ProviderIDs: map[string]string{"imdb": "tt1234567"}, Season: &season, Episode: &episode,
	})
	if err != nil || segments["intro"].EndMs != 50_000 || requests != 1 {
		t.Fatalf("segments=%+v requests=%d err=%v", segments, requests, err)
	}
	segments, err = New(server.URL).GetMedia(context.Background(), models.IntroSyncCandidate{
		Type: models.ItemEpisode, ProviderIDs: map[string]string{"tvdb": "71663"}, Season: &season, Episode: &episode,
	})
	if err != nil || len(segments) != 0 || requests != 1 {
		t.Fatalf("unsupported ID segments=%+v requests=%d err=%v", segments, requests, err)
	}
}

func TestGetMediaReturnsErrorForFailedRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := New(server.URL).GetMedia(context.Background(), models.IntroSyncCandidate{
		Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	})
	if err == nil {
		t.Fatal("expected failed request error")
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
