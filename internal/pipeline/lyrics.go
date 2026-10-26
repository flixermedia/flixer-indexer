package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/hibiken/asynq"

	"github.com/flixermedia/flixer-indexer/internal/config"
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

// queueLyrics queues a lrclib download for a track that has no lyrics yet.
// Lyrics are optional, so failing to queue only logs.
func (p *Pipeline) queueLyrics(ctx context.Context, item *models.Item, h models.Hints, id string) {
	if p.Lyrics == nil {
		return
	}
	store := lyrics.StorePath(p.LyricsDir, id)
	if store == "" || lyrics.Exists(item.Path) {
		return
	}
	if st, err := os.Stat(store); err == nil && st.Size() > 0 {
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
	if p.Lyrics == nil {
		return nil
	}
	found, err := lyrics.Ensure(ctx, p.Lyrics, task.Path, p.LyricsDir, task.TrackID, task.Query)
	if err != nil {
		p.logger(ctx).Warn("lyrics download failed", "path", task.Path, "error", err)
		return err
	}
	if found {
		p.logger(ctx).Info("lyrics ready", "path", task.Path, "id", task.TrackID)
	} else {
		p.logger(ctx).Info("no synced lyrics found", "path", task.Path)
	}
	return nil
}
