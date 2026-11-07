package tasklog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/flixermedia/flixer-logger/pkg/exporter"
)

type taskSink struct {
	events []exporter.Event
}

func (s *taskSink) Enqueue(event exporter.Event) error {
	s.events = append(s.events, event)
	return nil
}

func TestTaskExporterPreservesCorrelationAtWarnLogLevel(t *testing.T) {
	sink := &taskSink{}
	var output bytes.Buffer
	options := exporter.HandlerOptions{App: "flixer-indexer", InstanceID: "one", OnError: func(err error) { t.Error(err) }}
	base, err := exporter.NewSlogHandler(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelWarn}), sink, options)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := exporter.NewSlogHandler(slog.NewJSONHandler(&bytes.Buffer{}, nil), sink, options)
	if err != nil {
		t.Fatal(err)
	}
	store := NewWithFallback(fallback)
	logger := Logger(WithTask(context.Background(), "scan", "task-1"), store, slog.New(base))
	logger.Info("started", "library", "movies")
	logger.Warn("failed", "error", "oops")
	if len(sink.events) != 2 {
		t.Fatalf("unexpected exported records: %+v", sink.events)
	}
	for _, event := range sink.events {
		if event.TaskID != "task-1" || event.Fields["task_queue"] != "scan" {
			t.Fatalf("missing task correlation: %+v", event)
		}
		if event.TraceID != strings.Split(Traceparent(WithTask(context.Background(), "scan", "task-1")), "-")[1] {
			t.Fatalf("missing task trace correlation: %+v", event)
		}
	}
	if sink.events[0].Message != "started" || sink.events[1].Message != "failed" ||
		strings.Contains(output.String(), "started") || !strings.Contains(output.String(), "failed") {
		t.Fatalf("incorrect export or base output: %+v %s", sink.events, output.String())
	}
}
