package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/introdb"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/theintrodb"
)

var errIntroDeferred = errors.New("intro provider temporarily unavailable; retry scheduled")

const introMissTTL = 7 * 24 * time.Hour
const introRequestInterval = time.Second

func deferredIntro(delay time.Duration) error {
	if delay <= 0 {
		delay = introRateLimitRetryDelay
	}
	jitter := time.Duration(rand.Int64N(int64(min(delay/5, 5*time.Minute)) + 1))
	return retryAfter(errIntroDeferred, delay+jitter)
}

func (p *Pipeline) introPermit(ctx context.Context, provider string) (time.Duration, error) {
	if p.TaskLocks == nil {
		return 0, nil
	}
	const script = `
local wait = redis.call("PTTL", KEYS[1])
if wait > 0 then return wait end
wait = redis.call("PTTL", KEYS[2])
if wait > 0 then return wait end
redis.call("PSETEX", KEYS[2], ARGV[1], "1")
return 0`
	result, err := p.TaskLocks.Eval(ctx, script, []string{"flixer:intro:cooldown:" + provider, "flixer:intro:spacing:" + provider}, introRequestInterval.Milliseconds()).Int64()
	if err != nil {
		return 0, fmt.Errorf("check %s intro provider throttle: %w", provider, err)
	}
	return time.Duration(result) * time.Millisecond, nil
}

func (p *Pipeline) introCooldown(ctx context.Context, provider string, err error) (time.Duration, error) {
	delay := introRateLimitRetryDelay
	var limited *theintrodb.RateLimitError
	if errors.As(err, &limited) && limited.RetryAfter > 0 {
		delay = limited.RetryAfter
	}
	var fallbackLimited *introdb.RateLimitError
	if errors.As(err, &fallbackLimited) && fallbackLimited.RetryAfter > 0 {
		delay = fallbackLimited.RetryAfter
	}
	if p.TaskLocks == nil {
		return delay, nil
	}
	const script = `
local old = redis.call("PTTL", KEYS[1])
if old < tonumber(ARGV[1]) then redis.call("PSETEX", KEYS[1], ARGV[1], "1") end
return old`
	previous, writeErr := p.TaskLocks.Eval(ctx, script, []string{"flixer:intro:cooldown:" + provider}, delay.Milliseconds()).Int64()
	if writeErr != nil {
		return 0, fmt.Errorf("save %s intro provider cooldown: %w", provider, writeErr)
	}
	if previous <= 0 {
		p.Log.Warn("Intro provider rate limited; lookups temporarily deferred", "provider", provider, "retry_after", delay)
	}
	return max(delay, time.Duration(previous)*time.Millisecond), nil
}

func introMissKey(candidate models.IntroSyncCandidate, primary, fallback bool) (string, error) {
	raw, err := json.Marshal(struct {
		Candidate models.IntroSyncCandidate `json:"candidate"`
		Primary   bool                      `json:"primary"`
		Fallback  bool                      `json:"fallback"`
	}{candidate, primary, fallback})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "flixer:intro:miss:" + hex.EncodeToString(sum[:]), nil
}
