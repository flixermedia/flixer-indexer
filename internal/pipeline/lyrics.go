package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/hibiken/asynq"

	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/flixerclient"
	"github.com/flixermedia/flixer-indexer/internal/lyrics"
	"github.com/flixermedia/flixer-indexer/internal/models"
)

// lyricsRetries is high because lrclib's rate limits and outages are temporary.
const lyricsRetries = 8

// lyricsTask is one track whose lyrics should be downloaded.
type lyricsTask struct {
	LibraryID string       `json:"libraryId"`
	ItemType  string       `json:"itemType"`
	Path      string       `json:"path"`
	TrackID   string       `json:"trackId"`
	Query     lyrics.Query `json:"query"`
}

// queueLyrics syncs existing lyrics and fetches missing lyrics when enabled.
// Lyrics are optional, so failing to queue only logs.
func (p *Pipeline) queueLyrics(ctx context.Context, item *models.Item, h models.Hints, id string) {
	store := lyrics.StorePath(p.LyricsDir, id)
	hasLyrics := lyrics.Exists(item.Path)
	if !hasLyrics && store != "" {
		if st, err := os.Stat(store); err == nil && !st.IsDir() && st.Size() > 0 {
			hasLyrics = true
		}
	}
	if !hasLyrics && p.Lyrics == nil {
		return
	}
	m := item.Metadata
	q := lyrics.Query{
		Artist:      firstNonEmpty(m.Artist, h.Artist),
		AlbumArtist: m.AlbumArtist,
		Title:       firstNonEmpty(m.Title, h.Title),
		Album:       firstNonEmpty(m.Album, h.Album),
	}
	if item.Technical != nil {
		q.Duration = item.Technical.Duration
	}
	task := lyricsTask{LibraryID: item.LibraryID, ItemType: item.Type, Path: item.Path, TrackID: id, Query: q}
	if _, err := p.Enqueue(ctx, config.QueueLyrics, TypeLyrics, task, lyricsRetries); err != nil {
		p.logger(ctx).Warn("could not queue lyrics", "path", item.Path, "error", err)
	}
}

func (p *Pipeline) handleLyrics(ctx context.Context, t *asynq.Task) error {
	var task lyricsTask
	if err := json.Unmarshal(t.Payload(), &task); err != nil || task.TrackID == "" || task.Path == "" {
		return fmt.Errorf("bad lyrics payload: %w", asynq.SkipRetry)
	}
	found, err := lyrics.Ensure(ctx, p.Lyrics, task.Path, p.LyricsDir, task.TrackID, task.Query)
	if err != nil {
		p.logger(ctx).Warn("lyrics download failed", "path", task.Path, "error", err)
		return err
	}
	text := ""
	if found {
		for _, path := range []string{lyrics.Path(task.Path), lyrics.StorePath(p.LyricsDir, task.TrackID)} {
			if path == "" {
				continue
			}
			data, readErr := os.ReadFile(path)
			if readErr == nil {
				text = string(data)
				break
			}
			if !os.IsNotExist(readErr) {
				return fmt.Errorf("read track lyrics: %w", readErr)
			}
		}
		if text == "" {
			return fmt.Errorf("lyrics were reported available but no lyrics file could be read")
		}
		p.logger(ctx).Info("lyrics ready", "path", task.Path, "id", task.TrackID)
	} else {
		p.logger(ctx).Info("no synced lyrics found", "path", task.Path)
	}
	if err := p.Client.UpsertItemLyrics(ctx, task.TrackID, text); err != nil {
		if errors.Is(err, flixerclient.ErrPermanent) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return fmt.Errorf("sync track lyrics with flixer: %w", err)
	}
	return nil
}
