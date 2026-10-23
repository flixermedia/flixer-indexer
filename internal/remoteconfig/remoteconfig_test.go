package remoteconfig

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/auth"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func TestWaitRetriesUntilConfigured(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path || auth.Verify(secret, r, nil, time.Now()) != nil {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		if calls.Add(1) < 2 {
			w.Write([]byte(`{"version":1,"redis":null}`))
			return
		}
		w.Write([]byte(`{"version":2,"redis":{"addr":"redis:6379","username":"","password":"pw","db":3,"tls":true}}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cfg, err := Wait(ctx, srv.URL, secret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Redis.Addr != "redis:6379" || cfg.Redis.Password != "pw" || cfg.Redis.DB != 3 || !cfg.Redis.TLS || cfg.Version != 2 {
		t.Fatalf("unexpected config %+v", cfg)
	}
}

func TestFetchRejectsWrongSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth.Verify(secret, r, nil, time.Now()) != nil {
			http.Error(w, "no", http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL, []byte("another-secret-another-secret-123456")); err == nil {
		t.Fatal("expected an error")
	}
}
