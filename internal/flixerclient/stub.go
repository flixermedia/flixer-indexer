package flixerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

// Stub is a stand-in for flixer used until flixer exposes the internal API.
// Libraries come from a JSON file shaped like the real response, and upserted
// items are appended to a JSON-lines file instead of a database.
type Stub struct {
	libraries string
	output    string
	settings  models.Settings

	mu     sync.Mutex
	nextID atomic.Int64
}

func NewStub(librariesFile, outputFile string, settings models.Settings) *Stub {
	return &Stub{libraries: librariesFile, output: outputFile, settings: settings}
}

func (s *Stub) Libraries(context.Context) ([]models.Library, error) {
	if s.libraries == "" {
		return nil, nil
	}
	data, err := os.ReadFile(s.libraries)
	if err != nil {
		return nil, fmt.Errorf("read stub libraries: %w", err)
	}
	var body struct {
		Libraries []models.Library `json:"libraries"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("parse stub libraries: %w", err)
	}
	for i := range body.Libraries {
		normalizeLibrary(&body.Libraries[i])
	}
	return body.Libraries, nil
}

func (s *Stub) Settings(context.Context) (models.Settings, error) { return s.settings, nil }

func (s *Stub) PruneLibrary(context.Context, string, string) (int, error) { return 0, nil }

func (s *Stub) UpsertItem(_ context.Context, item models.Item) (string, error) {
	line, err := json.Marshal(item)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.output), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(s.output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return "", err
	}
	return fmt.Sprintf("stub-%d", s.nextID.Add(1)), nil
}
