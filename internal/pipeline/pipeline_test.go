package pipeline

import (
	"context"
	"encoding/json"
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
	"github.com/flixermedia/flixer-indexer/internal/trickplay"
)

type fakeAPI struct {
	libs                  []models.Library
	mu                    sync.Mutex
	got                   []models.Item
	pruned                []string
	introUpdates          []models.IntroSegmentUpdate
	lyricsUpdates         map[string]string
	existingIntroSegments models.ExistingIntroSegments
}

func (f *fakeAPI) PruneLibrary(_ context.Context, libraryID, _ string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pruned = append(f.pruned, libraryID)
	return 0, nil
}

func (f *fakeAPI) Libraries(context.Context) ([]models.Library, error) { return f.libs, nil }
func (f *fakeAPI) Settings(context.Context) (models.Settings, error)   { return models.Settings{}, nil }
func (f *fakeAPI) IntroSyncCandidates(context.Context, string, string, string, int) ([]models.IntroSyncCandidate, string, error) {
	return nil, "", nil
}
func (f *fakeAPI) ExistingIntroSegments(context.Context, string) (models.ExistingIntroSegments, error) {
	return f.existingIntroSegments, nil
}
func (f *fakeAPI) UpsertIntroSegments(_ context.Context, update models.IntroSegmentUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.introUpdates = append(f.introUpdates, update)
	return nil
}
func (f *fakeAPI) UpsertItemLyrics(_ context.Context, itemID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lyricsUpdates == nil {
		f.lyricsUpdates = make(map[string]string)
	}
	f.lyricsUpdates[itemID] = text
	return nil
}
func (f *fakeAPI) UpsertItem(_ context.Context, item models.Item) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, item)
	return "id", nil
}

func TestHandleLyricsSyncsExistingSidecar(t *testing.T) {
	root := t.TempDir()
	audio := filepath.Join(root, "song.flac")
	if err := os.WriteFile(audio, []byte("audio"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "song.lrc"), []byte("[00:01.00]hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{}
	pipeline := &Pipeline{
		Client:    api,
		LyricsDir: filepath.Join(root, "cache"),
		Log:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	payload, err := json.Marshal(lyricsTask{Path: audio, TrackID: "track-1"})
	if err != nil {
		t.Fatal(err)
	}

	if err := pipeline.handleLyrics(context.Background(), asynq.NewTask(TypeLyrics, payload)); err != nil {
		t.Fatal(err)
	}
	if got := api.lyricsUpdates["track-1"]; got != "[00:01.00]hello" {
		t.Fatalf("lyrics update = %q", got)
	}
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
	for _, q := range config.WorkerQueues {
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

func TestTrickplayOnlyScanSkipsProgressAndUsesDistinctScanPayload(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := models.Library{ID: "lib", Type: models.LibraryMovies, Folders: []models.Folder{{RootID: "root", Path: "Movies", AbsolutePath: root}}}
	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	queue := asynq.NewClient(opt)
	defer queue.Close()
	tracker := progress.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := New(&fakeAPI{libs: []models.Library{lib}}, images.NewStore(t.TempDir(), "ffmpeg", "test"), fakeProbe{}, tracker, queue, logger)
	p.SetTrickplay(true, t.TempDir(), "ffmpeg", 10, 320)
	payload, err := json.Marshal(models.ScanRequest{LibraryID: "lib", TrickplayOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.handleScan(context.Background(), asynq.NewTask(TypeScan, payload)); err != nil {
		t.Fatal(err)
	}
	if scan, err := tracker.Get(context.Background(), "lib"); err != nil || scan != nil {
		t.Fatalf("trickplay-only scan changed progress: scan=%+v err=%v", scan, err)
	}
	for _, req := range []models.ScanRequest{{LibraryID: "lib"}, {LibraryID: "lib", TrickplayOnly: true}} {
		queued, err := p.StartScan(context.Background(), req)
		if err != nil || !queued {
			t.Fatalf("scan payload %+v queued=%v err=%v", req, queued, err)
		}
	}
}

func TestTargetedScanQueuesSelectedJobsWithoutFullScanProgress(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := models.Library{ID: "lib", Type: models.LibraryMovies, Folders: []models.Folder{{RootID: "root", Path: "Movies", AbsolutePath: root}}}
	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	queue := asynq.NewClient(opt)
	defer queue.Close()
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	tracker := progress.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := New(&fakeAPI{libs: []models.Library{lib}}, images.NewStore(t.TempDir(), "ffmpeg", "test"), fakeProbe{}, tracker, queue, logger)

	payload, err := json.Marshal(models.ScanRequest{LibraryID: "lib", Jobs: []string{"images", "descriptions"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.handleScan(context.Background(), asynq.NewTask(TypeScan, payload)); err != nil {
		t.Fatal(err)
	}
	if scan, err := tracker.Get(context.Background(), "lib"); err != nil || scan != nil {
		t.Fatalf("targeted scan changed progress: scan=%+v err=%v", scan, err)
	}
	tasks, err := inspector.ListPendingTasks(config.QueueIdentify)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("identify tasks = %d, %v", len(tasks), err)
	}
	work, err := decodeWork(asynq.NewTask(TypeIdentify, tasks[0].Payload))
	if err != nil || len(work.Jobs) != 2 || !hasJob(work, "images", "descriptions") || !work.Untracked {
		t.Fatalf("targeted work = %+v, %v", work, err)
	}
}

func TestTrackedTargetedScanCompletesAfterItemPipeline(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := models.Library{ID: "lib", Type: models.LibraryMovies, Folders: []models.Folder{{RootID: "root", Path: "Movies", AbsolutePath: root}}}
	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	queue := asynq.NewClient(opt)
	defer queue.Close()
	tracker := progress.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := New(&fakeAPI{libs: []models.Library{lib}}, images.NewStore(t.TempDir(), "ffmpeg", "test"), fakeProbe{}, tracker, queue, logger)
	pool := NewPool(opt, p, logger)
	concurrency := map[string]int{}
	for _, q := range config.WorkerQueues {
		concurrency[q] = 2
	}
	if err := pool.Start(concurrency); err != nil {
		t.Fatal(err)
	}
	defer pool.Shutdown(context.Background())

	if queued, err := p.StartScan(context.Background(), models.ScanRequest{
		LibraryID: "lib", Jobs: []string{"images", "descriptions"}, TrackProgress: true,
	}); err != nil || !queued {
		t.Fatalf("start tracked targeted scan: queued=%v err=%v", queued, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	var scan *progress.Scan
	for time.Now().Before(deadline) {
		scan, _ = tracker.Get(context.Background(), "lib")
		if scan != nil && (scan.State == progress.StateComplete || scan.State == progress.StateFailed) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if scan == nil || scan.State != progress.StateComplete || scan.Done != 1 || scan.Failed != 0 {
		t.Fatalf("tracked targeted scan did not complete: %+v", scan)
	}
}

func TestTrackedTrickplayScanTracksQueuedGeneration(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "movie.mkv"), []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := models.Library{ID: "lib", Type: models.LibraryMovies, Folders: []models.Folder{{RootID: "root", Path: "Movies", AbsolutePath: root}}}
	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	queue := asynq.NewClient(opt)
	defer queue.Close()
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	tracker := progress.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := New(&fakeAPI{libs: []models.Library{lib}}, images.NewStore(t.TempDir(), "ffmpeg", "test"), fakeProbe{}, tracker, queue, logger)
	p.SetTrickplay(true, t.TempDir(), "ffmpeg", 10, 320)

	payload, err := json.Marshal(models.ScanRequest{LibraryID: "lib", TrickplayOnly: true, TrackProgress: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.handleScan(context.Background(), asynq.NewTask(TypeScan, payload)); err != nil {
		t.Fatal(err)
	}
	scan, err := tracker.Get(context.Background(), "lib")
	if err != nil || scan == nil || scan.State != progress.StateIndexing || scan.Discovered != 1 {
		t.Fatalf("tracked trickplay scan progress = %+v, %v", scan, err)
	}
	tasks, err := inspector.ListPendingTasks(config.QueueTrickplay)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("trickplay tasks = %d, %v", len(tasks), err)
	}
	var task trickplay.Task
	if err := json.Unmarshal(tasks[0].Payload, &task); err != nil || !task.TrackProgress {
		t.Fatalf("trickplay task = %+v, %v", task, err)
	}
}

func TestPoolStartKeepsConcurrencyAppliedBeforeStartup(t *testing.T) {
	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	queue := asynq.NewClient(opt)
	defer queue.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := New(&fakeAPI{}, images.NewStore(t.TempDir(), "ffmpeg", "test"), fakeProbe{}, nil, queue, logger)
	pool := NewPool(opt, p, logger)
	defer pool.Shutdown(context.Background())

	pool.Apply(map[string]int{config.QueueScan: 2})
	defaults := make(map[string]int, len(config.WorkerQueues))
	for _, workerQueue := range config.WorkerQueues {
		defaults[workerQueue] = 4
	}
	if err := pool.Start(defaults); err != nil {
		t.Fatal(err)
	}
	concurrency := pool.Concurrency()
	if concurrency[config.QueueScan] != 2 {
		t.Fatalf("persisted scan concurrency = %d, want 2", concurrency[config.QueueScan])
	}
	if concurrency[config.QueueIdentify] != 4 {
		t.Fatalf("default identify concurrency = %d, want 4", concurrency[config.QueueIdentify])
	}
}

func TestTrickplayOnlyScanQueuesOnlyMissingOrStaleResults(t *testing.T) {
	root := t.TempDir()
	files := map[string][]byte{
		"current.mkv": []byte("current source"),
		"stale.mkv":   []byte("updated source"),
		"missing.mkv": []byte("new source"),
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(root, name), contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cache := t.TempDir()
	for name, sourceSize := range map[string]int64{"current.mkv": int64(len(files["current.mkv"])), "stale.mkv": 1} {
		relativePath := filepath.ToSlash(filepath.Join("Movies", name))
		indexDir := filepath.Join(cache, trickplay.Key("root", relativePath))
		if err := os.MkdirAll(indexDir, 0o755); err != nil {
			t.Fatal(err)
		}
		modifiedAt := "2000-01-01T00:00:00Z"
		if name == "current.mkv" {
			info, err := os.Stat(filepath.Join(root, name))
			if err != nil {
				t.Fatal(err)
			}
			modifiedAt = info.ModTime().UTC().Format(time.RFC3339)
		}
		index, err := json.Marshal(trickplay.Index{SourceSize: sourceSize, SourceModifiedAt: modifiedAt})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(indexDir, "index.json"), index, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	lib := models.Library{ID: "lib", Type: models.LibraryMovies, Folders: []models.Folder{{RootID: "root", Path: "Movies", AbsolutePath: root}}}
	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	queue := asynq.NewClient(opt)
	defer queue.Close()
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	tracker := progress.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := New(&fakeAPI{libs: []models.Library{lib}}, images.NewStore(t.TempDir(), "ffmpeg", "test"), fakeProbe{}, tracker, queue, logger)
	p.SetTrickplay(true, cache, "ffmpeg", 10, 320)
	payload, err := json.Marshal(models.ScanRequest{LibraryID: "lib", TrickplayOnly: true, TrackProgress: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.handleScan(context.Background(), asynq.NewTask(TypeScan, payload)); err != nil {
		t.Fatal(err)
	}

	tasks, err := inspector.ListPendingTasks(config.QueueTrickplay)
	if err != nil || len(tasks) != 2 {
		t.Fatalf("trickplay tasks = %d, %v", len(tasks), err)
	}
	queuedPaths := make(map[string]bool, len(tasks))
	for _, task := range tasks {
		var payload trickplay.Task
		if err := json.Unmarshal(task.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		queuedPaths[filepath.Base(payload.Path)] = true
	}
	if !queuedPaths["stale.mkv"] || !queuedPaths["missing.mkv"] || queuedPaths["current.mkv"] {
		t.Fatalf("queued trickplay paths = %v", queuedPaths)
	}
	scan, err := tracker.Get(context.Background(), "lib")
	if err != nil || scan == nil || scan.Discovered != 2 {
		t.Fatalf("tracked trickplay scan progress = %+v, %v", scan, err)
	}
}

func TestScanJobValidation(t *testing.T) {
	for _, test := range []struct {
		jobs []string
		want bool
	}{
		{jobs: []string{"images", "descriptions"}, want: true},
		{jobs: []string{"unknown"}},
		{jobs: []string{"images", "images"}},
	} {
		if err := validateScanJobs(test.jobs); (err == nil) != test.want {
			t.Errorf("validateScanJobs(%v) error = %v", test.jobs, err)
		}
	}
}

func TestEnqueueDeduplicatesSameItemAcrossQueuesAndPayloads(t *testing.T) {
	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	queue := asynq.NewClient(opt)
	defer queue.Close()
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	locks := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer locks.Close()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := New(&fakeAPI{}, images.NewStore(t.TempDir(), "ffmpeg", "test"), fakeProbe{}, nil, queue, logger)
	p.TaskLocks = locks

	work := models.Work{Item: models.Item{LibraryID: "lib", Type: models.ItemMovie, Path: "/movies/a.mkv"}}
	queued, err := p.Enqueue(context.Background(), config.QueueIdentify, TypeIdentify, work, maxRetry)
	if err != nil || !queued {
		t.Fatalf("first enqueue: queued=%v err=%v", queued, err)
	}

	changedPayload := work
	changedPayload.Force = true
	changedPayload.Jobs = []string{"images"}
	queued, err = p.Enqueue(context.Background(), config.QueueIdentify, TypeIdentify, changedPayload, maxRetry)
	if err != nil || queued {
		t.Fatalf("duplicate payload enqueue: queued=%v err=%v", queued, err)
	}

	priorityPayload := work
	priorityPayload.Priority = true
	queued, err = p.Enqueue(context.Background(), config.QueueIdentify, TypeIdentify, priorityPayload, maxRetry)
	if err != nil || queued {
		t.Fatalf("duplicate priority enqueue: queued=%v err=%v", queued, err)
	}

	differentItem := work
	differentItem.Item.Path = "/movies/b.mkv"
	queued, err = p.Enqueue(context.Background(), config.QueueIdentify, TypeIdentify, differentItem, maxRetry)
	if err != nil || !queued {
		t.Fatalf("different item enqueue: queued=%v err=%v", queued, err)
	}

	differentStage, err := p.Enqueue(context.Background(), config.QueueMetadata, TypeMetadata, work, maxRetry)
	if err != nil || !differentStage {
		t.Fatalf("different stage enqueue: queued=%v err=%v", differentStage, err)
	}

	tasks, err := inspector.ListPendingTasks(config.QueueIdentify, asynq.PageSize(10))
	if err != nil || len(tasks) != 2 {
		t.Fatalf("identify pending tasks = %d, err=%v", len(tasks), err)
	}
	var first *asynq.TaskInfo
	for _, task := range tasks {
		if task.Payload == nil {
			continue
		}
		var got models.Work
		if json.Unmarshal(task.Payload, &got) == nil && got.Item.Path == work.Item.Path {
			first = task
			break
		}
	}
	if first == nil {
		t.Fatal("original task not found")
	}
	if first.Headers[taskLockHeader] == "" {
		t.Fatalf("original task has no lock header: %#v", first.Headers)
	}
	wrapped := p.withTaskLog(config.QueueIdentify, func(context.Context, *asynq.Task) error { return nil })
	if err := wrapped(context.Background(), asynq.NewTaskWithHeaders(first.Type, first.Payload, first.Headers)); err != nil {
		t.Fatalf("complete task wrapper: %v", err)
	}
	if err := inspector.DeleteTask(config.QueueIdentify, first.ID); err != nil {
		t.Fatalf("remove completed task: %v", err)
	}
	queued, err = p.Enqueue(context.Background(), config.QueueIdentify, TypeIdentify, work, maxRetry)
	if err != nil || !queued {
		t.Fatalf("enqueue after completion: queued=%v err=%v", queued, err)
	}
}

func TestTaskSubjectFindsTheFileForEveryTaskType(t *testing.T) {
	cases := map[string]struct{ typ, payload, lib, path string }{
		"trickplay": {TypeTrickplay, `{"libraryId":"L","path":"/m/a.mkv","rootId":"r","relativePath":"a.mkv"}`, "L", "/m/a.mkv"},
		"themes":    {TypeThemes, `{"libraryId":"L","path":"/m/show","itemType":"series"}`, "L", "/m/show"},
		"work":      {TypeProbe, `{"item":{"libraryId":"L","type":"movie","path":"/m/b.mkv"}}`, "L", "/m/b.mkv"},
		"scan":      {TypeScan, `{"libraryId":"L"}`, "L", ""},
	}
	for name, c := range cases {
		lib, _, path := TaskSubject(c.typ, []byte(c.payload))
		if lib != c.lib || path != c.path {
			t.Errorf("%s: got %q %q, want %q %q", name, lib, path, c.lib, c.path)
		}
	}
}

func TestPriorityRefreshQueuesManualIDsAheadAtEveryStage(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "movie.mkv")
	if err := os.WriteFile(path, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	lib := models.Library{ID: "lib", Type: models.LibraryMovies, Folders: []models.Folder{{RootID: "root", AbsolutePath: root}}}
	api := &fakeAPI{libs: []models.Library{lib}}
	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	queue := asynq.NewClient(opt)
	defer queue.Close()
	inspector := asynq.NewInspector(opt)
	defer inspector.Close()
	tracker := progress.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p := New(api, images.NewStore(t.TempDir(), "ffmpeg", "test"), fakeProbe{}, tracker, queue, logger)

	queued, err := p.StartRefresh(context.Background(), models.RefreshRequest{
		LibraryID: "lib", Path: path, ItemType: models.ItemMovie, ProviderIDs: map[string]string{"tmdb": "42"}, Priority: true,
	})
	if err != nil || !queued {
		t.Fatalf("start priority refresh: queued=%v err=%v", queued, err)
	}
	refreshTasks, err := inspector.ListPendingTasks(config.QueueRefresh+config.PriorityQueueSuffix, asynq.PageSize(10))
	if err != nil || len(refreshTasks) != 1 {
		t.Fatalf("priority refresh tasks = %d, err=%v", len(refreshTasks), err)
	}
	if err := p.handleRefresh(context.Background(), asynq.NewTask(TypeRefresh, refreshTasks[0].Payload)); err != nil {
		t.Fatalf("handle priority refresh: %v", err)
	}

	identifyTasks, err := inspector.ListPendingTasks(config.QueueIdentify+config.PriorityQueueSuffix, asynq.PageSize(10))
	if err != nil || len(identifyTasks) != 1 {
		t.Fatalf("priority identify tasks = %d, err=%v", len(identifyTasks), err)
	}
	work, err := decodeWork(asynq.NewTask(TypeIdentify, identifyTasks[0].Payload))
	if err != nil {
		t.Fatal(err)
	}
	if !work.Priority || work.Item.ProviderIDs["tmdb"] != "42" {
		t.Fatalf("priority work lost its settings: %+v", work)
	}
}
