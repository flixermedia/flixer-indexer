// Package tasklog attaches task correlation to Indexer log records.
package tasklog

import (
	"context"
	"log/slog"
)

// Store holds the Logger handler used for task records.
type Store struct {
	fallback slog.Handler
}

func NewWithFallback(fallback slog.Handler) *Store {
	return &Store{fallback: fallback}
}

// Handler sends task records to the normal logger, or to the task handler when
// the normal logger suppresses the record's level.
type Handler struct {
	Base     slog.Handler
	fallback slog.Handler
}

func (h Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.Base.Enabled(ctx, level) || (level >= slog.LevelInfo && h.fallback != nil && h.fallback.Enabled(ctx, level))
}

func (h Handler) Handle(ctx context.Context, record slog.Record) error {
	if h.Base.Enabled(ctx, record.Level) {
		return h.Base.Handle(ctx, record)
	}
	if record.Level >= slog.LevelInfo && h.fallback != nil && h.fallback.Enabled(ctx, record.Level) {
		return h.fallback.Handle(ctx, record)
	}
	return nil
}

func (h Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.Base = h.Base.WithAttrs(attrs)
	if h.fallback != nil {
		h.fallback = h.fallback.WithAttrs(attrs)
	}
	return h
}

func (h Handler) WithGroup(name string) slog.Handler {
	h.Base = h.Base.WithGroup(name)
	if h.fallback != nil {
		h.fallback = h.fallback.WithGroup(name)
	}
	return h
}

type ctxKey struct{}

type taskRef struct{ queue, id, trace string }

// WithTask marks ctx as running the given task.
func WithTask(ctx context.Context, queue, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, taskRef{queue, id, taskTraceID(queue, id)})
}

// InTask reports whether ctx belongs to a task whose logs are correlated.
func InTask(ctx context.Context) bool {
	_, ok := ctx.Value(ctxKey{}).(taskRef)
	return ok
}

// Logger returns base, or a logger that adds task correlation to its records.
func Logger(ctx context.Context, store *Store, base *slog.Logger) *slog.Logger {
	ref, ok := ctx.Value(ctxKey{}).(taskRef)
	if !ok || store == nil {
		return base
	}
	attrs := []slog.Attr{
		slog.String("task_id", ref.id),
		slog.String("task_queue", ref.queue),
		slog.String("trace_id", ref.trace),
	}
	var fallback slog.Handler
	if store.fallback != nil {
		fallback = store.fallback.WithAttrs(attrs)
	}
	return slog.New(Handler{
		Base:     base.With("task_id", ref.id, "task_queue", ref.queue, "trace_id", ref.trace).Handler(),
		fallback: fallback,
	})
}
