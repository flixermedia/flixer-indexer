// Package watcher watches library folders and queues a scan when media is added, so new content
// shows up without anyone pressing "scan".
package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

// Scanner is the part of the pipeline the watcher needs.
type Scanner interface {
	Libraries(ctx context.Context) ([]models.Library, error)
	StartScan(ctx context.Context, req models.ScanRequest) (bool, error)
}

// Watcher turns filesystem changes into library scans.
type Watcher struct {
	Scanner Scanner
	Log     *slog.Logger
	// Debounce is how long a library must be quiet before it is scanned, so a large copy finishes first.
	Debounce time.Duration
	// Rescan scans every library at this interval regardless of events. Network shares (SMB/NFS) do not
	// report changes made by other machines, so this is the only way to notice them. Zero disables it.
	Rescan time.Duration
	// SyncEvery is how often the library list is re-read to pick up new libraries and folders.
	SyncEvery time.Duration

	fs       *fsnotify.Watcher
	dirs     map[string]string          // watched directory -> library id
	roots    map[string]map[string]bool // library id -> watched root folders
	pending  map[string]time.Time       // library id -> time of the last change
	lastScan map[string]time.Time
	limitHit bool
}

// partialSuffixes mark files other programs are still writing.
var partialSuffixes = []string{".part", ".partial", ".tmp", ".temp", ".crdownload", ".!qb", ".download", ".filepart"}

// Run blocks until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	var err error
	if w.fs, err = fsnotify.NewWatcher(); err != nil {
		return err
	}
	defer w.fs.Close()
	w.dirs = map[string]string{}
	w.roots = map[string]map[string]bool{}
	w.pending = map[string]time.Time{}
	w.lastScan = map[string]time.Time{}
	if w.Debounce <= 0 {
		w.Debounce = 30 * time.Second
	}
	if w.SyncEvery <= 0 {
		w.SyncEvery = time.Minute
	}

	w.sync(ctx)
	syncTick := time.NewTicker(w.SyncEvery)
	defer syncTick.Stop()
	tick := time.NewTicker(max(w.Debounce/4, 10*time.Millisecond))
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.fs.Events:
			if !ok {
				return nil
			}
			w.handle(ev)
		case err, ok := <-w.fs.Errors:
			if !ok {
				return nil
			}
			w.Log.Warn("folder watcher error", "error", err)
		case <-syncTick.C:
			w.sync(ctx)
		case <-tick.C:
			w.flush(ctx)
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event) {
	if ev.Op == fsnotify.Chmod || ignored(ev.Name) {
		return
	}
	lib, ok := w.dirs[filepath.Dir(ev.Name)]
	if !ok {
		return
	}
	// Windows reports a Write on a folder whenever its contents change; the child event covers it.
	if ev.Op == fsnotify.Write {
		if _, isDir := w.dirs[ev.Name]; isDir {
			return
		}
	}
	if ev.Has(fsnotify.Create) {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			w.addTree(ev.Name, lib)
		}
	}
	w.pending[lib] = time.Now()
}

// flush scans libraries that have been quiet for the debounce period, plus those due a periodic rescan.
func (w *Watcher) flush(ctx context.Context) {
	now := time.Now()
	for lib, last := range w.pending {
		if now.Sub(last) >= w.Debounce {
			delete(w.pending, lib)
			w.scan(ctx, lib, "files changed")
		}
	}
	if w.Rescan > 0 {
		for lib, last := range w.lastScan {
			if now.Sub(last) >= w.Rescan {
				w.scan(ctx, lib, "periodic rescan")
			}
		}
	}
}

func (w *Watcher) scan(ctx context.Context, lib, reason string) {
	w.lastScan[lib] = time.Now()
	queued, err := w.Scanner.StartScan(ctx, models.ScanRequest{LibraryID: lib})
	if err != nil {
		w.Log.Warn("could not queue automatic scan", "library", lib, "error", err)
		return
	}
	if queued {
		w.Log.Info("automatic scan queued", "library", lib, "reason", reason)
	}
}

// sync reconciles the watched folders with flixer's libraries.
func (w *Watcher) sync(ctx context.Context) {
	libs, err := w.Scanner.Libraries(ctx)
	if err != nil {
		w.Log.Warn("folder watcher could not list libraries", "error", err)
		return
	}
	want := map[string]map[string]bool{}
	for _, lib := range libs {
		want[lib.ID] = map[string]bool{}
		for _, folder := range lib.Folders {
			if folder.AbsolutePath != "" {
				want[lib.ID][filepath.Clean(folder.AbsolutePath)] = true
			}
		}
	}
	for id, roots := range w.roots {
		for root := range roots {
			if !want[id][root] {
				w.drop(root)
				delete(roots, root)
			}
		}
		if _, ok := want[id]; !ok {
			delete(w.roots, id)
			delete(w.lastScan, id)
			delete(w.pending, id)
		}
	}
	for id, roots := range want {
		if _, ok := w.lastScan[id]; !ok {
			w.lastScan[id] = time.Now()
		}
		for root := range roots {
			if w.roots[id][root] {
				continue
			}
			if info, err := os.Stat(root); err != nil || !info.IsDir() {
				continue // not mounted yet; tried again on the next sync
			}
			if w.roots[id] == nil {
				w.roots[id] = map[string]bool{}
			}
			w.roots[id][root] = true
			count := w.addTree(root, id)
			w.Log.Info("watching library folder", "library", id, "folder", root, "directories", count)
		}
	}
}

// addTree watches dir and every directory beneath it.
func (w *Watcher) addTree(dir, lib string) int {
	count := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if _, ok := w.dirs[path]; ok {
			return nil
		}
		if err := w.fs.Add(path); err != nil {
			if !w.limitHit {
				w.limitHit = true
				w.Log.Warn("could not watch every folder (raise fs.inotify.max_user_watches); set WATCH_RESCAN_INTERVAL to cover the rest", "folder", path, "error", err)
			}
			return filepath.SkipDir
		}
		w.dirs[path] = lib
		count++
		return nil
	})
	return count
}

func (w *Watcher) drop(root string) {
	prefix := root + string(filepath.Separator)
	for dir := range w.dirs {
		if dir == root || strings.HasPrefix(dir, prefix) {
			_ = w.fs.Remove(dir)
			delete(w.dirs, dir)
		}
	}
}

func ignored(path string) bool {
	name := strings.ToLower(filepath.Base(path))
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, suffix := range partialSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}
