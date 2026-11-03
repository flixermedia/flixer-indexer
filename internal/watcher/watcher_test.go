package watcher

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

type fakeScanner struct {
	mu      sync.Mutex
	libs    []models.Library
	scans   []models.ScanRequest
	changes []models.PathChangeRequest
}

func (f *fakeScanner) Libraries(context.Context) ([]models.Library, error) { return f.libs, nil }
func (f *fakeScanner) StartScan(_ context.Context, r models.ScanRequest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans = append(f.scans, r)
	return true, nil
}
func (f *fakeScanner) StartChange(_ context.Context, r models.PathChangeRequest) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes = append(f.changes, r)
	return true, nil
}
func (f *fakeScanner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.scans) + len(f.changes)
}
func (f *fakeScanner) requests() []models.ScanRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]models.ScanRequest(nil), f.scans...)
}
func (f *fakeScanner) changeRequests() []models.PathChangeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]models.PathChangeRequest(nil), f.changes...)
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
	requests := sc.changeRequests()
	if len(requests[0].Paths) != 1 || requests[0].Paths[0] == root {
		t.Fatalf("scan paths = %v, want only the newly added subtree", requests[0].Paths)
	}

	// Hidden and partial files never trigger a scan.
	os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "movie.mkv.part"), []byte("x"), 0o644)
	time.Sleep(600 * time.Millisecond)
	if n := sc.count(); n != 1 {
		t.Fatalf("scans after ignored files = %d, want 1", n)
	}
}

func TestChangedFilesQueueOnlyTheirPaths(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.mkv")
	second := filepath.Join(root, "second.mkv")
	for _, path := range []string{first, second} {
		if err := os.WriteFile(path, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sc := &fakeScanner{}
	w := &Watcher{
		Scanner:  sc,
		Log:      slog.Default(),
		Debounce: time.Second,
		dirs:     map[string]string{root: "lib"},
		pending:  map[string]*pendingScan{},
		lastScan: map[string]time.Time{},
	}
	for _, path := range []string{first, second} {
		w.handle(fsnotify.Event{Name: path, Op: fsnotify.Write})
	}
	w.pending["lib"].changedAt = time.Now().Add(-2 * time.Second)
	w.flush(context.Background())

	requests := sc.changeRequests()
	if len(requests) != 1 {
		t.Fatalf("change requests = %d, want 1", len(requests))
	}
	if len(requests[0].Paths) != 2 || requests[0].Paths[0] != first || requests[0].Paths[1] != second {
		t.Fatalf("scan paths = %v, want only changed files", requests[0].Paths)
	}
}

func TestSidecarsTargetTheirFolderAndUnknownFilesAreIgnored(t *testing.T) {
	root := t.TempDir()
	path, ok := scanTarget(filepath.Join(root, "movie.nfo"), false)
	if !ok || path != root {
		t.Fatalf("sidecar target = %q, %v; want containing folder %q", path, ok, root)
	}
	if path, ok := scanTarget(filepath.Join(root, "notes.txt"), false); ok {
		t.Fatalf("unrelated file target = %q; want ignored", path)
	}
}

func TestCompactPathsRemovesCoveredTargets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Movies")
	got := compactPaths([]string{
		filepath.Join(root, "Show", "Season 1", "episode.mkv"),
		filepath.Join(root, "Other.mkv"),
		filepath.Join(root, "Show"),
	})
	want := []string{filepath.Join(root, "Other.mkv"), filepath.Join(root, "Show")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("compacted paths = %v, want %v", got, want)
	}
}

func TestRemovedFilesQueueOnlyTheRemovedPath(t *testing.T) {
	root := t.TempDir()
	sc := &fakeScanner{}
	w := &Watcher{
		Scanner:  sc,
		Log:      slog.Default(),
		Debounce: time.Second,
		dirs:     map[string]string{root: "lib"},
		pending:  map[string]*pendingScan{},
		lastScan: map[string]time.Time{},
	}
	w.handle(fsnotify.Event{Name: filepath.Join(root, "removed.mkv"), Op: fsnotify.Remove})
	w.pending["lib"].changedAt = time.Now().Add(-2 * time.Second)
	w.flush(context.Background())

	requests := sc.changeRequests()
	want := filepath.Join(root, "removed.mkv")
	if len(requests) != 1 || len(requests[0].Removed) != 1 || requests[0].Removed[0] != (models.PathRemoval{Path: want}) {
		t.Fatalf("change requests = %+v, want only exact removed file %s", requests, want)
	}
	if len(sc.requests()) != 0 {
		t.Fatalf("removal triggered a scan: %+v", sc.requests())
	}
}

func TestRemovedDirectoryQueuesRecursiveRemoval(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Show")
	sc := &fakeScanner{}
	w := &Watcher{
		Scanner:  sc,
		Log:      slog.Default(),
		Debounce: time.Second,
		dirs:     map[string]string{root: "lib", dir: "lib", filepath.Join(dir, "Season 1"): "lib"},
		pending:  map[string]*pendingScan{},
		lastScan: map[string]time.Time{},
	}
	w.handle(fsnotify.Event{Name: dir, Op: fsnotify.Remove})
	w.pending["lib"].changedAt = time.Now().Add(-2 * time.Second)
	w.flush(context.Background())

	requests := sc.changeRequests()
	if len(requests) != 1 || len(requests[0].Removed) != 1 ||
		requests[0].Removed[0] != (models.PathRemoval{Path: dir, Recursive: true}) {
		t.Fatalf("change requests = %+v, want recursive removal of %s", requests, dir)
	}
}

func TestRenameDeletesOldPathAndScansNewPathInOneChange(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "Old Name (2000).mkv")
	newPath := filepath.Join(root, "New Name (2000).mkv")
	if err := os.WriteFile(newPath, []byte("video"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := &fakeScanner{}
	w := &Watcher{
		Scanner:  sc,
		Log:      slog.Default(),
		Debounce: time.Second,
		dirs:     map[string]string{root: "lib"},
		pending:  map[string]*pendingScan{},
		lastScan: map[string]time.Time{},
	}
	w.handle(fsnotify.Event{Name: oldPath, Op: fsnotify.Rename})
	w.handle(fsnotify.Event{Name: newPath, Op: fsnotify.Create})
	w.pending["lib"].changedAt = time.Now().Add(-2 * time.Second)
	w.flush(context.Background())

	requests := sc.changeRequests()
	if len(requests) != 1 || len(requests[0].Removed) != 1 || len(requests[0].Paths) != 1 ||
		requests[0].Removed[0] != (models.PathRemoval{Path: oldPath}) || requests[0].Paths[0] != newPath {
		t.Fatalf("rename change = %+v, want removal at %s and scan at %s", requests, oldPath, newPath)
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
	if paths := sc.requests()[0].Paths; len(paths) != 0 {
		t.Fatalf("periodic rescan paths = %v, want full-library scan", paths)
	}
}
