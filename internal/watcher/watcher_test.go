package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

type fakeScanner struct {
	mu    sync.Mutex
	libs  []models.Library
	scans []string
}

func (f *fakeScanner) Libraries(context.Context) ([]models.Library, error) { return f.libs, nil }
func (f *fakeScanner) StartScan(_ context.Context, r models.ScanRequest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans = append(f.scans, r.LibraryID)
	return true, nil
}
func (f *fakeScanner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scans)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNewFilesInNewFoldersQueueOneScan(t *testing.T) {
	root := t.TempDir()
	sc := &fakeScanner{libs: []models.Library{{ID: "lib", Folders: []models.Folder{{AbsolutePath: root}}}}}
	w := &Watcher{Scanner: sc, Log: slog.Default(), Debounce: 200 * time.Millisecond, SyncEvery: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	time.Sleep(300 * time.Millisecond)

	// A new folder, then a file inside it, which is only noticed if the folder was watched on creation.
	dir := filepath.Join(root, "Movies", "Inception (2010)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "Inception (2010).mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "scan", func() bool { return sc.count() >= 1 })
	time.Sleep(500 * time.Millisecond)
	if n := sc.count(); n != 1 {
		t.Fatalf("scans = %d, want 1 (changes should be batched)", n)
	}

	// Hidden and partial files never trigger a scan.
	os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.mkv.part"), []byte("x"), 0o644)
	time.Sleep(600 * time.Millisecond)
	if n := sc.count(); n != 1 {
		t.Fatalf("scans after ignored files = %d, want 1", n)
	}
}

func TestPeriodicRescan(t *testing.T) {
	root := t.TempDir()
	sc := &fakeScanner{libs: []models.Library{{ID: "lib", Folders: []models.Folder{{AbsolutePath: root}}}}}
	w := &Watcher{Scanner: sc, Log: slog.Default(), Debounce: 40 * time.Millisecond, Rescan: 150 * time.Millisecond, SyncEvery: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)
	waitFor(t, "periodic rescan", func() bool { return sc.count() >= 1 })
}
