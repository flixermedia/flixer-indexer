package subtitles

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadReportsMissingCredentials(t *testing.T) {
	client := NewClient("key", "", "", "Flixer test")
	if _, err := client.Download(context.Background(), 123); !errors.Is(err, ErrCredentials) {
		t.Fatalf("expected credentials error, got %v", err)
	}
}

func TestDownloadClassifiesLoginFailures(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest, http.StatusInternalServerError, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/login" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := NewClient("key", "user", "password", "Flixer test")
			client.BaseURL = server.URL
			_, err := client.Download(context.Background(), 123)
			switch status {
			case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest:
				if !errors.Is(err, ErrCredentials) {
					t.Fatalf("expected credentials error, got %v", err)
				}
			case http.StatusTooManyRequests:
				if !errors.Is(err, ErrQuota) {
					t.Fatalf("expected quota error, got %v", err)
				}
			default:
				if err == nil || errors.Is(err, ErrCredentials) || errors.Is(err, ErrQuota) {
					t.Fatalf("expected provider error, got %v", err)
				}
			}
		})
	}
}

func TestDownloadUsesLoginTokenAndReturnsSubtitle(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
		case "/download":
			if r.Header.Get("Authorization") != "Bearer test-token" {
				t.Error("download request is missing the login token")
			}
			_, _ = fmt.Fprintf(w, `{"link":%q}`, server.URL+"/file")
		case "/file":
			_, _ = w.Write([]byte("subtitle text"))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewClient("key", "user", "password", "Flixer test")
	client.BaseURL = server.URL
	data, err := client.Download(context.Background(), 123)
	if err != nil || string(data) != "subtitle text" {
		t.Fatalf("download = %q, %v", data, err)
	}
}

func TestDownloadPreservesProviderFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			_, _ = w.Write([]byte(`{"token":"test-token"}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := NewClient("key", "user", "password", "Flixer test")
	client.BaseURL = server.URL
	_, err := client.Download(context.Background(), 123)
	if err == nil || errors.Is(err, ErrCredentials) || !strings.Contains(err.Error(), "status 503") {
		t.Fatalf("expected upstream status error, got %v", err)
	}
}
