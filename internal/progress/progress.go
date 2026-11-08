// Package progress tracks per-library scan progress in Redis so that API and
// worker processes can report on the same scans.
package progress

import (
	"context"
	"errors"
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
func (t *Tracker) Start(ctx context.Context, libraryID, scanID string, canPrune bool) error {
	const script = `
		if redis.call("HGET", KEYS[1], "pruning") == "1" then return 0 end
		redis.call("DEL", KEYS[1])
		redis.call("HSET", KEYS[1], "state", "scanning", "startedAt", ARGV[2],
		  "scanId", ARGV[1], "canPrune", ARGV[3], "discovered", 0, "done", 0, "failed", 0)
		redis.call("EXPIRE", KEYS[1], ARGV[4])
		return 1`
	allowed := "0"
	if canPrune {
		allowed = "1"
	}
	result, err := t.rdb.Eval(ctx, script, []string{key(libraryID)}, scanID,
		time.Now().UTC().Format(time.RFC3339Nano), allowed, int(ttl.Seconds())).Int()
	if err != nil {
		return err
	}
	if result == 0 {
		return errors.New("library cleanup is still running")
	}
	return nil
}

func (t *Tracker) Discovered(ctx context.Context, libraryID, scanID string) error {
	const script = `if redis.call("HGET", KEYS[1], "scanId") == ARGV[1] then return redis.call("HINCRBY", KEYS[1], "discovered", 1) end return 0`
	return t.rdb.Eval(ctx, script, []string{key(libraryID)}, scanID).Err()
}

func (t *Tracker) Done(ctx context.Context, libraryID, scanID, itemID string) error {
	return t.finish(ctx, libraryID, scanID, itemID, "done")
}

func (t *Tracker) Failed(ctx context.Context, libraryID, scanID, itemID string) error {
	return t.finish(ctx, libraryID, scanID, itemID, "failed")
}

const settleScript = `
	if redis.call("HGET", KEYS[1], "state") == "indexing" and
	   redis.call("HGET", KEYS[1], "walked") == "1" and
	   tonumber(redis.call("HGET", KEYS[1], "done") or "0") + tonumber(redis.call("HGET", KEYS[1], "failed") or "0") >= tonumber(redis.call("HGET", KEYS[1], "discovered") or "0") then
		redis.call("HSET", KEYS[1], "state", "complete", "finishedAt", ARGV[2])
	end`

func (t *Tracker) finish(ctx context.Context, libraryID, scanID, itemID, field string) error {
	if scanID == "" || itemID == "" {
		return nil
	}
	const script = `
		if redis.call("HGET", KEYS[1], "scanId") ~= ARGV[1] then return 0 end
		if redis.call("HSETNX", KEYS[1], "item:" .. ARGV[3], ARGV[4]) == 1 then
			redis.call("HINCRBY", KEYS[1], ARGV[4], 1)
		end
	` + settleScript + `
		return 1`
	return t.rdb.Eval(ctx, script, []string{key(libraryID)}, scanID,
		time.Now().UTC().Format(time.RFC3339), itemID, field).Err()
}

// WalkDone marks that the filesystem walk finished and no more items will be
// discovered.
func (t *Tracker) WalkDone(ctx context.Context, libraryID, scanID string) error {
	const script = `
		if redis.call("HGET", KEYS[1], "scanId") ~= ARGV[1] then return 0 end
		redis.call("HSET", KEYS[1], "state", "indexing", "walked", 1)
	` + settleScript + `
		return 1`
	return t.rdb.Eval(ctx, script, []string{key(libraryID)}, scanID, time.Now().UTC().Format(time.RFC3339)).Err()
}

// WalkFailed ends a scan whose walk could not complete.
func (t *Tracker) WalkFailed(ctx context.Context, libraryID, scanID string, cause error) error {
	const script = `if redis.call("HGET", KEYS[1], "scanId") == ARGV[1] then
		return redis.call("HSET", KEYS[1], "state", "failed", "error", ARGV[2], "finishedAt", ARGV[3]) end return 0`
	return t.rdb.Eval(ctx, script, []string{key(libraryID)}, scanID, cause.Error(), time.Now().UTC().Format(time.RFC3339)).Err()
}

// ClaimPrune reports whether the caller should prune the library now: the scan
// finished cleanly and no one has pruned for it yet. It returns the scan's start
// time, so only items older than the scan are removed.
func (t *Tracker) ClaimPrune(ctx context.Context, libraryID, scanID string) (string, bool, error) {
	if scanID == "" {
		return "", false, nil
	}
	const script = `
		if redis.call("HGET", KEYS[1], "scanId") ~= ARGV[1] or
		   redis.call("HGET", KEYS[1], "state") ~= "complete" or
		   redis.call("HGET", KEYS[1], "canPrune") ~= "1" or
		   tonumber(redis.call("HGET", KEYS[1], "discovered") or "0") == 0 or
		   redis.call("HGET", KEYS[1], "failed") ~= "0" or
		   redis.call("HGET", KEYS[1], "pruned") then return "" end
		redis.call("HSET", KEYS[1], "pruned", 1, "pruning", 1)
		return redis.call("HGET", KEYS[1], "startedAt") or ""`
	started, err := t.rdb.Eval(ctx, script, []string{key(libraryID)}, scanID).Text()
	return started, started != "", err
}

func (t *Tracker) FinishPrune(ctx context.Context, libraryID, scanID string) error {
	const script = `if redis.call("HGET", KEYS[1], "scanId") == ARGV[1] then return redis.call("HDEL", KEYS[1], "pruning") end return 0`
	return t.rdb.Eval(ctx, script, []string{key(libraryID)}, scanID).Err()
}

// Get returns the latest scan for a library, or nil if there is none.
func (t *Tracker) Get(ctx context.Context, libraryID string) (*Scan, error) {
	fields := []string{"state", "discovered", "done", "failed", "startedAt", "finishedAt", "error"}
	vals, err := t.rdb.HMGet(ctx, key(libraryID), fields...).Result()
	if err != nil || vals[0] == nil {
		return nil, err
	}
	m := make(map[string]string, len(fields))
	for i, value := range vals {
		if text, ok := value.(string); ok {
			m[fields[i]] = text
		}
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
