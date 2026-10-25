package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/flixermedia/flixer-indexer/internal/auth"
	"github.com/flixermedia/flixer-indexer/internal/flixerclient"
	"github.com/flixermedia/flixer-indexer/internal/images"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/pipeline"
	"github.com/flixermedia/flixer-indexer/internal/probe"
	"github.com/flixermedia/flixer-indexer/internal/progress"
	"github.com/flixermedia/flixer-indexer/internal/tasklog"
)

type noProbe struct{}

func (noProbe) Probe(context.Context, string) (*probe.Result, error) { return &probe.Result{}, nil }

var secret = []byte("test-secret")

func newServer(t *testing.T) http.Handler {
	t.Helper()
	mr := miniredis.RunT(t)
	opt := asynq.RedisClientOpt{Addr: mr.Addr()}
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	client := asynq.NewClient(opt)
	t.Cleanup(func() { client.Close() })
	insp := asynq.NewInspector(opt)
	t.Cleanup(func() { insp.Close() })

	stub := flixerclientStub(t)
	log := slog.New(slog.NewTextHandler(testWriter{t}, nil))
	tracker := progress.New(rdb)
	p := pipeline.New(stub, images.NewStore(t.TempDir(), "ffmpeg", "t"), noProbe{}, tracker, client, log)
	p.TaskLogs = tasklog.New(rdb)
	return (&Server{Secret: secret, Pipeline: p, Inspector: insp, Progress: tracker, Redis: rdb, Log: log}).Handler()
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(b []byte) (int, error) { w.t.Log(string(b)); return len(b), nil }

func flixerclientStub(t *testing.T) flixerclient.API {
	t.Helper()
	dir := t.TempDir()
	file := dir + "/libs.json"
	data, _ := json.Marshal(map[string]any{"libraries": []models.Library{{ID: "lib1", Type: models.LibraryMovies, Folders: []models.Folder{{AbsolutePath: dir}}}}})
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return flixerclient.NewStub(file, dir+"/out.jsonl", models.Settings{})
}

func do(h http.Handler, method, path string, payload any, signed bool) *httptest.ResponseRecorder {
	var body []byte
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if signed {
		auth.Sign(secret, req, body, time.Now())
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthRequired(t *testing.T) {
	h := newServer(t)
	if rec := do(h, "GET", "/internal/stats", nil, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned status = %d", rec.Code)
	}
	if rec := do(h, "GET", "/health", nil, false); rec.Code != http.StatusOK {
		t.Errorf("health status = %d", rec.Code)
	}
}

func TestScanAndInspect(t *testing.T) {
	h := newServer(t)

	if rec := do(h, "POST", "/internal/scan", models.ScanRequest{LibraryID: "nope"}, true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown library status = %d", rec.Code)
	}
	if rec := do(h, "POST", "/internal/scan", models.ScanRequest{LibraryID: "lib1"}, true); rec.Code != http.StatusAccepted {
		t.Fatalf("scan status = %d: %s", rec.Code, rec.Body)
	}
	rec := do(h, "POST", "/internal/scan", models.ScanRequest{LibraryID: "lib1"}, true)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"alreadyQueued":true`)) {
		t.Errorf("duplicate scan = %d %s", rec.Code, rec.Body)
	}

	rec = do(h, "GET", "/internal/tasks?queue=scan&state=pending", nil, true)
	var out struct {
		Tasks []struct{ Type, LibraryID string } `json:"tasks"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || len(out.Tasks) != 1 || out.Tasks[0].LibraryID != "lib1" {
		t.Errorf("tasks = %d %s", rec.Code, rec.Body)
	}

	if rec := do(h, "GET", "/internal/stats", nil, true); rec.Code != http.StatusOK {
		t.Errorf("stats = %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/internal/queues/scan/pause", nil, true); rec.Code != http.StatusOK {
		t.Errorf("pause = %d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/internal/concurrency", map[string]int{"scan": 2}, true); rec.Code != http.StatusConflict {
		t.Errorf("concurrency without pool = %d", rec.Code)
	}
	if rec := do(h, "GET", "/internal/tasks?queue=bogus", nil, true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad queue = %d", rec.Code)
	}
}

func TestTaskLogsEndpoint(t *testing.T) {
	h := newServer(t)
	if rec := do(h, http.MethodGet, "/internal/tasks/nope/abc/logs", nil, true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown queue = %d", rec.Code)
	}
	rec := do(h, http.MethodGet, "/internal/tasks/probe/abc/logs?offset=0", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("logs = %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"entries":[]`) || !strings.Contains(rec.Body.String(), `"total":0`) {
		t.Fatalf("unexpected body %s", rec.Body)
	}
	if rec := do(h, http.MethodGet, "/internal/tasks/probe/abc/logs", nil, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned = %d", rec.Code)
	}
}
