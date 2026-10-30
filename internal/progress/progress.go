// Package progress tracks per-library scan progress in Redis so that API and
// worker processes can report on the same scans.
package progress

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	StateScanning = "scanning"
	StateIndexing = "indexing"
	StateComplete = "complete"
	StateFailed   = "failed"

	ttl = 7 * 24 * time.Hour
)

// Scan is a snapshot of one library's most recent scan.
type Scan struct {
	LibraryID  string `json:"libraryId"`
	State      string `json:"state"`
	Discovered int64  `json:"discovered"`
	Done       int64  `json:"done"`
	Failed     int64  `json:"failed"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
	Error      string `json:"error,omitempty"`
}

type Tracker struct {
	rdb redis.UniversalClient
}

func New(rdb redis.UniversalClient) *Tracker { return &Tracker{rdb: rdb} }

func key(libraryID string) string { return "indexer:scan:" + libraryID }

// Start resets the counters for a new scan.
func (t *Tracker) Start(ctx context.Context, libraryID string) error {
	pipe := t.rdb.TxPipeline()
	pipe.Del(ctx, key(libraryID))
	pipe.HSet(ctx, key(libraryID), "state", StateScanning, "startedAt", time.Now().UTC().Format(time.RFC3339Nano),
		"discovered", 0, "done", 0, "failed", 0)
	pipe.Expire(ctx, key(libraryID), ttl)
	_, err := pipe.Exec(ctx)
	return err
}

func (t *Tracker) Discovered(ctx context.Context, libraryID string) error {
	return t.rdb.HIncrBy(ctx, key(libraryID), "discovered", 1).Err()
}

func (t *Tracker) Done(ctx context.Context, libraryID string) error {
	return t.finish(ctx, libraryID, "done")
}

func (t *Tracker) Failed(ctx context.Context, libraryID string) error {
	return t.finish(ctx, libraryID, "failed")
}

func (t *Tracker) finish(ctx context.Context, libraryID, field string) error {
	if err := t.rdb.HIncrBy(ctx, key(libraryID), field, 1).Err(); err != nil {
		return err
	}
	return t.settle(ctx, libraryID)
}

// WalkDone marks that the filesystem walk finished and no more items will be
// discovered.
func (t *Tracker) WalkDone(ctx context.Context, libraryID string) error {
	if err := t.rdb.HSet(ctx, key(libraryID), "state", StateIndexing, "walked", 1).Err(); err != nil {
		return err
	}
	return t.settle(ctx, libraryID)
}

// WalkFailed ends a scan whose walk could not complete.
func (t *Tracker) WalkFailed(ctx context.Context, libraryID string, cause error) error {
	return t.rdb.HSet(ctx, key(libraryID), "state", StateFailed, "error", cause.Error(),
		"finishedAt", time.Now().UTC().Format(time.RFC3339)).Err()
}

func (t *Tracker) settle(ctx context.Context, libraryID string) error {
	s, err := t.Get(ctx, libraryID)
	if err != nil || s == nil {
		return err
	}
	vals, err := t.rdb.HMGet(ctx, key(libraryID), "walked").Result()
	if err != nil {
		return err
	}
	walked := len(vals) > 0 && vals[0] != nil
	if walked && s.State == StateIndexing && s.Done+s.Failed >= s.Discovered {
		return t.rdb.HSet(ctx, key(libraryID), "state", StateComplete, "finishedAt", time.Now().UTC().Format(time.RFC3339)).Err()
	}
	return nil
}

// ClaimPrune reports whether the caller should prune the library now: the scan
// finished cleanly and no one has pruned for it yet. It returns the scan's start
// time, so only items older than the scan are removed.
func (t *Tracker) ClaimPrune(ctx context.Context, libraryID string) (string, bool, error) {
	s, err := t.Get(ctx, libraryID)
	if err != nil || s == nil || s.State != StateComplete || s.Failed > 0 || s.StartedAt == "" {
		return "", false, err
	}
	claimed, err := t.rdb.HSetNX(ctx, key(libraryID), "pruned", 1).Result()
	if err != nil || !claimed {
		return "", false, err
	}
	return s.StartedAt, true, nil
}

// Get returns the latest scan for a library, or nil if there is none.
func (t *Tracker) Get(ctx context.Context, libraryID string) (*Scan, error) {
	m, err := t.rdb.HGetAll(ctx, key(libraryID)).Result()
	if err != nil || len(m) == 0 {
		return nil, err
	}
	num := func(k string) int64 { n, _ := strconv.ParseInt(m[k], 10, 64); return n }
	return &Scan{
		LibraryID: libraryID, State: m["state"], Discovered: num("discovered"), Done: num("done"), Failed: num("failed"),
		StartedAt: m["startedAt"], FinishedAt: m["finishedAt"], Error: m["error"],
	}, nil
}

// All lists every tracked scan.
func (t *Tracker) All(ctx context.Context) ([]Scan, error) {
	var out []Scan
	iter := t.rdb.Scan(ctx, 0, key("*"), 100).Iterator()
	prefix := len(key(""))
	for iter.Next(ctx) {
		if s, err := t.Get(ctx, iter.Val()[prefix:]); err == nil && s != nil {
			out = append(out, *s)
		}
	}
	return out, iter.Err()
}
