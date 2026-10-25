package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/subtitles"
)

const (
	subtitleTimeout = 45 * time.Second
	// A video with no match is not searched again for a day, so rescans stay cheap.
	subtitleRetryAfter = 24 * time.Hour
)

type subtitleState struct {
	creds     [3]string
	client    *subtitles.Client
	languages []string
	autoFetch bool
}

// SetSubtitles applies the OpenSubtitles configuration. An empty API key turns
// the integration off. The client (and its login token) is kept while the
// credentials stay the same.
func (p *Pipeline) SetSubtitles(s models.SubtitleSettings, userAgent string) {
	next := &subtitleState{languages: s.Languages, autoFetch: s.AutoFetch}
	if s.APIKey != "" {
		next.creds = [3]string{s.APIKey, s.Username, s.Password}
		if cur := p.subs.Load(); cur != nil && cur.client != nil && cur.creds == next.creds {
			next.client = cur.client
		} else {
			next.client = subtitles.NewClient(s.APIKey, s.Username, s.Password, userAgent)
		}
	}
	p.subs.Store(next)
}

// SubtitleConfig returns the current client (nil when not configured), the
// languages to fetch automatically and whether automatic fetching is on.
func (p *Pipeline) SubtitleConfig() (*subtitles.Client, []string, bool) {
	s := p.subs.Load()
	if s == nil {
		return nil, nil, false
	}
	return s.client, s.languages, s.autoFetch
}

// SubtitleQuery describes a video for OpenSubtitles. Episodes are matched
// through the series ids plus season and episode numbers.
func SubtitleQuery(item models.Item, h models.Hints) subtitles.Query {
	m := item.Metadata
	q := subtitles.Query{Text: firstNonEmpty(m.Title, h.Title), Year: firstNonZero(m.Year, h.Year)}
	if item.Type == models.ItemEpisode {
		q.Episodic = true
		q.Text = firstNonEmpty(h.SeriesTitle, q.Text)
		q.ParentIMD, q.ParentTMD = h.SeriesIDs["imdb"], h.SeriesIDs["tmdb"]
		if item.SeasonNumber != nil {
			q.Season = *item.SeasonNumber
		}
		if item.EpisodeNumber != nil {
			q.Episode = *item.EpisodeNumber
		}
		return q
	}
	q.IMDB, q.TMDB = item.ProviderIDs["imdb"], item.ProviderIDs["tmdb"]
	return q
}

// ensureSubtitles downloads a sidecar for each wanted language that has neither
// a sidecar nor an embedded track. Failures only log; subtitles are optional.
func (p *Pipeline) ensureSubtitles(ctx context.Context, item *models.Item, h models.Hints) {
	client, wanted, auto := p.SubtitleConfig()
	if client == nil || !auto || len(wanted) == 0 {
		return
	}
	switch item.Type {
	case models.ItemMovie, models.ItemEpisode, models.ItemVideo:
	default:
		return
	}
	if last, ok := p.subtitleMisses.Load(item.Path); ok && time.Since(last.(time.Time)) < subtitleRetryAfter {
		return
	}
	target := subtitles.Target{Video: item.Path, Fallback: subtitles.FallbackDir(p.SubtitleDir, item.RootID, item.RelativePath)}
	existing, err := subtitles.List(target)
	if err != nil {
		return
	}
	if item.Technical != nil {
		for _, lang := range item.Technical.SubtitleLanguages {
			existing = append(existing, subtitles.Track{Language: lang})
		}
	}
	var missing []string
	for _, lang := range wanted {
		if !subtitles.Has(existing, lang) {
			missing = append(missing, lang)
		}
	}
	if len(missing) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, subtitleTimeout)
	defer cancel()
	q := SubtitleQuery(*item, h)
	if hash, err := subtitles.Hash(item.Path); err == nil {
		q.Hash = hash
	}
	for _, lang := range missing {
		path, err := client.Fetch(ctx, target, q, lang)
		switch {
		case err == nil:
			p.Log.Info("subtitles downloaded", "path", path, "language", lang)
		case errors.Is(err, subtitles.ErrNotFound):
			p.subtitleMisses.Store(item.Path, time.Now())
		case errors.Is(err, subtitles.ErrQuota):
			p.Log.Warn("opensubtitles quota reached; skipping automatic subtitles for now")
			return
		default:
			p.Log.Warn("subtitles unavailable", "path", item.Path, "language", lang, "error", err)
			if ctx.Err() != nil {
				return
			}
		}
	}
}
