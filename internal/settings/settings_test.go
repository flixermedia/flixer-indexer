package settings

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/flixerclient"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/pipeline"
	"github.com/flixermedia/flixer-indexer/internal/plugins/runner"
	"github.com/flixermedia/flixer-indexer/internal/providers"
	"github.com/flixermedia/flixer-indexer/internal/providers/wasmplugin"
)

type settingsClient struct {
	flixerclient.API
	settings models.Settings
}

func (client settingsClient) Settings(context.Context) (models.Settings, error) {
	return client.settings, nil
}

func TestReloadKeepsHealthyPluginTargetsAvailable(t *testing.T) {
	ctx := context.Background()
	pluginRunner, err := runner.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pluginRunner.Close(ctx) })
	root := t.TempDir()
	pluginDir := filepath.Join(root, "flixer.example")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	module := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, 0x01, 0x04, 0x01, 0x60, 0x00, 0x00, 0x03, 0x02, 0x01, 0x00, 0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00, 0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b}
	if err := os.WriteFile(filepath.Join(pluginDir, "valid.wasm"), module, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, backend, indexer, backendState, indexerState string
	}{
		{"backend survives indexer failure", "valid.wasm", "missing.wasm", "registered", "error"},
		{"indexer survives backend failure", "missing.wasm", "valid.wasm", "error", "registered"},
	} {
		t.Run(test.name, func(t *testing.T) {
			module := models.PluginModule{ID: "flixer.example", BackendEntry: test.backend, Entry: test.indexer, Capabilities: []string{"api.routes", "indexer.provider.movie"}}
			reloader := &Reloader{Client: settingsClient{settings: models.Settings{Plugins: []models.PluginModule{module}}}, Pipeline: &pipeline.Pipeline{}, PluginsPath: root, PluginRunner: pluginRunner, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			if err := reloader.Reload(ctx); err != nil {
				t.Fatal(err)
			}
			statuses := reloader.PluginStatuses()
			if len(statuses) != 1 || statuses[0].State != "registered" || statuses[0].Targets["flixer"].State != test.backendState || statuses[0].Targets["flixer-indexer"].State != test.indexerState {
				t.Fatalf("unexpected target status: %+v", statuses)
			}
			_, err := reloader.InvokePlugin(ctx, "flixer.example", []byte(`{}`))
			if (err == nil) != (test.backendState == "registered") {
				t.Fatalf("unexpected backend availability: %v", err)
			}
		})
	}
}

func TestBuiltMultiServicePlugin(t *testing.T) {
	wasmPath := os.Getenv("FLIXER_TEST_MULTISERVICE_WASM")
	if wasmPath == "" {
		t.Skip("set FLIXER_TEST_MULTISERVICE_WASM to a compiled multi-service example")
	}
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	pluginDir := filepath.Join(root, "example.multiservice")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.wasm"), wasm, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pluginRunner, err := runner.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pluginRunner.Close(ctx) })
	module := models.PluginModule{ID: "example.multiservice", Entry: "plugin.wasm", StreamerEntry: "plugin.wasm", Capabilities: []string{"indexer.provider.movie", "streamer.processing"}}
	reloader := &Reloader{Client: settingsClient{settings: models.Settings{Plugins: []models.PluginModule{module}}}, Pipeline: &pipeline.Pipeline{}, PluginsPath: root, PluginRunner: pluginRunner, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := reloader.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	statuses := reloader.PluginStatuses()
	if len(statuses) != 1 || statuses[0].Targets["flixer-indexer"].State != "registered" || statuses[0].Targets["flixer-streamer"].State != "registered" {
		t.Fatalf("unexpected target status: %+v", statuses)
	}
	provider, err := wasmplugin.New(root, module, pluginRunner)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := provider.Movie(ctx, providers.Query{Title: "Example plugin movie"}); err != nil || result == nil {
		t.Fatalf("movie lookup failed: %+v, %v", result, err)
	}
	output, err := reloader.InvokePluginTarget(ctx, module.ID, "flixer-streamer", []byte(`{"method":"streamer-processing"}`))
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Result struct {
			MaxHeight int `json:"maxHeight"`
		} `json:"result"`
	}
	if err := json.Unmarshal(output, &response); err != nil || response.Result.MaxHeight != 720 {
		t.Fatalf("unexpected streamer output: %s (%v)", output, err)
	}
}

func TestInvokePluginRunsOnlyRegisteredBackendModules(t *testing.T) {
	ctx := context.Background()
	pluginRunner, err := runner.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pluginRunner.Close(ctx); err != nil {
			t.Errorf("close plugin runtime: %v", err)
		}
	})

	root := t.TempDir()
	pluginDir := filepath.Join(root, "flixer.example")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	module := []byte{
		0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00,
		0x01, 0x04, 0x01, 0x60, 0x00, 0x00,
		0x03, 0x02, 0x01, 0x00,
		0x07, 0x0a, 0x01, 0x06, 0x5f, 0x73, 0x74, 0x61, 0x72, 0x74, 0x00, 0x00,
		0x0a, 0x04, 0x01, 0x02, 0x00, 0x0b,
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "backend.wasm"), module, 0o600); err != nil {
		t.Fatal(err)
	}

	reloader := &Reloader{
		PluginsPath:  root,
		PluginRunner: pluginRunner,
		backend: map[string]models.PluginModule{
			"flixer.example": {
				ID:           "flixer.example",
				BackendEntry: "backend.wasm",
				Capabilities: []string{"api.routes"},
			},
		},
	}
	output, err := reloader.InvokePlugin(ctx, "flixer.example", []byte(`{"apiVersion":1,"operation":"status"}`))
	if err != nil {
		t.Fatalf("invoke registered plugin: %v", err)
	}
	if len(output) != 0 {
		t.Fatalf("expected empty module output, got %q", output)
	}
	if _, err := reloader.InvokePlugin(ctx, "flixer.unknown", nil); err == nil {
		t.Fatal("expected unregistered plugin to be unavailable")
	}
}
