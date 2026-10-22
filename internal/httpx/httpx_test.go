package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRetriesOnRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c := New("test", "ua", 0)
	var out struct{ OK bool }
	if err := c.GetJSON(context.Background(), srv.URL, nil, &out); err != nil || !out.OK {
		t.Fatalf("out = %+v, err = %v", out, err)
	}
	if calls.Load() != 2 || c.Stats.RateLimited.Load() != 1 {
		t.Errorf("calls = %d, rateLimited = %d", calls.Load(), c.Stats.RateLimited.Load())
	}
}

func TestNotFoundAndKeyRedaction(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	c := New("test", "ua", 0)
	var out any
	if err := c.GetJSON(context.Background(), srv.URL+"/missing", nil, &out); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	err := c.GetJSON(context.Background(), srv.URL+"/secret?api_key=TOPSECRET", nil, &out)
	if err == nil || strings.Contains(err.Error(), "TOPSECRET") {
		t.Errorf("error should fail and not leak the key: %v", err)
	}
}
