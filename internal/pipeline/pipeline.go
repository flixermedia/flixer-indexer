// Package pipeline wires the indexing stages together as Asynq tasks:
// scan -> identify -> metadata -> images -> probe (which persists the item).
package pipeline

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/flixerclient"
	"github.com/flixermedia/flixer-indexer/internal/images"
	"github.com/flixermedia/flixer-indexer/internal/introdb"
	"github.com/flixermedia/flixer-indexer/internal/lyrics"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/probe"
	"github.com/flixermedia/flixer-indexer/internal/progress"
	"github.com/flixermedia/flixer-indexer/internal/providers/registry"
	"github.com/flixermedia/flixer-indexer/internal/tasklog"
	"github.com/flixermedia/flixer-indexer/internal/theintrodb"
	"github.com/flixermedia/flixer-indexer/internal/themes"
	"github.com/flixermedia/flixer-indexer/internal/trickplay"
)

// Task types, one per stage.
const (
	TypeScan         = "scan:library"
	TypeIdentify     = "identify:item"
	TypeMetadata     = "metadata:fetch"
	TypeImages       = "images:fetch"
	TypeProbe        = "probe:item"
	TypeRefresh      = "refresh:item"
	TypeTrickplay    = "trickplay:item"
	TypeThemes       = "themes:item"
	TypeLyrics       = "lyrics:item"
	TypeIntroMovie   = "intro:movie"
	TypeIntroEpisode = "intro:episode"
)

const (
	maxRetry    = 3
	uniqueFor   = 30 * time.Minute
	taskLockTTL = 24 * time.Hour
	libraryTTL  = 30 * time.Second
)

const taskLockHeader = "flixer-item-task-lock"

// Prober inspects media files; *probe.Prober is the production implementation.
type Prober interface {
	Probe(ctx context.Context, file string) (*probe.Result, error)
}

type IntroDBLookup interface {
	GetMedia(context.Context, models.IntroSyncCandidate) (map[string]models.IntroTimeRange, error)
}

// Pipeline holds everything the stage handlers need.
type Pipeline struct {
	Client          flixerclient.API
	IntroDB         IntroDBLookup
	IntroDBFallback IntroDBLookup
	Images          *images.Store
	Probe           Prober
	// Lyrics downloads missing .lrc files; nil keeps lyrics local-only.
	Lyrics *lyrics.Fetcher
	// LyricsDir is where downloaded lyrics are stored as <track id>.lrc.
	LyricsDir string
	Progress  *progress.Tracker
	Queue     *asynq.Client
	TaskLocks redis.UniversalClient
	Log       *slog.Logger
	// TaskLogs records each task's log output; nil disables it.
	TaskLogs         *tasklog.Store
	Trickplay        *trickplay.Generator
	TrickplayEnabled bool
	// SubtitleDir stores subtitles for videos whose media folder is read-only.
	SubtitleDir string

	// Themes downloads ThemerrDB theme songs; nil disables them.
	Themes                 *themes.Fetcher
	themesEnabled          atomic.Bool
	introDBEnabled         atomic.Bool
	introDBFallbackEnabled atomic.Bool
	introSyncWake          chan struct{}

	subs           atomic.Pointer[subtitleState]
	subtitleMisses sync.Map // video path -> time of the last fruitless search

	registry atomic.Pointer[registry.Registry]

	mu        sync.Mutex
	libs      map[string]models.Library
	libsFetch time.Time
}

// New builds a pipeline. The provider registry must be set before work runs.
func New(client flixerclient.API, store *images.Store, prober Prober, tracker *progress.Tracker, queue *asynq.Client, log *slog.Logger) *Pipeline {
	p := &Pipeline{Client: client, IntroDB: theintrodb.New(""), IntroDBFallback: introdb.New(""), Images: store, Probe: prober, Progress: tracker, Queue: queue, Log: log, introSyncWake: make(chan struct{}, 1)}
	p.introDBEnabled.Store(true)
	return p
}

// SetRegistry swaps the provider set; running tasks keep the one they started with.
func (p *Pipeline) SetRegistry(r *registry.Registry) { p.registry.Store(r) }

func (p *Pipeline) SetTheIntroDBEnabled(enabled bool) {
	wasEnabled := p.introDBEnabled.Swap(enabled)
	if enabled && !wasEnabled {
		p.wakeIntroBackfill()
	}
}

func (p *Pipeline) SetIntroDBEnabled(enabled bool) {
	wasEnabled := p.introDBFallbackEnabled.Swap(enabled)
	if enabled && !wasEnabled {
		p.wakeIntroBackfill()
	}
}

func (p *Pipeline) introSyncEnabled() bool {
	return p.introDBEnabled.Load() || p.introDBFallbackEnabled.Load()
}

func (p *Pipeline) wakeIntroBackfill() {
	select {
	case p.introSyncWake <- struct{}{}:
	default:
	}
}

func (p *Pipeline) SetTrickplay(enabled bool, directory, ffmpeg string, interval, width int) {
	p.TrickplayEnabled = enabled
	p.Trickplay = &trickplay.Generator{Directory: directory, FFmpeg: ffmpeg, Interval: interval, Width: width, Prober: p.Probe}
}

// Registry returns the provider set currently in use, or nil before the first load.
func (p *Pipeline) Registry() *registry.Registry { return p.registry.Load() }

func (p *Pipeline) providers(lib models.Library) *registry.Registry {
	r := p.registry.Load()
	if r == nil {
		return nil
	}
	if len(lib.Providers) > 0 {
		return r.Restrict(lib.Providers)
	}
	return r
}

// Library returns a library by id, refreshing the cached list when it is stale
// or the id is unknown.
func (p *Pipeline) Library(ctx context.Context, id string) (models.Library, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lib, ok := p.libs[id]; ok && time.Since(p.libsFetch) < libraryTTL {
		return lib, nil
	}
	list, err := p.Client.Libraries(ctx)
	if err != nil {
		if lib, ok := p.libs[id]; ok {
			return lib, nil
		}
		return models.Library{}, err
	}
	p.libs = make(map[string]models.Library, len(list))
	for _, l := range list {
		p.libs[l.ID] = l
	}
	p.libsFetch = time.Now()
	lib, ok := p.libs[id]
	if !ok {
		return lib, fmt.Errorf("library %s not found: %w", id, asynq.SkipRetry)
	}
	return lib, nil
}

// Libraries lists all libraries from flixer.
func (p *Pipeline) Libraries(ctx context.Context) ([]models.Library, error) {
	return p.Client.Libraries(ctx)
}

func queueTimeout(queue string) time.Duration {
	switch queue {
	case config.QueueScan:
		return 12 * time.Hour
	case config.QueueProbe:
		return 3 * time.Minute
	case config.QueueTrickplay:
		return 3 * time.Hour
	case config.QueueThemes:
		return 15 * time.Minute
	case config.QueueLyrics:
		return 3 * time.Minute
	default:
		return 5 * time.Minute
	}
}

// Enqueue adds a task. It reports false when an identical task is already queued.
func (p *Pipeline) Enqueue(ctx context.Context, queue, typ string, payload any, retries int) (bool, error) {
	if work, ok := payload.(models.Work); ok && work.Priority {
		if priorityQueue, supported := config.PriorityQueue(queue); supported {
			queue = priorityQueue
		}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	lockKey := taskItemLockKey(typ, data)
	locked := false
	lockToken := ""
	if lockKey != "" && p.TaskLocks != nil {
		lockToken, err = newTaskLockToken()
		if err != nil {
			return false, fmt.Errorf("create %s task lock token: %w", typ, err)
		}
		locked, err = p.TaskLocks.SetNX(ctx, lockKey, lockToken, taskLockTTL).Result()
		if err != nil {
			return false, fmt.Errorf("deduplicate %s task: %w", typ, err)
		}
		if !locked {
			return false, nil
		}
	}
	headers := map[string]string(nil)
	if lockToken != "" {
		headers = map[string]string{taskLockHeader: lockToken}
	}
	_, err = p.Queue.EnqueueContext(ctx, asynq.NewTaskWithHeaders(typ, data, headers),
		asynq.Queue(queue), asynq.MaxRetry(retries), asynq.Unique(uniqueFor),
		asynq.Timeout(queueTimeout(queue)), asynq.Retention(2*time.Hour))
	if errors.Is(err, asynq.ErrDuplicateTask) || errors.Is(err, asynq.ErrTaskIDConflict) {
		p.releaseTaskLock(lockKey, lockToken)
		return false, nil
	}
	if err != nil {
		p.releaseTaskLock(lockKey, lockToken)
	}
	return err == nil, err
}

func newTaskLockToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func taskItemLockKey(typ string, payload []byte) string {
	libraryID, itemType, path := TaskSubject(typ, payload)
	if typ == TypeScan {
		var request models.ScanRequest
		if json.Unmarshal(payload, &request) == nil {
			libraryID, itemType, path = request.LibraryID, "library", "library"
		}
	}
	if typ == TypeIntroMovie || typ == TypeIntroEpisode {
		var candidate models.IntroSyncCandidate
		if json.Unmarshal(payload, &candidate) == nil {
			libraryID, itemType, path = "intro", candidate.Type, candidate.ID
		}
	}
	if libraryID == "" || path == "" {
		return ""
	}
	if libraryID != "intro" {
		path = filepath.Clean(path)
	}
	sum := sha256.Sum256([]byte(typ + "\x00" + libraryID + "\x00" + itemType + "\x00" + path))
	return "flixer:indexer:item-task:" + hex.EncodeToString(sum[:])
}

func (p *Pipeline) releaseTaskLock(key, token string) {
	if key == "" || token == "" || p.TaskLocks == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	const compareAndDelete = `if redis.call("GET", KEYS[1]) == ARGV[1] then return redis.call("DEL", KEYS[1]) else return 0 end`
	if err := p.TaskLocks.Eval(ctx, compareAndDelete, []string{key}, token).Err(); err != nil {
		p.Log.Warn("could not release item task lock", "key", key, "error", err)
	}
}

func (p *Pipeline) releaseFinishedTaskLock(ctx context.Context, task *asynq.Task, taskErr error) {
	retried, hasRetryCount := asynq.GetRetryCount(ctx)
	limit, hasMaxRetry := asynq.GetMaxRetry(ctx)
	if taskErr == nil || errors.Is(taskErr, asynq.SkipRetry) || (hasRetryCount && hasMaxRetry && retried >= limit) {
		p.releaseTaskLock(taskItemLockKey(task.Type(), task.Payload()), task.Headers()[taskLockHeader])
	}
}

// StartScan queues a scan of one library.
func (p *Pipeline) StartScan(ctx context.Context, req models.ScanRequest) (bool, error) {
	return p.Enqueue(ctx, config.QueueScan, TypeScan, req, 0)
}

// StartRefresh queues a forced re-index of one item.
func (p *Pipeline) StartRefresh(ctx context.Context, req models.RefreshRequest) (bool, error) {
	queue := config.QueueRefresh
	if req.Priority {
		if priorityQueue, supported := config.PriorityQueue(queue); supported {
			queue = priorityQueue
		}
	}
	return p.Enqueue(ctx, queue, TypeRefresh, req, maxRetry)
}

// logger returns the logger for work running under ctx; inside a task it also records into that task's log.
func (p *Pipeline) logger(ctx context.Context) *slog.Logger {
	return tasklog.Logger(ctx, p.TaskLogs, p.Log)
}

// withTaskLog records the start and outcome of every attempt in the task's log.
func (p *Pipeline) withTaskLog(queue string, h asynq.HandlerFunc) asynq.HandlerFunc {
	return func(ctx context.Context, t *asynq.Task) error {
		id, ok := asynq.GetTaskID(ctx)
		if !ok || p.TaskLogs == nil {
			err := h(ctx, t)
			p.releaseFinishedTaskLock(ctx, t, err)
			return err
		}
		taskQueue := queue
		if priorityQueue, supported := config.PriorityQueue(queue); supported && isPriorityTask(t) {
			taskQueue = priorityQueue
		}
		ctx = tasklog.WithTask(ctx, taskQueue, id)
		retried, _ := asynq.GetRetryCount(ctx)
		lib, _, path := TaskSubject(t.Type(), t.Payload())
		args := []any{"type", t.Type(), "attempt", retried + 1}
		if path != "" {
			args = append(args, "path", path)
		}
		if lib != "" {
			if l, err := p.Library(ctx, lib); err == nil && l.Name != "" {
				args = append(args, "library", l.Name)
			}
		}
		p.logger(ctx).Info("attempt started", args...)
		err := h(ctx, t)
		p.releaseFinishedTaskLock(ctx, t, err)
		if err != nil {
			p.logger(ctx).Error("attempt failed", "error", err)
		} else {
			p.logger(ctx).Info("attempt finished")
		}
		return err
	}
}

func isPriorityTask(task *asynq.Task) bool {
	if task.Type() == TypeRefresh {
		var req models.RefreshRequest
		return json.Unmarshal(task.Payload(), &req) == nil && req.Priority
	}
	var work models.Work
	return json.Unmarshal(task.Payload(), &work) == nil && work.Priority
}

// Handlers maps each worker queue to the handler for its task type.
func (p *Pipeline) Handlers() map[string]map[string]asynq.HandlerFunc {
	handlers := p.handlers()
	for queue, byType := range handlers {
		for typ, h := range byType {
			byType[typ] = p.withTaskLog(queue, h)
		}
	}
	return handlers
}

func (p *Pipeline) handlers() map[string]map[string]asynq.HandlerFunc {
	return map[string]map[string]asynq.HandlerFunc{
		config.QueueScan:          {TypeScan: p.handleScan},
		config.QueueIdentify:      {TypeIdentify: p.handleIdentify},
		config.QueueMetadata:      {TypeMetadata: p.handleMetadata},
		config.QueueImages:        {TypeImages: p.handleImages},
		config.QueueProbe:         {TypeProbe: p.handleProbe},
		config.QueueRefresh:       {TypeRefresh: p.handleRefresh},
		config.QueueTrickplay:     {TypeTrickplay: p.handleTrickplay},
		config.QueueThemes:        {TypeThemes: p.handleThemes},
		config.QueueLyrics:        {TypeLyrics: p.handleLyrics},
		config.QueueIntroMovies:   {TypeIntroMovie: p.handleIntroMovie},
		config.QueueIntroEpisodes: {TypeIntroEpisode: p.handleIntroEpisode},
	}
}

// OnError is the Asynq error handler. When a task has used up its retries the
// item counts as failed in its scan's progress.
func (p *Pipeline) OnError(ctx context.Context, task *asynq.Task, err error) {
	retried, _ := asynq.GetRetryCount(ctx)
	limit, _ := asynq.GetMaxRetry(ctx)
	terminal := retried >= limit || errors.Is(err, asynq.SkipRetry)
	level := slog.LevelWarn
	if terminal {
		level = slog.LevelError
	}
	p.Log.Log(ctx, level, "task failed", "type", task.Type(), "retry", retried, "terminal", terminal, "error", err)
	if !terminal || task.Type() == TypeScan || task.Type() == TypeRefresh || task.Type() == TypeLyrics {
		return
	}
	if task.Type() == TypeTrickplay {
		var trickplayTask trickplay.Task
		if json.Unmarshal(task.Payload(), &trickplayTask) == nil && trickplayTask.TrackProgress {
			_ = p.Progress.Failed(context.WithoutCancel(ctx), trickplayTask.LibraryID)
		}
		return
	}
	var w models.Work
	if json.Unmarshal(task.Payload(), &w) == nil && !w.Untracked {
		_ = p.Progress.Failed(context.WithoutCancel(ctx), w.Item.LibraryID)
	}
}

// TaskSubject says which library and media file a queued task works on.
func TaskSubject(typ string, payload []byte) (libraryID, itemType, path string) {
	switch typ {
	case TypeScan:
		var req models.ScanRequest
		if json.Unmarshal(payload, &req) == nil {
			libraryID = req.LibraryID
		}
	case TypeRefresh:
		var req models.RefreshRequest
		if json.Unmarshal(payload, &req) == nil {
			libraryID, itemType, path = req.LibraryID, req.ItemType, req.Path
		}
	case TypeTrickplay, TypeThemes, TypeLyrics:
		var task struct {
			LibraryID string `json:"libraryId"`
			ItemType  string `json:"itemType"`
			Path      string `json:"path"`
		}
		if json.Unmarshal(payload, &task) == nil {
			libraryID, itemType, path = task.LibraryID, task.ItemType, task.Path
		}
	default:
		var w models.Work
		if json.Unmarshal(payload, &w) == nil {
			libraryID, itemType, path = w.Item.LibraryID, w.Item.Type, w.Item.Path
		}
	}
	return libraryID, itemType, path
}

func decodeWork(t *asynq.Task) (models.Work, error) {
	var w models.Work
	if err := json.Unmarshal(t.Payload(), &w); err != nil {
		return w, fmt.Errorf("bad payload: %w", asynq.SkipRetry)
	}
	return w, nil
}
