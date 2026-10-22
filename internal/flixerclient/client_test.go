package flixerclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/auth"
	"github.com/flixermedia/flixer-indexer/internal/models"
)

var secret = []byte("s3cret")

func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := auth.Verify(secret, r, body, time.Now()); err != nil {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL+"/", secret)
}

func TestLibrariesNormalizesStorage(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != librariesPath {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"libraries":[{"id":"a","type":"movies","imageStorage":"weird","folders":[]},{"id":"b","type":"music","imageStorage":"alongside","folders":[]}]}`))
	})
	libs, err := c.Libraries(context.Background())
	if err != nil || len(libs) != 2 {
		t.Fatalf("libs = %+v, %v", libs, err)
	}
	if libs[0].ImageStorage != models.StorageCache || libs[1].ImageStorage != models.StorageAlongside {
		t.Errorf("storage = %q, %q", libs[0].ImageStorage, libs[1].ImageStorage)
	}
}

func TestUpsertItem(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != itemsPath {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"item-1"}`))
	})
	id, err := c.UpsertItem(context.Background(), models.Item{Type: models.ItemMovie, Path: "/x.mkv"})
	if err != nil || id != "item-1" {
		t.Errorf("id = %q, %v", id, err)
	}
}

func TestErrorClassification(t *testing.T) {
	status := http.StatusBadRequest
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })

	if _, err := c.UpsertItem(context.Background(), models.Item{}); !errors.Is(err, ErrPermanent) {
		t.Errorf("400 should be permanent: %v", err)
	}
	status = http.StatusTooManyRequests
	if _, err := c.UpsertItem(context.Background(), models.Item{}); err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("429 should be retryable: %v", err)
	}
	status = http.StatusBadGateway
	if _, err := c.UpsertItem(context.Background(), models.Item{}); err == nil || errors.Is(err, ErrPermanent) {
		t.Errorf("502 should be retryable: %v", err)
	}
}

func TestStubWritesItems(t *testing.T) {
	dir := t.TempDir()
	stub := NewStub("", dir+"/out.jsonl", models.Settings{})
	if _, err := stub.UpsertItem(context.Background(), models.Item{Type: models.ItemMovie, Path: "/a"}); err != nil {
		t.Fatal(err)
	}
	if libs, err := stub.Libraries(context.Background()); err != nil || libs != nil {
		t.Errorf("libs = %v, %v", libs, err)
	}
}
