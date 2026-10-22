// Package settings keeps the provider registry and worker concurrency in step
// with the configuration flixer owns.
package settings

import (
	"context"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/flixerclient"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/pipeline"
	"github.com/flixermedia/flixer-indexer/internal/providers/registry"
)

// Reloader polls flixer's settings.
type Reloader struct {
	Client   flixerclient.API
	Pipeline *pipeline.Pipeline
	// Pool is nil in API-only processes.
	Pool     *pipeline.Pool
	EnvKeys  map[string]string
	// EnvSubtitles is used while flixer has no OpenSubtitles API key.
	EnvSubtitles models.SubtitleSettings
	UA       string
	Language string
	Interval time.Duration
	Log      *slog.Logger

	mu          sync.Mutex
	lastConc    map[string]int
	lastApplied bool
}

// Reload fetches settings once and applies them. Concurrency is only touched
// for queues whose value flixer changed, so a live adjustment through the
// internal API is not undone by the next poll.
func (r *Reloader) Reload(ctx context.Context) error {
	s, err := r.Client.Settings(ctx)
	if err != nil {
		return err
	}
	reg := registry.Build(s, r.EnvKeys, r.UA, r.Language)
	r.Pipeline.SetRegistry(reg)
	r.Log.Info("providers loaded", "providers", reg.Names(), "language", reg.Language())

	subs := r.EnvSubtitles
	if s.Subtitles != nil && s.Subtitles.APIKey != "" {
		subs = *s.Subtitles
		if len(subs.Languages) == 0 {
			subs.Languages = r.EnvSubtitles.Languages
		}
	}
	r.Pipeline.SetSubtitles(subs, r.UA)
	if r.Pool == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := map[string]int{}
	for q, n := range s.Concurrency {
		if !r.lastApplied || r.lastConc[q] != n {
			changed[q] = n
		}
	}
	if len(changed) > 0 {
		r.Pool.Apply(changed)
	}
	r.lastConc, r.lastApplied = maps.Clone(s.Concurrency), true
	return nil
}

// Run reloads on an interval until ctx ends.
func (r *Reloader) Run(ctx context.Context) {
	if r.Interval <= 0 {
		r.Interval = time.Minute
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Reload(ctx); err != nil {
				r.Log.Warn("settings reload failed; keeping previous settings", "error", err)
			}
		}
	}
}
