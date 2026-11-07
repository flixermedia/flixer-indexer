package tasklog

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestTaskLoggerCorrelatesNormalAndSuppressedRecords(t *testing.T) {
	var normal, fallback bytes.Buffer
	base := slog.New(slog.NewJSONHandler(&normal, &slog.HandlerOptions{Level: slog.LevelWarn}))
	store := NewWithFallback(slog.NewJSONHandler(&fallback, nil))
	ctx := WithTask(context.Background(), "scan", "task-1")
	logger := Logger(ctx, store, base).With("library", "movies")
	logger.Info("started")
	logger.Warn("failed")

	if normal.Len() == 0 || fallback.Len() == 0 {
		t.Fatalf("task output missing from normal or fallback logger: normal=%q fallback=%q", normal.String(), fallback.String())
	}
	if bytes.Contains(normal.Bytes(), []byte("started")) || !bytes.Contains(normal.Bytes(), []byte("failed")) {
		t.Fatalf("normal logger did not preserve its level: %s", normal.String())
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace([]byte(fallback.String())), &event); err != nil {
		t.Fatal(err)
	}
	if event["msg"] != "started" || event["task_id"] != "task-1" || event["task_queue"] != "scan" ||
		event["library"] != "movies" || event["trace_id"] != taskTraceID("scan", "task-1") {
		t.Fatalf("task correlation missing: %+v", event)
	}
	if InTask(context.Background()) || !InTask(ctx) {
		t.Fatal("task context detection failed")
	}
}

func TestLoggerReturnsBaseOutsideTasksOrWhenDisabled(t *testing.T) {
	base := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	if got := Logger(context.Background(), NewWithFallback(nil), base); got != base {
		t.Fatal("non-task context should return the base logger")
	}
	if got := Logger(WithTask(context.Background(), "scan", "task-1"), nil, base); got != base {
		t.Fatal("nil task logger should return the base logger")
	}
}
