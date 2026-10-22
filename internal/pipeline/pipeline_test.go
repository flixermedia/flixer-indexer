package pipeline

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/images"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/probe"
	"github.com/flixermedia/flixer-indexer/internal/progress"
)

type fakeAPI struct {
	libs   []models.Library
	mu     sync.Mutex
	got    []models.Item
	pruned []string
}

func (f *fakeAPI) PruneLibrary(_ context.Context, libraryID, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned = append(f.pruned, libraryID)
	return 0, nil
}

func (f *fakeAPI) Libraries(context.Context) ([]models.Library, error) { return f.libs, nil }
func (f *fakeAPI) Settings(context.Context) (models.Settings, error)   { return models.Settings{}, nil }
func (f *fakeAPI) UpsertItem(_ context.Context, item models.Item) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, item)
	return "id", nil
}

type fakeProbe struct{}

func (fakeProbe) Probe(context.Context, string) (*probe.Result, error) {
	return &probe.Result{Technical: models.Technical{VideoCodec: "h264", Duration: 5400}, Tags: map[string]string{}}, nil
}

func TestEndToEnd(t *testing.T) {
	root := t.TempDir()
	movie := filepath.Join(root, "Movie (2000)")
	_ = os.MkdirAll(movie, 0o755)
	_ = os.WriteFile(filepath.Join(movie, "Movie (2000).mkv"), []byte("x"), 0o644)
	_ = os.WriteFile(filepath.Join(movie, "movie.nfo"), []byte(`<movie><title>The Movie</title><plot>Plot</plot><year>2000</year><genre>Drama</genre></movie>`), 0o644)
	_ = os.WriteFile(filepath.Join(movie, "poster.jpg"), []byte("x"), 0o644)
	show := filepath.Join(root, "Show", "Season 1")
	_ = os.MkdirAll(show, 0o755)
	_ = os.WriteFile(filepath.Join(show, "Show S01E01.mkv"), []byte("x"), 0o644)

	lib := models.Library{ID: "lib", Type: models.LibraryMixed, ImageStorage: models.StorageCache,
		Folders: []models.Folder{{RootID: "r", Path: "Lib", AbsolutePath: root}}}
	api := &fakeAPI{libs: []models.Library{lib}}

	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	client := asynq.NewClient(opt)
	defer client.Close()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tracker := progress.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	p := New(api, images.NewStore(t.TempDir(), "ffmpeg", "test"), fakeProbe{}, tracker, client, log)

	pool := NewPool(opt, p, log)
	conc := map[string]int{}
	for _, q := range config.Queues {
		conc[q] = 2
	}
	if err := pool.Start(conc); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown(context.Background())

	if ok, err := p.StartScan(context.Background(), models.ScanRequest{LibraryID: "lib"}); err != nil || !ok {
		t.Fatalf("start scan: %v %v", ok, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var s *progress.Scan
	for time.Now().Before(deadline) {
		s, _ = tracker.Get(context.Background(), "lib")
		if s != nil && s.State == progress.StateComplete {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if s == nil || s.State != progress.StateComplete || s.Failed != 0 {
		t.Fatalf("scan did not complete: %+v", s)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	types := map[string]models.Item{}
	for _, it := range api.got {
		types[it.Type] = it
	}
	if len(api.got) != 4 {
		t.Fatalf("persisted %d items: %+v", len(api.got), types)
	}
	m := types[models.ItemMovie]
	if m.Metadata.Title != "The Movie" || !m.NFOPresent || !m.LocalImagesUsed || m.Technical == nil || m.Technical.VideoCodec != "h264" || m.Metadata.Runtime != 90 {
		t.Errorf("movie = %+v", m)
	}
	if ep := types[models.ItemEpisode]; ep.EpisodeNumber == nil || *ep.EpisodeNumber != 1 || ep.Metadata.Title == "" {
		t.Errorf("episode = %+v", ep)
	}
}
