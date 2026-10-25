package tasklog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	return New(rdb)
}

func TestLoggerRecordsOnlyInsideATask(t *testing.T) {
	store := newStore(t)
	var out bytes.Buffer
	base := slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}))

	base2 := Logger(context.Background(), store, base)
	if base2 != base {
		t.Fatal("outside a task the base logger must be returned")
	}

	ctx := WithTask(context.Background(), "probe", "t1")
	log := Logger(ctx, store, base).With("library", "L1")
	log.Info("scan started", "name", "Movies")
	log.Warn("probe failed", "error", "boom")
	log.Debug("hidden")

	entries, total, err := store.Read(context.Background(), "probe", "t1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(entries) != 2 {
		t.Fatalf("want 2 entries, got total=%d entries=%v", total, entries)
	}
	if entries[0].Level != "INFO" || !strings.Contains(entries[0].Message, "scan started library=L1 name=Movies") {
		t.Errorf("unexpected first entry %+v", entries[0])
	}
	if entries[1].Level != "WARN" || !strings.Contains(entries[1].Message, "error=boom") {
		t.Errorf("unexpected second entry %+v", entries[1])
	}
	if strings.Contains(out.String(), "scan started") || !strings.Contains(out.String(), "probe failed") {
		t.Errorf("base logger must still honour its own level, got %q", out.String())
	}

	rest, total, _ := store.Read(context.Background(), "probe", "t1", 1)
	if total != 2 || len(rest) != 1 || rest[0].Level != "WARN" {
		t.Errorf("offset read returned %v (total %d)", rest, total)
	}
	none, _, _ := store.Read(context.Background(), "probe", "missing", 0)
	if len(none) != 0 {
		t.Errorf("unknown task must have no entries")
	}
}

func TestAppendStopsAtTheCap(t *testing.T) {
	store := newStore(t)
	for i := 0; i < MaxEntries+25; i++ {
		store.Append(context.Background(), "scan", "t", Entry{Message: "x"})
	}
	_, total, _ := store.Read(context.Background(), "scan", "t", 0)
	if total != MaxEntries {
		t.Fatalf("total = %d, want %d", total, MaxEntries)
	}
}
