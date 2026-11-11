package registry

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

type testExtension struct {
	capabilities map[string]bool
	calls        int
}

type resultProvider struct {
	name   string
	result *providers.Result
}

func (p resultProvider) Name() string { return p.name }
func (p resultProvider) Movie(context.Context, providers.Query) (*providers.Result, error) {
	return p.result, nil
}

func TestManualMatchRejectsUnverifiedAndConflictingProviderResults(t *testing.T) {
	wrong := resultProvider{"wrong", &providers.Result{IDs: map[string]string{"tvdb": "wrong"}, Meta: models.Metadata{Title: "Wrong show"}}}
	conflicting := resultProvider{"conflicting", &providers.Result{IDs: map[string]string{"tmdb": "200", "tvdb": "wrong"}, Meta: models.Metadata{Title: "Conflicting show"}}}
	right := resultProvider{"right", &providers.Result{IDs: map[string]string{"tmdb": "200", "tvdb": "right"}, Meta: models.Metadata{Title: "Correct show"}}}
	q := providers.Query{IDs: map[string]string{"tmdb": "200", "tvdb": "right"}, RequireIDMatch: true}

	result, err := run(context.Background(), q, []resultProvider{wrong, conflicting, right}, func(p resultProvider, q providers.Query) (*providers.Result, error) {
		return p.Movie(context.Background(), q)
	})
	if err != nil || result == nil || result.Meta.Title != "Correct show" {
		t.Fatalf("manual match returned wrong metadata: result=%+v err=%v", result, err)
	}
	result, err = run(context.Background(), q, []resultProvider{wrong, conflicting}, func(p resultProvider, q providers.Query) (*providers.Result, error) {
		return p.Movie(context.Background(), q)
	})
	if err != nil || result != nil {
		t.Fatalf("unverified match was accepted: result=%+v err=%v", result, err)
	}
}

func TestManualAndUnrestrictedResultsHaveSeparateCacheEntries(t *testing.T) {
	provider := resultProvider{"wrong", &providers.Result{IDs: map[string]string{"tmdb": "wrong"}, Meta: models.Metadata{Title: "Wrong show"}}}
	reg := &Registry{movie: []providers.MovieProvider{provider}, cache: providers.NewCache(time.Minute)}
	query := providers.Query{Title: "Show", IDs: map[string]string{"tmdb": "correct"}}
	unrestricted, err := reg.Movie(context.Background(), query)
	if err != nil || unrestricted == nil {
		t.Fatalf("unrestricted result = %+v, %v", unrestricted, err)
	}
	query.RequireIDMatch = true
	strict, err := reg.Movie(context.Background(), query)
	if err != nil || strict != nil {
		t.Fatalf("manual query reused unrestricted cache result = %+v, %v", strict, err)
	}
}

func TestClonePersonCreditsAreIndependent(t *testing.T) {
	original := &providers.Result{Meta: models.Metadata{Cast: []models.Person{{Name: "Actor"}}, Crew: []models.Person{{Name: "Director"}}}}
	copy := clone(original)
	copy.Meta.Cast[0].FallbackImage = "https://image.tmdb.org/actor.jpg"
	copy.Meta.Crew[0].FallbackImage = "https://image.tmdb.org/director.jpg"
	if original.Meta.Cast[0].FallbackImage != "" || original.Meta.Crew[0].FallbackImage != "" {
		t.Fatal("editing a result changed cached person credits")
	}
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

func TestReplacementCapabilityChangesOnlyItsProviderOrder(t *testing.T) {
	first := &testExtension{capabilities: map[string]bool{"indexer.provider.movie": true}}
	override := &testExtension{capabilities: map[string]bool{"indexer.provider.movie": true, "indexer.provider.movie.replace": true}}
	reg := Build(models.Settings{}, nil, "test", "en", first, override)
	if len(reg.movie) != 2 || reg.movie[0] != override || reg.movie[1] != first {
		t.Fatalf("unexpected provider order: %+v", reg.movie)
	}
	if len(reg.series) != 0 {
		t.Fatal("movie override must not register another provider type")
	}
}
