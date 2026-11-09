package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"

	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/httpx"
)

func TestRateLimitedTasksAreDeferredAndProviderWarningIsThrottled(t *testing.T) {
	var logs bytes.Buffer
	p := &Pipeline{Log: slog.New(slog.NewJSONHandler(&logs, nil))}
	task := asynq.NewTask(TypeMetadata, nil)
	handler := p.withTaskLog(config.QueueMetadata, func(context.Context, *asynq.Task) error {
		return &httpx.RateLimitError{Provider: "tmdb", RetryAfter: 30 * time.Minute}
	})

	for i := 0; i < 2; i++ {
		err := handler(context.Background(), task)
		if !errors.Is(err, errProviderDeferred) {
			t.Fatalf("handler error = %v, want deferred provider rate limit", err)
		}
		if got := retryDelay(0, err, task); got != 30*time.Minute {
			t.Fatalf("retry delay = %s, want 30m", got)
		}
	}

	if got := strings.Count(logs.String(), "metadata provider rate limited"); got != 1 {
		t.Fatalf("provider warning count = %d, want 1; logs: %s", got, logs.String())
	}
	if strings.Contains(logs.String(), "attempt failed") {
		t.Fatalf("expected rate limits not to be logged as failed attempts: %s", logs.String())
	}
	p.OnError(context.Background(), task, errProviderDeferred)
	if strings.Contains(logs.String(), `"msg":"task failed"`) {
		t.Fatalf("provider deferral should not be logged as a failed task: %s", logs.String())
	}
}
