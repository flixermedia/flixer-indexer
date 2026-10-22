// Package api exposes the indexer's internal HTTP API. Everything except
// /health requires a request signed with the shared secret.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/flixermedia/flixer-indexer/internal/auth"
	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/pipeline"
	"github.com/flixermedia/flixer-indexer/internal/progress"
	"github.com/flixermedia/flixer-indexer/internal/settings"
)

const maxBody = 1 << 20

// Server serves the internal API.
type Server struct {
	Secret    []byte
	Pipeline  *pipeline.Pipeline
	Inspector *asynq.Inspector
	Progress  *progress.Tracker
	Redis     redis.UniversalClient
	// Pool and Reloader are nil when this process only serves the API.
	Pool     *pipeline.Pool
	Reloader *settings.Reloader
	Log      *slog.Logger
	Now      func() time.Time
	// CachePath is where downloaded artwork is cached; images there may be served.
	CachePath string
}

// Handler returns the routed, authenticated handler.
func (s *Server) Handler() http.Handler {
	if s.Now == nil {
		s.Now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)

	signed := http.NewServeMux()
	signed.HandleFunc("POST /internal/scan", s.scan)
	signed.HandleFunc("POST /internal/refresh", s.refresh)
	signed.HandleFunc("GET /internal/scans", s.scans)
	signed.HandleFunc("GET /internal/stats", s.stats)
	signed.HandleFunc("GET /internal/tasks", s.tasks)
	signed.HandleFunc("POST /internal/tasks/{queue}/{id}/cancel", s.cancelTask)
	signed.HandleFunc("POST /internal/tasks/{queue}/{id}/retry", s.retryTask)
	signed.HandleFunc("POST /internal/queues/{queue}/pause", s.pause(true))
	signed.HandleFunc("POST /internal/queues/{queue}/resume", s.pause(false))
	signed.HandleFunc("GET /internal/concurrency", s.getConcurrency)
	signed.HandleFunc("POST /internal/concurrency", s.setConcurrency)
	signed.HandleFunc("POST /internal/image", s.image)
	signed.HandleFunc("POST /internal/subtitles/list", s.subtitleList)
	signed.HandleFunc("POST /internal/subtitles/search", s.subtitleSearch)
	signed.HandleFunc("POST /internal/subtitles/download", s.subtitleDownload)
	signed.HandleFunc("POST /internal/subtitles/delete", s.subtitleDelete)
	signed.HandleFunc("POST /internal/settings/reload", s.reload)
	mux.Handle("/internal/", s.authenticate(signed))
	return mux
}

type ctxKey struct{}

func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		if err := auth.Verify(s.Secret, r, raw, s.Now()); err != nil {
			s.Log.Warn("rejected request", "path", r.URL.Path, "error", err)
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, raw)))
	})
}

func body(r *http.Request) []byte {
	b, _ := r.Context().Value(ctxKey{}).([]byte)
	return b
}

func decode(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.Unmarshal(body(r), into); err != nil {
		writeError(w, http.StatusBadRequest, "request body must be valid JSON")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Redis.Ping(ctx).Err(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "redis": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "redis": true})
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	var req models.ScanRequest
	if !decode(w, r, &req) {
		return
	}
	if req.LibraryID == "" {
		writeError(w, http.StatusBadRequest, "libraryId is required")
		return
	}
	if _, err := s.Pipeline.Library(r.Context(), req.LibraryID); err != nil {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	queued, err := s.Pipeline.StartScan(r.Context(), req)
	if err != nil {
		s.Log.Error("enqueue scan failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not queue scan")
		return
	}
	status := http.StatusAccepted
	if !queued {
		status = http.StatusOK
	}
	writeJSON(w, status, map[string]any{"libraryId": req.LibraryID, "queued": queued, "alreadyQueued": !queued})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var req models.RefreshRequest
	if !decode(w, r, &req) {
		return
	}
	valid := []string{models.ItemMovie, models.ItemSeries, models.ItemSeason, models.ItemEpisode, models.ItemAlbum, models.ItemTrack, models.ItemVideo}
	if req.LibraryID == "" || req.Path == "" || !slices.Contains(valid, req.ItemType) {
		writeError(w, http.StatusBadRequest, "libraryId, path and a valid itemType are required")
		return
	}
	if _, err := s.Pipeline.Library(r.Context(), req.LibraryID); err != nil {
		writeError(w, http.StatusNotFound, "library not found")
		return
	}
	queued, err := s.Pipeline.StartRefresh(r.Context(), req)
	if err != nil {
		s.Log.Error("enqueue refresh failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not queue refresh")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"queued": queued, "alreadyQueued": !queued})
}

func (s *Server) scans(w http.ResponseWriter, r *http.Request) {
	list, err := s.Progress.All(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not read scans")
		return
	}
	if list == nil {
		list = []progress.Scan{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"scans": list})
}

type queueStats struct {
	Queue     string `json:"queue"`
	Pending   int    `json:"pending"`
	Active    int    `json:"active"`
	Scheduled int    `json:"scheduled"`
	Retry     int    `json:"retry"`
	Archived  int    `json:"archived"`
	Completed int    `json:"completed"`
	Processed int    `json:"processed"`
	Failed    int    `json:"failed"`
	Paused    bool   `json:"paused"`
	LatencyMs int64  `json:"latencyMs"`
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	queues := make([]queueStats, 0, len(config.Queues))
	for _, q := range config.Queues {
		info, err := s.Inspector.GetQueueInfo(q)
		if err != nil {
			// A queue that has never held a task does not exist in Redis yet.
			queues = append(queues, queueStats{Queue: q})
			continue
		}
		queues = append(queues, queueStats{
			Queue: q, Pending: info.Pending, Active: info.Active, Scheduled: info.Scheduled, Retry: info.Retry,
			Archived: info.Archived, Completed: info.Completed, Processed: info.Processed, Failed: info.Failed,
			Paused: info.Paused, LatencyMs: info.Latency.Milliseconds(),
		})
	}
	scans, _ := s.Progress.All(r.Context())
	if scans == nil {
		scans = []progress.Scan{}
	}
	out := map[string]any{"queues": queues, "scans": scans}
	if reg := s.Pipeline.Registry(); reg != nil {
		out["providers"] = reg.Stats()
		out["enabledProviders"] = reg.Names()
	}
	if s.Pool != nil {
		out["concurrency"] = s.Pool.Concurrency()
	}
	writeJSON(w, http.StatusOK, out)
}

type taskView struct {
	ID         string `json:"id"`
	Queue      string `json:"queue"`
	Type       string `json:"type"`
	State      string `json:"state"`
	Retried    int    `json:"retried"`
	MaxRetry   int    `json:"maxRetry"`
	LastError  string `json:"lastError,omitempty"`
	LastFailed string `json:"lastFailedAt,omitempty"`
	LibraryID  string `json:"libraryId,omitempty"`
	ItemType   string `json:"itemType,omitempty"`
	Path       string `json:"path,omitempty"`
}

func view(info *asynq.TaskInfo) taskView {
	v := taskView{
		ID: info.ID, Queue: info.Queue, Type: info.Type, State: info.State.String(),
		Retried: info.Retried, MaxRetry: info.MaxRetry, LastError: info.LastErr,
	}
	if !info.LastFailedAt.IsZero() {
		v.LastFailed = info.LastFailedAt.UTC().Format(time.RFC3339)
	}
	switch info.Type {
	case pipeline.TypeScan:
		var req models.ScanRequest
		if json.Unmarshal(info.Payload, &req) == nil {
			v.LibraryID = req.LibraryID
		}
	case pipeline.TypeRefresh:
		var req models.RefreshRequest
		if json.Unmarshal(info.Payload, &req) == nil {
			v.LibraryID, v.ItemType, v.Path = req.LibraryID, req.ItemType, req.Path
		}
	default:
		var w models.Work
		if json.Unmarshal(info.Payload, &w) == nil {
			v.LibraryID, v.ItemType, v.Path = w.Item.LibraryID, w.Item.Type, w.Item.Path
		}
	}
	return v
}

func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	queue := q.Get("queue")
	if !slices.Contains(config.Queues, queue) {
		writeError(w, http.StatusBadRequest, "queue must be one of "+strings.Join(config.Queues, ", "))
		return
	}
	state := q.Get("state")
	if state == "" {
		state = "pending"
	}
	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("size"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 200 {
		size = 50
	}
	opts := []asynq.ListOption{asynq.Page(page), asynq.PageSize(size)}

	var list []*asynq.TaskInfo
	var err error
	switch state {
	case "pending":
		list, err = s.Inspector.ListPendingTasks(queue, opts...)
	case "active":
		list, err = s.Inspector.ListActiveTasks(queue, opts...)
	case "scheduled":
		list, err = s.Inspector.ListScheduledTasks(queue, opts...)
	case "retry":
		list, err = s.Inspector.ListRetryTasks(queue, opts...)
	case "archived":
		list, err = s.Inspector.ListArchivedTasks(queue, opts...)
	case "completed":
		list, err = s.Inspector.ListCompletedTasks(queue, opts...)
	default:
		writeError(w, http.StatusBadRequest, "state must be pending, active, scheduled, retry, archived or completed")
		return
	}
	if err != nil {
		if errors.Is(err, asynq.ErrQueueNotFound) {
			list = nil
		} else {
			writeError(w, http.StatusInternalServerError, "could not list tasks")
			return
		}
	}
	out := make([]taskView, 0, len(list))
	for _, t := range list {
		out = append(out, view(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"queue": queue, "state": state, "page": page, "tasks": out})
}

func (s *Server) cancelTask(w http.ResponseWriter, r *http.Request) {
	queue, id := r.PathValue("queue"), r.PathValue("id")
	info, err := s.Inspector.GetTaskInfo(queue, id)
	if err != nil {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}
	if info.State == asynq.TaskStateActive {
		err = s.Inspector.CancelProcessing(id)
	} else {
		err = s.Inspector.DeleteTask(queue, id)
	}
	if err != nil {
		writeError(w, http.StatusConflict, "task could not be cancelled")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": id})
}

func (s *Server) retryTask(w http.ResponseWriter, r *http.Request) {
	queue, id := r.PathValue("queue"), r.PathValue("id")
	if err := s.Inspector.RunTask(queue, id); err != nil {
		if errors.Is(err, asynq.ErrTaskNotFound) || errors.Is(err, asynq.ErrQueueNotFound) {
			writeError(w, http.StatusNotFound, "task not found")
			return
		}
		writeError(w, http.StatusConflict, "only archived, scheduled or retrying tasks can be retried")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"retrying": id})
}

func (s *Server) pause(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		queue := r.PathValue("queue")
		if !slices.Contains(config.Queues, queue) {
			writeError(w, http.StatusNotFound, "unknown queue")
			return
		}
		var err error
		if paused {
			err = s.Inspector.PauseQueue(queue)
		} else {
			err = s.Inspector.UnpauseQueue(queue)
		}
		if err != nil && !errors.Is(err, asynq.ErrQueueNotFound) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"queue": queue, "paused": paused})
	}
}

func (s *Server) getConcurrency(w http.ResponseWriter, _ *http.Request) {
	if s.Pool == nil {
		writeError(w, http.StatusConflict, "this process does not run workers")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"concurrency": s.Pool.Concurrency(), "max": config.MaxConcurrency})
}

func (s *Server) setConcurrency(w http.ResponseWriter, r *http.Request) {
	if s.Pool == nil {
		writeError(w, http.StatusConflict, "this process does not run workers")
		return
	}
	var req map[string]int
	if !decode(w, r, &req) {
		return
	}
	for q, n := range req {
		if !slices.Contains(config.Queues, q) {
			writeError(w, http.StatusBadRequest, "unknown queue "+q)
			return
		}
		if n < 1 || n > config.MaxConcurrency {
			writeError(w, http.StatusBadRequest, q+" concurrency must be between 1 and "+strconv.Itoa(config.MaxConcurrency))
			return
		}
	}
	for q, n := range req {
		if err := s.Pool.SetConcurrency(q, n); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"concurrency": s.Pool.Concurrency()})
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	if s.Reloader == nil {
		writeError(w, http.StatusConflict, "this process does not load settings")
		return
	}
	if err := s.Reloader.Reload(r.Context()); err != nil {
		writeError(w, http.StatusBadGateway, "could not load settings from flixer")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reloaded": true})
}
