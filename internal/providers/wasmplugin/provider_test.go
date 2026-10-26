package wasmplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

type executorStub struct {
	output []byte
	input  []byte
	path   string
}

type validatingExecutorStub struct {
	executorStub
	err error
}

func (e *validatingExecutorStub) Validate(context.Context, string) error { return e.err }

func (e *executorStub) Run(_ context.Context, path string, input []byte) ([]byte, error) {
	e.path = path
	e.input = append([]byte(nil), input...)
	return e.output, nil
}

func TestMovieDispatchesVersionedRequestAndDecodesResult(t *testing.T) {
	root := makePlugin(t)
	executor := &executorStub{output: []byte(`{"result":{"ids":{"tmdb":"123"},"meta":{"title":"A Film"}}}`)}
	provider, err := New(root, models.PluginModule{
		ID:           "flixer.example",
		Entry:        "provider.wasm",
		Capabilities: []string{"indexer.provider.movie"},
	}, executor)
	if err != nil {
		t.Fatal(err)
	}

	result, err := provider.Movie(context.Background(), providers.Query{Title: "A Film", Year: 2025})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Meta.Title != "A Film" || result.IDs["tmdb"] != "123" {
		t.Fatalf("unexpected provider result: %#v", result)
	}
	if !provider.Supports("indexer.provider.movie") || provider.Supports("indexer.provider.album") {
		t.Fatal("provider capabilities were not preserved")
	}
	var request pluginRequest
	if err := json.Unmarshal(executor.input, &request); err != nil {
		t.Fatal(err)
	}
	if request.APIVersion != pluginAPIVersion || request.Method != "movie" || request.Query == nil || request.Query.Title != "A Film" {
		t.Fatalf("unexpected plugin request: %#v", request)
	}
}

func TestNotFoundResponseMapsToProviderSentinel(t *testing.T) {
	root := makePlugin(t)
	provider, err := New(root, models.PluginModule{ID: "flixer.example", Entry: "provider.wasm"}, &executorStub{
		output: []byte(`{"error":{"code":"not_found","message":"No match"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Series(context.Background(), providers.Query{Title: "missing"})
	if !errors.Is(err, httpx.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestPluginErrorDoesNotLeakGuestMessage(t *testing.T) {
	root := makePlugin(t)
	provider, err := New(root, models.PluginModule{ID: "flixer.example", Entry: "provider.wasm"}, &executorStub{
		output: []byte(`{"error":{"code":"upstream","message":"secret=private-token"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Movie(context.Background(), providers.Query{Title: "Example"})
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("plugin error leaked message: %v", err)
	}
}

func TestNewRejectsUnsafeEntry(t *testing.T) {
	root := makePlugin(t)
	if _, err := New(root, models.PluginModule{ID: "flixer.example", Entry: "../outside.wasm"}, &executorStub{}); err == nil {
		t.Fatal("expected unsafe path error")
	}
}

func TestNewRejectsModuleThatFailsValidation(t *testing.T) {
	root := makePlugin(t)
	executor := &validatingExecutorStub{err: fmt.Errorf("invalid WASM")}
	if _, err := New(root, models.PluginModule{ID: "flixer.example", Entry: "provider.wasm"}, executor); err == nil {
		t.Fatal("expected WASM validation error")
	}
}

func makePlugin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	pluginDir := filepath.Join(root, "flixer.example")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "provider.wasm"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}
