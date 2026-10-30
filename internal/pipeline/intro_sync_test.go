package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/introdb"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/theintrodb"
)

type fakeIntroDB struct {
	candidates []models.IntroSyncCandidate
	result     map[string]models.IntroTimeRange
	err        error
}

func (f *fakeIntroDB) GetMedia(_ context.Context, item models.IntroSyncCandidate) (map[string]models.IntroTimeRange, error) {
	f.candidates = append(f.candidates, item)
	return f.result, f.err
}

func TestIntroSyncWorkersUseSeparateQueuesAndPersistResults(t *testing.T) {
	api := &fakeAPI{}
	lookup := &fakeIntroDB{result: map[string]models.IntroTimeRange{"intro": {StartMs: 1000, EndMs: 4000}}}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	p := New(api, nil, nil, nil, nil, log)
	p.IntroDB = lookup
	season, episode := 1, 2
	tasks := []struct {
		taskType  string
		handler   func(context.Context, *asynq.Task) error
		candidate models.IntroSyncCandidate
	}{
		{TypeIntroMovie, p.handleIntroMovie, models.IntroSyncCandidate{
			ID: "movie-id", Type: models.ItemMovie, ProviderIDs: map[string]string{"tvdb": "169", "tmdb": "42"}, DurationMs: 120_000,
		}},
		{TypeIntroEpisode, p.handleIntroEpisode, models.IntroSyncCandidate{
			ID: "episode-id", Type: models.ItemEpisode, ProviderIDs: map[string]string{"tvdb": "71663"}, Season: &season, Episode: &episode,
		}},
	}
	for _, entry := range tasks {
		payload, err := json.Marshal(entry.candidate)
		if err != nil {
			t.Fatal(err)
		}
		if err := entry.handler(context.Background(), asynq.NewTask(entry.taskType, payload)); err != nil {
			t.Fatal(err)
		}
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(lookup.candidates) != 2 || lookup.candidates[0].ProviderIDs["tvdb"] != "169" || lookup.candidates[1].Season == nil || *lookup.candidates[1].Episode != 2 {
		t.Fatalf("lookup candidates = %+v", lookup.candidates)
	}
	if len(api.introUpdates) != 2 || api.introUpdates[0].ItemID != "movie-id" || api.introUpdates[1].ItemID != "episode-id" {
		t.Fatalf("updates = %+v", api.introUpdates)
	}
	if got := p.Handlers(); len(got[config.QueueIntroMovies]) != 1 || len(got[config.QueueIntroEpisodes]) != 1 {
		t.Fatalf("intro workers are not independently registered: %v", got)
	}
}

func TestIntroSyncClearsStaleTimingsWithoutIDsAndHonorsDisable(t *testing.T) {
	api := &fakeAPI{}
	lookup := &fakeIntroDB{result: map[string]models.IntroTimeRange{"intro": {StartMs: 1, EndMs: 2}}}
	p := New(api, nil, nil, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	p.IntroDB = lookup
	candidate := models.IntroSyncCandidate{ID: "movie-id", Type: models.ItemMovie}
	payload, _ := json.Marshal(candidate)
	if err := p.handleIntroMovie(context.Background(), asynq.NewTask(TypeIntroMovie, payload)); err != nil {
		t.Fatal(err)
	}
	p.SetTheIntroDBEnabled(false)
	if err := p.handleIntroMovie(context.Background(), asynq.NewTask(TypeIntroMovie, payload)); err != nil {
		t.Fatal(err)
	}
	if len(lookup.candidates) != 0 {
		t.Fatalf("missing ID triggered external request: %+v", lookup.candidates)
	}
	if len(api.introUpdates) != 1 || len(api.introUpdates[0].Segments) != 0 {
		t.Fatalf("updates = %+v", api.introUpdates)
	}
}

func TestIntroSyncSkipsProviderCallsWhenTimingsAlreadyExist(t *testing.T) {
	api := &fakeAPI{existingIntroSegments: models.ExistingIntroSegments{"intro": true}}
	primary := &fakeIntroDB{}
	fallback := &fakeIntroDB{}
	p := New(api, nil, nil, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	p.IntroDB = primary
	p.IntroDBFallback = fallback
	p.SetIntroDBEnabled(true)
	candidate := models.IntroSyncCandidate{
		ID: "movie-id", Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	}
	payload, _ := json.Marshal(candidate)
	if err := p.handleIntroMovie(context.Background(), asynq.NewTask(TypeIntroMovie, payload)); err != nil {
		t.Fatal(err)
	}
	if len(primary.candidates) != 0 || len(fallback.candidates) != 0 || len(api.introUpdates) != 0 {
		t.Fatalf("primary=%d fallback=%d updates=%d", len(primary.candidates), len(fallback.candidates), len(api.introUpdates))
	}
}

func TestIntroSyncUsesIntroDBToFillMissingTimings(t *testing.T) {
	api := &fakeAPI{}
	primary := &fakeIntroDB{result: map[string]models.IntroTimeRange{
		"intro": {StartMs: 1_000, EndMs: 4_000},
	}}
	fallback := &fakeIntroDB{result: map[string]models.IntroTimeRange{
		"intro":   {StartMs: 2_000, EndMs: 5_000},
		"credits": {StartMs: 90_000, EndMs: 100_000},
	}}
	p := New(api, nil, nil, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	p.IntroDB = primary
	p.IntroDBFallback = fallback
	p.SetIntroDBEnabled(true)
	candidate := models.IntroSyncCandidate{
		ID: "movie-id", Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	}
	payload, _ := json.Marshal(candidate)
	if err := p.handleIntroMovie(context.Background(), asynq.NewTask(TypeIntroMovie, payload)); err != nil {
		t.Fatal(err)
	}
	if len(primary.candidates) != 1 || len(fallback.candidates) != 1 {
		t.Fatalf("primary=%d fallback=%d", len(primary.candidates), len(fallback.candidates))
	}
	if len(api.introUpdates) != 1 {
		t.Fatalf("updates = %+v", api.introUpdates)
	}
	got := api.introUpdates[0].Segments
	if got["intro"] != primary.result["intro"] || got["credits"] != fallback.result["credits"] {
		t.Fatalf("segments = %+v", got)
	}
}

func TestIntroSyncFallsBackAfterPrimaryRequestFailure(t *testing.T) {
	api := &fakeAPI{}
	primary := &fakeIntroDB{err: errors.New("primary unavailable")}
	fallback := &fakeIntroDB{result: map[string]models.IntroTimeRange{
		"intro": {StartMs: 1_000, EndMs: 4_000},
	}}
	p := New(api, nil, nil, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	p.IntroDB = primary
	p.IntroDBFallback = fallback
	p.SetIntroDBEnabled(true)
	candidate := models.IntroSyncCandidate{
		ID: "movie-id", Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	}
	payload, _ := json.Marshal(candidate)
	if err := p.handleIntroMovie(context.Background(), asynq.NewTask(TypeIntroMovie, payload)); err != nil {
		t.Fatal(err)
	}
	if len(primary.candidates) != 1 || len(fallback.candidates) != 1 {
		t.Fatalf("primary=%d fallback=%d", len(primary.candidates), len(fallback.candidates))
	}
	if len(api.introUpdates) != 1 || api.introUpdates[0].Segments["intro"] != fallback.result["intro"] {
		t.Fatalf("updates = %+v", api.introUpdates)
	}
}

func TestIntroSyncFallsBackQuietlyWhenTheIntroDBRateLimits(t *testing.T) {
	api := &fakeAPI{}
	primary := &fakeIntroDB{err: theintrodb.ErrRateLimited}
	fallback := &fakeIntroDB{result: map[string]models.IntroTimeRange{
		"intro": {StartMs: 1_000, EndMs: 4_000},
	}}
	var logs bytes.Buffer
	p := New(api, nil, nil, nil, nil, slog.New(slog.NewTextHandler(&logs, nil)))
	p.IntroDB = primary
	p.IntroDBFallback = fallback
	p.SetIntroDBEnabled(true)
	candidate := models.IntroSyncCandidate{
		ID: "movie-id", Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	}
	payload, _ := json.Marshal(candidate)
	if err := p.handleIntroMovie(context.Background(), asynq.NewTask(TypeIntroMovie, payload)); err != nil {
		t.Fatal(err)
	}
	if len(primary.candidates) != 1 || len(fallback.candidates) != 1 {
		t.Fatalf("primary=%d fallback=%d", len(primary.candidates), len(fallback.candidates))
	}
	if len(api.introUpdates) != 1 || api.introUpdates[0].Segments["intro"] != fallback.result["intro"] {
		t.Fatalf("updates = %+v", api.introUpdates)
	}
	if strings.Contains(logs.String(), "TheIntroDB lookup failed") {
		t.Fatalf("rate limit was logged instead of silently falling back: %s", logs.String())
	}
}

func TestIntroSyncRateLimitRetriesAfterOneHourWhenFallbackCannotProvideTimings(t *testing.T) {
	tests := []struct {
		name     string
		fallback *fakeIntroDB
		enabled  bool
	}{
		{name: "timings not found", fallback: &fakeIntroDB{result: map[string]models.IntroTimeRange{}}, enabled: true},
		{name: "fallback unavailable", enabled: false},
		{name: "fallback failed", fallback: &fakeIntroDB{err: errors.New("fallback unavailable")}, enabled: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			primary := &fakeIntroDB{err: theintrodb.ErrRateLimited}
			p := New(&fakeAPI{}, nil, nil, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
			p.IntroDB = primary
			p.IntroDBFallback = test.fallback
			if test.enabled {
				p.SetIntroDBEnabled(true)
			}
			candidate := models.IntroSyncCandidate{
				ID: "movie-id", Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
			}
			payload, _ := json.Marshal(candidate)
			err := p.handleIntroMovie(context.Background(), asynq.NewTask(TypeIntroMovie, payload))
			if err == nil {
				t.Fatal("expected a delayed retry error")
			}
			if got := retryDelay(0, err, asynq.NewTask(TypeIntroMovie, payload)); got != time.Hour {
				t.Fatalf("retry delay = %s, want %s", got, time.Hour)
			}
		})
	}
}

func TestIntroSyncIntroDBRateLimitRetriesAfterOneHour(t *testing.T) {
	primary := &fakeIntroDB{result: map[string]models.IntroTimeRange{}}
	fallback := &fakeIntroDB{err: introdb.ErrRateLimited}
	p := New(&fakeAPI{}, nil, nil, nil, nil, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	p.IntroDB = primary
	p.IntroDBFallback = fallback
	p.SetIntroDBEnabled(true)
	candidate := models.IntroSyncCandidate{
		ID: "movie-id", Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"},
	}
	payload, _ := json.Marshal(candidate)
	err := p.handleIntroMovie(context.Background(), asynq.NewTask(TypeIntroMovie, payload))
	if err == nil {
		t.Fatal("expected a delayed retry error")
	}
	if got := retryDelay(0, err, asynq.NewTask(TypeIntroMovie, payload)); got != time.Hour {
		t.Fatalf("retry delay = %s, want %s", got, time.Hour)
	}
}
