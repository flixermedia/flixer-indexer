package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hibiken/asynq"

	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/introdb"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/theintrodb"
)

const (
	introBackfillPageSize    = 250
	introRateLimitRetryDelay = time.Hour
)

func (p *Pipeline) queueIntroSync(ctx context.Context, item models.Item, itemID string) {
	if !p.introSyncEnabled() || itemID == "" {
		return
	}
	queue, taskType := "", ""
	switch item.Type {
	case models.ItemMovie:
		queue, taskType = config.QueueIntroMovies, TypeIntroMovie
	case models.ItemEpisode:
		queue, taskType = config.QueueIntroEpisodes, TypeIntroEpisode
	default:
		return
	}
	candidate := models.IntroSyncCandidate{ID: itemID, Type: item.Type, ProviderIDs: item.ProviderIDs}
	if item.Technical != nil && item.Technical.Duration > 0 {
		candidate.DurationMs = int64(item.Technical.Duration * 1000)
	}
	if item.Type == models.ItemEpisode {
		candidate.ProviderIDs = item.ParentProviderIDs
		candidate.Season, candidate.Episode = item.SeasonNumber, item.EpisodeNumber
	}
	if _, err := p.Enqueue(ctx, queue, taskType, candidate, maxRetry); err != nil {
		p.logger(ctx).Warn("could not queue intro timing sync", "item", itemID, "type", item.Type, "error", err)
	}
}

func (p *Pipeline) handleIntroMovie(ctx context.Context, task *asynq.Task) error {
	return p.handleIntroSync(ctx, task, models.ItemMovie)
}

func (p *Pipeline) handleIntroEpisode(ctx context.Context, task *asynq.Task) error {
	return p.handleIntroSync(ctx, task, models.ItemEpisode)
}

func (p *Pipeline) handleIntroSync(ctx context.Context, task *asynq.Task, itemType string) error {
	if !p.introSyncEnabled() {
		return nil
	}
	var candidate models.IntroSyncCandidate
	if err := json.Unmarshal(task.Payload(), &candidate); err != nil || candidate.ID == "" || candidate.Type != itemType {
		return fmt.Errorf("bad intro timing task payload: %w", asynq.SkipRetry)
	}
	existing, err := p.Client.ExistingIntroSegments(ctx, candidate.ID)
	if err != nil {
		return fmt.Errorf("check existing intro timings for %s: %w", candidate.ID, err)
	}
	if existing["intro"] || existing["credits"] {
		p.logger(ctx).Info("intro timing sync skipped; segment results already exist", "item", candidate.ID, "type", itemType)
		return nil
	}
	segments := map[string]models.IntroTimeRange{}
	if hasAddress(candidate.ProviderIDs) && (itemType == models.ItemMovie || candidate.Season != nil && candidate.Episode != nil) {
		var primaryErr error
		if p.introDBEnabled.Load() {
			if p.IntroDB == nil {
				primaryErr = errors.New("TheIntroDB client is unavailable")
			} else {
				segments, primaryErr = p.IntroDB.GetMedia(ctx, candidate)
			}
		}
		primaryRateLimited := errors.Is(primaryErr, theintrodb.ErrRateLimited)
		if primaryErr != nil && !primaryRateLimited {
			p.logger(ctx).Warn("TheIntroDB lookup failed; trying IntroDB fallback", "item", candidate.ID, "error", primaryErr)
		}

		needsFallback := p.introDBFallbackEnabled.Load() &&
			candidate.ProviderIDs["imdb"] != "" &&
			(primaryErr != nil || segments["intro"].EndMs == 0 || segments["credits"].EndMs == 0)
		if primaryRateLimited && !needsFallback {
			return retryAfter(primaryErr, introRateLimitRetryDelay)
		}
		if needsFallback {
			if p.IntroDBFallback == nil {
				err := errors.New("IntroDB fallback client is unavailable")
				if primaryRateLimited {
					return retryAfter(err, introRateLimitRetryDelay)
				}
				return err
			}
			fallbackSegments, err := p.IntroDBFallback.GetMedia(ctx, candidate)
			if err != nil {
				err = fmt.Errorf("IntroDB fallback lookup failed: %w", err)
				if primaryRateLimited || errors.Is(err, introdb.ErrRateLimited) {
					return retryAfter(err, introRateLimitRetryDelay)
				}
				return err
			}
			if primaryRateLimited && len(fallbackSegments) == 0 {
				return retryAfter(errors.New("TheIntroDB rate limited the request and IntroDB had no timings"), introRateLimitRetryDelay)
			}
			if segments == nil {
				segments = make(map[string]models.IntroTimeRange, len(fallbackSegments))
			}
			for name, segment := range fallbackSegments {
				if segments[name].EndMs == 0 {
					segments[name] = segment
				}
			}
			if primaryErr != nil && !primaryRateLimited {
				p.logger(ctx).Info("IntroDB fallback recovered intro timings after TheIntroDB failed", "item", candidate.ID)
			}
		} else if primaryErr != nil {
			return primaryErr
		}
	}
	return p.Client.UpsertIntroSegments(ctx, models.IntroSegmentUpdate{ItemID: candidate.ID, Segments: segments})
}

func hasAddress(ids map[string]string) bool {
	for _, name := range []string{"tvdb", "tmdb", "imdb"} {
		if ids[name] != "" {
			return true
		}
	}
	return false
}

// RunIntroBackfill queues already indexed movies and episodes on startup and
// periodically thereafter. Enabling the provider wakes the backfill immediately.
func (p *Pipeline) RunIntroBackfill(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 12 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	p.queueExistingIntroData(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.introSyncWake:
			if p.introSyncEnabled() {
				p.queueExistingIntroData(ctx)
			}
		case <-ticker.C:
			p.queueExistingIntroData(ctx)
		}
	}
}

func (p *Pipeline) queueExistingIntroData(ctx context.Context) {
	if !p.introSyncEnabled() {
		return
	}
	for _, itemType := range []string{models.ItemMovie, models.ItemEpisode} {
		if _, err := p.QueueIntroBackfill(ctx, "", itemType); err != nil {
			p.Log.Warn("TheIntroDB backfill failed", "type", itemType, "error", err)
		}
	}
}

// QueueIntroBackfill schedules provider timing lookups for indexed items, optionally limited to one library.
func (p *Pipeline) QueueIntroBackfill(ctx context.Context, libraryID, itemType string) (int, error) {
	if !p.introSyncEnabled() {
		return 0, errors.New("intro segment providers are disabled")
	}
	if itemType != models.ItemMovie && itemType != models.ItemEpisode {
		return 0, fmt.Errorf("unsupported intro segment type %q", itemType)
	}
	queue, taskType := config.QueueIntroMovies, TypeIntroMovie
	if itemType == models.ItemEpisode {
		queue, taskType = config.QueueIntroEpisodes, TypeIntroEpisode
	}
	queued := 0
	cursor := ""
	for {
		items, next, err := p.Client.IntroSyncCandidates(ctx, itemType, libraryID, cursor, introBackfillPageSize)
		if err != nil {
			return queued, fmt.Errorf("load %s intro candidates: %w", itemType, err)
		}
		for _, candidate := range items {
			if candidate.Type != itemType || candidate.ID == "" {
				continue
			}
			added, err := p.Enqueue(ctx, queue, taskType, candidate, maxRetry)
			if err != nil {
				return queued, fmt.Errorf("queue intro timing for %s: %w", candidate.ID, err)
			}
			if added {
				queued++
			}
		}
		if next == "" || next == cursor || len(items) == 0 {
			break
		}
		cursor = next
	}
	return queued, nil
}
