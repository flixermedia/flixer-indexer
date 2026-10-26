// Package settings keeps the provider registry and worker concurrency in step
// with the configuration flixer owns.
package settings

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/flixerclient"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/pipeline"
	"github.com/flixermedia/flixer-indexer/internal/plugins/runner"
	"github.com/flixermedia/flixer-indexer/internal/providers"
	"github.com/flixermedia/flixer-indexer/internal/providers/registry"
	"github.com/flixermedia/flixer-indexer/internal/providers/wasmplugin"
)

// Reloader polls flixer's settings.
type Reloader struct {
	Client   flixerclient.API
	Pipeline *pipeline.Pipeline
	// Pool is nil in API-only processes.
	Pool    *pipeline.Pool
	EnvKeys map[string]string
	// EnvSubtitles is used while flixer has no OpenSubtitles API key.
	EnvSubtitles models.SubtitleSettings
	PluginsPath  string
	PluginRunner *runner.Runner
	UA           string
	Language     string
	Interval     time.Duration
	Log          *slog.Logger

	mu          sync.Mutex
	lastConc    map[string]int
	lastApplied bool
	pluginMu    sync.RWMutex
	pluginState []models.PluginRuntimeStatus
	backend     map[string]models.PluginModule
}

// Reload fetches settings once and applies them. Concurrency is only touched
// for queues whose value flixer changed, so a live adjustment through the
// internal API is not undone by the next poll.
func (r *Reloader) Reload(ctx context.Context) error {
	s, err := r.Client.Settings(ctx)
	if err != nil {
		return err
	}
	var extensions []providers.Provider
	pluginState := make([]models.PluginRuntimeStatus, 0, len(s.Plugins))
	backendModules := make(map[string]models.PluginModule)
	for _, plugin := range s.Plugins {
		indexerTarget := supportsIndexerCapability(plugin.Capabilities)
		backendTarget := plugin.BackendEntry != "" && supportsBackendCapability(plugin.Capabilities)
		if !indexerTarget && !backendTarget {
			pluginState = append(pluginState, models.PluginRuntimeStatus{ID: plugin.ID, State: "error", Error: "no supported backend or indexer capability"})
			continue
		}
		if r.PluginRunner == nil {
			pluginState = append(pluginState, models.PluginRuntimeStatus{ID: plugin.ID, State: "error", Error: "WASI runtime is unavailable"})
			continue
		}

		if backendTarget {
			path, err := wasmplugin.ResolveModulePath(r.PluginsPath, plugin.ID, plugin.BackendEntry)
			if err == nil {
				err = r.PluginRunner.Validate(ctx, path)
			}
			if err != nil {
				r.Log.Warn("skipping backend plugin", "plugin", plugin.ID, "error", err)
				pluginState = append(pluginState, models.PluginRuntimeStatus{ID: plugin.ID, State: "error", Error: "backend module could not be loaded"})
				continue
			}
			backendModules[plugin.ID] = plugin
		}

		if indexerTarget {
			provider, err := wasmplugin.New(r.PluginsPath, plugin, r.PluginRunner)
			if err != nil {
				r.Log.Warn("skipping indexer plugin", "plugin", plugin.ID, "error", err)
				delete(backendModules, plugin.ID)
				pluginState = append(pluginState, models.PluginRuntimeStatus{ID: plugin.ID, State: "error", Error: "indexer module could not be loaded"})
				continue
			}
			extensions = append(extensions, provider)
		}
		pluginState = append(pluginState, models.PluginRuntimeStatus{ID: plugin.ID, State: "registered"})
	}
	r.pluginMu.Lock()
	r.pluginState = pluginState
	r.backend = backendModules
	r.pluginMu.Unlock()
	reg := registry.Build(s, r.EnvKeys, r.UA, r.Language, extensions...)
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
	r.Pipeline.SetThemesEnabled(s.Themes != nil && s.Themes.Enabled)
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

func supportsIndexerCapability(capabilities []string) bool {
	for _, capability := range capabilities {
		switch capability {
		case "indexer.provider.movie", "indexer.provider.series", "indexer.provider.season", "indexer.provider.episode", "indexer.provider.album", "indexer.provider.images":
			return true
		}
	}
	return false
}

func supportsBackendCapability(capabilities []string) bool {
	for _, capability := range capabilities {
		if capability == "api.routes" || capability == "backend.routes" {
			return true
		}
	}
	return false
}

// InvokePlugin executes an enabled backend route module in the constrained WASI runtime.
func (r *Reloader) InvokePlugin(ctx context.Context, pluginID string, input []byte) ([]byte, error) {
	r.pluginMu.RLock()
	module, ok := r.backend[pluginID]
	r.pluginMu.RUnlock()
	if !ok {
		return nil, errors.New("backend plugin is not registered")
	}
	if r.PluginRunner == nil {
		return nil, errors.New("WASI runtime is unavailable")
	}
	path, err := wasmplugin.ResolveModulePath(r.PluginsPath, module.ID, module.BackendEntry)
	if err != nil {
		return nil, err
	}
	return r.PluginRunner.Run(ctx, path, input)
}

func (r *Reloader) PluginStatuses() []models.PluginRuntimeStatus {
	r.pluginMu.RLock()
	defer r.pluginMu.RUnlock()
	return append([]models.PluginRuntimeStatus(nil), r.pluginState...)
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
