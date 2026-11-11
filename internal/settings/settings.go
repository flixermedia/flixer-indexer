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
	streamer    map[string]models.PluginModule
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
	streamerModules := make(map[string]models.PluginModule)
	for _, plugin := range s.Plugins {
		indexerTarget := supportsIndexerCapability(plugin.Capabilities)
		backendTarget := plugin.BackendEntry != "" && supportsBackendCapability(plugin.Capabilities)
		streamerTarget := plugin.StreamerEntry != "" && supportsStreamerCapability(plugin.Capabilities)
		if !indexerTarget && !backendTarget && !streamerTarget {
			pluginState = append(pluginState, models.PluginRuntimeStatus{ID: plugin.ID, State: "error", Error: "no supported backend or indexer capability"})
			continue
		}
		if r.PluginRunner == nil {
			targets := make(map[string]models.PluginTargetStatus)
			if backendTarget {
				targets["flixer"] = models.PluginTargetStatus{State: "error", Error: "WASI runtime is unavailable"}
			}
			if indexerTarget {
				targets["flixer-indexer"] = models.PluginTargetStatus{State: "error", Error: "WASI runtime is unavailable"}
			}
			if streamerTarget {
				targets["flixer-streamer"] = models.PluginTargetStatus{State: "error", Error: "WASI runtime is unavailable"}
			}
			pluginState = append(pluginState, models.PluginRuntimeStatus{ID: plugin.ID, State: "error", Error: "WASI runtime is unavailable", Targets: targets})
			continue
		}

		targets := make(map[string]models.PluginTargetStatus)
		registered := false
		if backendTarget {
			path, err := wasmplugin.ResolveModulePath(r.PluginsPath, plugin.ID, plugin.BackendEntry)
			if err == nil {
				err = r.PluginRunner.Validate(ctx, path)
			}
			if err != nil {
				r.Log.Warn("skipping backend plugin", "plugin", plugin.ID, "error", err)
				targets["flixer"] = models.PluginTargetStatus{State: "error", Error: "backend module could not be loaded"}
			} else {
				backendModules[plugin.ID] = plugin
				targets["flixer"] = models.PluginTargetStatus{State: "registered"}
				registered = true
			}
		}

		if indexerTarget {
			host, _ := r.Client.(wasmplugin.HostCaller)
			provider, err := wasmplugin.New(r.PluginsPath, plugin, r.PluginRunner, host)
			if err != nil {
				r.Log.Warn("skipping indexer plugin", "plugin", plugin.ID, "error", err)
				targets["flixer-indexer"] = models.PluginTargetStatus{State: "error", Error: "indexer module could not be loaded"}
			} else {
				extensions = append(extensions, provider)
				targets["flixer-indexer"] = models.PluginTargetStatus{State: "registered"}
				registered = true
			}
		}
		if streamerTarget {
			path, err := wasmplugin.ResolveModulePath(r.PluginsPath, plugin.ID, plugin.StreamerEntry)
			if err == nil {
				err = r.PluginRunner.Validate(ctx, path)
			}
			if err != nil {
				r.Log.Warn("skipping streamer plugin", "plugin", plugin.ID, "error", err)
				targets["flixer-streamer"] = models.PluginTargetStatus{State: "error", Error: "streamer module could not be loaded"}
			} else {
				streamerModules[plugin.ID] = plugin
				targets["flixer-streamer"] = models.PluginTargetStatus{State: "registered"}
				registered = true
			}
		}
		state := "error"
		if registered {
			state = "registered"
		}
		pluginState = append(pluginState, models.PluginRuntimeStatus{ID: plugin.ID, State: state, Targets: targets})
	}
	r.pluginMu.Lock()
	r.pluginState = pluginState
	r.backend = backendModules
	r.streamer = streamerModules
	r.pluginMu.Unlock()
	reg := registry.Build(s, r.EnvKeys, r.UA, r.Language, extensions...)
	r.Pipeline.SetRegistry(reg)
	r.Log.Info("providers loaded", "providers", reg.Names(), "language", reg.Language())
	introDB, configured := s.Providers["theintrodb"]
	if client, ok := r.Pipeline.IntroDB.(interface{ SetAPIKey(string) }); ok {
		client.SetAPIKey(introDB.APIKey)
	}
	r.Pipeline.SetTheIntroDBEnabled(!configured || introDB.Enabled)
	introDBFallback, configured := s.Providers["introdb"]
	r.Pipeline.SetIntroDBEnabled(!configured || introDBFallback.Enabled)

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

func supportsStreamerCapability(capabilities []string) bool {
	for _, capability := range capabilities {
		switch capability {
		case "streamer.playback.authorize", "streamer.manifest", "streamer.processing":
			return true
		}
	}
	return false
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
		if capability == "api.routes" || capability == "backend.routes" || capability == "auth.directory" || capability == "auth.oidc" || capability == "auth.oauth" {
			return true
		}
	}
	return false
}

// InvokePlugin executes an enabled backend route module in the constrained WASI runtime.
func (r *Reloader) InvokePlugin(ctx context.Context, pluginID string, input []byte) ([]byte, error) {
	return r.InvokePluginTarget(ctx, pluginID, "flixer", input)
}

func (r *Reloader) InvokePluginTarget(ctx context.Context, pluginID, target string, input []byte) ([]byte, error) {
	r.pluginMu.RLock()
	var module models.PluginModule
	var ok bool
	if target == "flixer" {
		module, ok = r.backend[pluginID]
	} else if target == "flixer-streamer" {
		module, ok = r.streamer[pluginID]
	}
	r.pluginMu.RUnlock()
	if !ok {
		return nil, errors.New("backend plugin is not registered")
	}
	if r.PluginRunner == nil {
		return nil, errors.New("WASI runtime is unavailable")
	}
	entry := module.BackendEntry
	if target == "flixer-streamer" {
		entry = module.StreamerEntry
	}
	path, err := wasmplugin.ResolveModulePath(r.PluginsPath, module.ID, entry)
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
