package registry

import (
	"context"
	"slices"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

type testExtension struct {
	capabilities map[string]bool
	calls        int
}

func (p *testExtension) Name() string { return "flixer.test-plugin" }

func (p *testExtension) Supports(capability string) bool { return p.capabilities[capability] }

func (p *testExtension) Movie(_ context.Context, query providers.Query) (*providers.Result, error) {
	p.calls++
	return &providers.Result{Meta: models.Metadata{Title: query.Title}}, nil
}

func TestBuildRegistersExtensionOnlyForDeclaredCapability(t *testing.T) {
	extension := &testExtension{capabilities: map[string]bool{"indexer.provider.movie": true}}
	registry := Build(models.Settings{}, nil, "test", "en", extension)
	result, err := registry.Movie(context.Background(), providers.Query{Title: "Plugin title"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Meta.Title != "Plugin title" || extension.calls != 1 {
		t.Fatalf("extension was not called: result=%#v calls=%d", result, extension.calls)
	}
	if names := registry.Names(); !slices.Contains(names, extension.Name()) {
		t.Fatalf("unexpected provider names: %v", names)
	}

	unsupported := &testExtension{capabilities: map[string]bool{"indexer.provider.series": true}}
	registry = Build(models.Settings{}, nil, "test", "en", unsupported)
	result, err = registry.Movie(context.Background(), providers.Query{Title: "No movie capability"})
	if err != nil || result != nil || unsupported.calls != 0 {
		t.Fatalf("undeclared capability was invoked: result=%#v err=%v calls=%d", result, err, unsupported.calls)
	}
}
