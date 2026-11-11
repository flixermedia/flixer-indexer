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

type continuationExecutor struct {
	requests []pluginRequest
	outputs  [][]byte
}

func (e *continuationExecutor) Run(_ context.Context, _ string, input []byte) ([]byte, error) {
	var request pluginRequest
	if err := json.Unmarshal(input, &request); err != nil {
		return nil, err
	}
	e.requests = append(e.requests, request)
	output := e.outputs[0]
	e.outputs = e.outputs[1:]
	return output, nil
}

type hostStub struct {
	pluginID string
	calls    []json.RawMessage
}

func (host *hostStub) CallPluginHost(_ context.Context, pluginID string, calls []json.RawMessage) ([]json.RawMessage, error) {
	host.pluginID, host.calls = pluginID, calls
	return []json.RawMessage{json.RawMessage(`{"id":"lookup","ok":true,"body":"example"}`)}, nil
}

func TestProviderResumesAfterHostCalls(t *testing.T) {
	executor := &continuationExecutor{outputs: [][]byte{
		[]byte(`{"hostCalls":[{"id":"lookup","op":"http.get","url":"https://api.example.com"}],"continuation":{"stage":1}}`),
		[]byte(`{"result":{"meta":{"title":"Resolved"}}}`),
	}}
	host := &hostStub{}
	provider, err := New(makePlugin(t), models.PluginModule{ID: "flixer.example", Entry: "provider.wasm"}, executor, host)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Movie(context.Background(), providers.Query{Title: "Example"})
	if err != nil || result == nil || result.Meta.Title != "Resolved" {
		t.Fatalf("provider result = %+v, %v", result, err)
	}
	if host.pluginID != "flixer.example" || len(host.calls) != 1 || len(executor.requests) != 2 || string(executor.requests[1].Continuation) != `{"stage":1}` || len(executor.requests[1].HostResults) != 1 {
		t.Fatalf("unexpected host continuation: host=%+v requests=%+v", host, executor.requests)
	}
}

func TestProviderRejectsHostCallsWithoutBroker(t *testing.T) {
	executor := &executorStub{output: []byte(`{"hostCalls":[{"id":"lookup","op":"http.get"}]}`)}
	provider, err := New(makePlugin(t), models.PluginModule{ID: "flixer.example", Entry: "provider.wasm"}, executor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Movie(context.Background(), providers.Query{}); err == nil {
		t.Fatal("expected host calls to fail without a broker")
	}
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
