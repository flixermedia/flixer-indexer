package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/theintrodb"
)

func TestIntroProviderLimitsSharedAcrossPipelines(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	first := New(&fakeAPI{}, nil, nil, nil, nil, log)
	second := New(&fakeAPI{}, nil, nil, nil, nil, log)
	first.TaskLocks, second.TaskLocks = client, client
	ctx := context.Background()
	if delay, err := first.introPermit(ctx, "theintrodb"); err != nil || delay != 0 {
		t.Fatalf("first permit: %s %v", delay, err)
	}
	if delay, err := second.introPermit(ctx, "theintrodb"); err != nil || delay <= 0 {
		t.Fatalf("second permit: %s %v", delay, err)
	}
	if _, err := first.introCooldown(ctx, "theintrodb", &theintrodb.RateLimitError{RetryAfter: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	if delay, err := second.introPermit(ctx, "theintrodb"); err != nil || delay < time.Minute {
		t.Fatalf("shared cooldown: %s %v", delay, err)
	}
	if delay, err := second.introPermit(ctx, "introdb"); err != nil || delay != 0 {
		t.Fatalf("fallback should remain available: %s %v", delay, err)
	}
	if _, err := second.introCooldown(ctx, "theintrodb", &theintrodb.RateLimitError{RetryAfter: 3 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	server.FastForward(2 * time.Minute)
	if delay, err := first.introPermit(ctx, "theintrodb"); err != nil || delay < 59*time.Second {
		t.Fatalf("extended cooldown: %s %v", delay, err)
	}
	server.FastForward(time.Minute)
	if delay, err := second.introPermit(ctx, "theintrodb"); err != nil || delay != 0 {
		t.Fatalf("expired cooldown: %s %v", delay, err)
	}
}

func TestIntroMissCacheDoesNotCacheRateLimits(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	p := New(&fakeAPI{}, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.TaskLocks = client
	primary := &fakeIntroDB{err: theintrodb.ErrRateLimited}
	p.IntroDB = primary
	candidate := models.IntroSyncCandidate{ID: "movie-id", Type: models.ItemMovie, ProviderIDs: map[string]string{"imdb": "tt0111161"}}
	payload, _ := json.Marshal(candidate)
	task := asynq.NewTask(TypeIntroMovie, payload)
	if err := p.handleIntroMovie(context.Background(), task); !errors.Is(err, errIntroDeferred) {
		t.Fatalf("expected deferred rate limit: %v", err)
	}
	key, _ := introMissKey(candidate, true, false)
	if server.Exists(key) {
		t.Fatal("rate limit incorrectly cached as a miss")
	}
	server.FastForward(time.Hour + time.Second)
	primary.err = nil
	if err := p.handleIntroMovie(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if !server.Exists(key) {
		t.Fatal("successful miss not cached")
	}
	if err := p.handleIntroMovie(context.Background(), task); err != nil || len(primary.candidates) != 2 {
		t.Fatalf("cached miss was looked up again: calls=%d err=%v", len(primary.candidates), err)
	}
	otherKey, _ := introMissKey(candidate, true, true)
	if key == otherKey || server.Exists(otherKey) {
		t.Fatal("provider configuration must have a separate cache entry")
	}
	server.FastForward(introMissTTL)
	if server.Exists(key) {
		t.Fatal("miss cache did not expire")
	}
}
