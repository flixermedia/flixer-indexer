package api

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flixermedia/flixer-indexer/internal/subtitles"
)

func TestSubtitleFailureDistinguishesProviderErrors(t *testing.T) {
	tests := []struct {
		err     error
		status  int
		message string
	}{
		{subtitles.ErrCredentials, http.StatusFailedDependency, "valid OpenSubtitles username and password"},
		{subtitles.ErrQuota, http.StatusTooManyRequests, "quota reached"},
		{subtitles.ErrNotConfigured, http.StatusServiceUnavailable, "not configured"},
		{errors.New("private upstream error"), http.StatusBadGateway, "OpenSubtitles request failed"},
	}
	for _, test := range tests {
		t.Run(test.message, func(t *testing.T) {
			var logs bytes.Buffer
			server := &Server{Log: slog.New(slog.NewTextHandler(&logs, nil))}
			rec := httptest.NewRecorder()
			server.subtitleFailure(rec, test.err)
			if rec.Code != test.status || !strings.Contains(rec.Body.String(), test.message) {
				t.Fatalf("response = %d %s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "private upstream error") {
				t.Fatal("provider details leaked to response")
			}
			if test.status == http.StatusBadGateway && !strings.Contains(logs.String(), "private upstream error") {
				t.Fatal("upstream failure was not logged")
			}
		})
	}
}
