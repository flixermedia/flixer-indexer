package settings

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/plugins/runner"
)

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
