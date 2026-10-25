// Package tasklog keeps the log output of individual queue tasks in Redis so the admin
// dashboard can show it while a task runs and after it has failed.
package tasklog

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	// MaxEntries caps one task's log; later lines are dropped so offsets stay stable.
	MaxEntries = 1000
	// TTL is how long a task's log outlives its last line.
	TTL = 7 * 24 * time.Hour
)

// Entry is one log line.
type Entry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Store persists task logs.
type Store struct {
	rdb redis.UniversalClient
}

func New(rdb redis.UniversalClient) *Store { return &Store{rdb: rdb} }

func key(queue, id string) string { return "flixer:tasklog:" + queue + ":" + id }

// Append adds a line to a task's log. Failures are ignored: logging must never fail a task.
func (s *Store) Append(ctx context.Context, queue, id string, e Entry) {
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	k := key(queue, id)
	n, err := s.rdb.RPush(ctx, k, data).Result()
	if err != nil {
		return
	}
	if n > MaxEntries {
		_ = s.rdb.RPop(ctx, k).Err()
		return
	}
	_ = s.rdb.Expire(ctx, k, TTL).Err()
}

// Read returns the lines from offset on and the total number of stored lines.
func (s *Store) Read(ctx context.Context, queue, id string, offset int) ([]Entry, int, error) {
	if offset < 0 {
		offset = 0
	}
	k := key(queue, id)
	total, err := s.rdb.LLen(ctx, k).Result()
	if err != nil {
		return nil, 0, err
	}
	if int64(offset) >= total {
		return []Entry{}, int(total), nil
	}
	raw, err := s.rdb.LRange(ctx, k, int64(offset), int64(offset)+MaxEntries).Result()
	if err != nil {
		return nil, 0, err
	}
	entries := make([]Entry, 0, len(raw))
	for _, line := range raw {
		var e Entry
		if json.Unmarshal([]byte(line), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries, int(total), nil
}

// Handler copies every record at info level or above to a task's log, then passes it on.
type Handler struct {
	Base  slog.Handler
	Store *Store
	Queue string
	ID    string
	attrs []slog.Attr
	group string
}

func (h Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo || h.Base.Enabled(ctx, level)
}

func (h Handler) Handle(ctx context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	write := func(prefix string, a slog.Attr) {
		fmt.Fprintf(&b, " %s%s=%v", prefix, a.Key, a.Value.Resolve())
	}
	for _, a := range h.attrs {
		write("", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		write(h.group, a)
		return true
	})
	h.Store.Append(ctx, h.Queue, h.ID, Entry{Time: r.Time.UTC().Format(time.RFC3339Nano), Level: r.Level.String(), Message: b.String()})
	if h.Base.Enabled(ctx, r.Level) {
		return h.Base.Handle(ctx, r)
	}
	return nil
}

func (h Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.Base = h.Base.WithAttrs(attrs)
	h.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return h
}

func (h Handler) WithGroup(name string) slog.Handler {
	h.Base = h.Base.WithGroup(name)
	h.group += name + "."
	return h
}

type ctxKey struct{}

type taskRef struct{ queue, id string }

// WithTask marks ctx as running the given task.
func WithTask(ctx context.Context, queue, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, taskRef{queue, id})
}

// InTask reports whether ctx belongs to a task whose log is recorded.
func InTask(ctx context.Context) bool {
	_, ok := ctx.Value(ctxKey{}).(taskRef)
	return ok
}

// Logger returns base, or a logger that also records into the task's log when ctx belongs to a task.
func Logger(ctx context.Context, store *Store, base *slog.Logger) *slog.Logger {
	ref, ok := ctx.Value(ctxKey{}).(taskRef)
	if !ok || store == nil {
		return base
	}
	return slog.New(Handler{Base: base.Handler(), Store: store, Queue: ref.queue, ID: ref.id})
}
