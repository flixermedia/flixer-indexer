// Package watcher watches library folders and queues a scan when media is added, so new content
// shows up without anyone pressing "scan".
package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/naming"
)

// Scanner is the part of the pipeline the watcher needs.
type Scanner interface {
	Libraries(ctx context.Context) ([]models.Library, error)
	StartScan(ctx context.Context, req models.ScanRequest) (bool, error)
	StartChange(ctx context.Context, req models.PathChangeRequest) (bool, error)
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
	pending  map[string]*pendingScan    // library id -> changed paths and last change
	lastScan map[string]time.Time
	limitHit bool
}

type pendingScan struct {
	paths     map[string]struct{}
	removed   map[string]bool
	changedAt time.Time
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
	w.pending = map[string]*pendingScan{}
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
	path := filepath.Clean(ev.Name)
	info, statErr := os.Stat(path)
	_, watchedDir := w.dirs[path]
	isDir := (statErr == nil && info.IsDir()) || watchedDir
	if ev.Has(fsnotify.Create) {
		if isDir {
			w.addTree(path, lib)
		}
	}
	if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		if isDir || naming.IsVideo(path) || naming.IsAudio(path) {
			pending := w.pendingFor(lib)
			pending.removed[path] = pending.removed[path] || isDir
			pending.changedAt = time.Now()
			if isDir {
				w.drop(path)
			}
		} else if target, relevant := scanTarget(path, false); relevant {
			pending := w.pendingFor(lib)
			pending.paths[target] = struct{}{}
			pending.changedAt = time.Now()
		}
		return
	}
	path, relevant := scanTarget(path, isDir)
	if !relevant {
		return
	}
	pending := w.pendingFor(lib)
	pending.paths[path] = struct{}{}
	pending.changedAt = time.Now()
}

func (w *Watcher) pendingFor(lib string) *pendingScan {
	pending := w.pending[lib]
	if pending == nil {
		pending = &pendingScan{paths: map[string]struct{}{}, removed: map[string]bool{}}
		w.pending[lib] = pending
	}
	return pending
}

func scanTarget(path string, isDir bool) (string, bool) {
	if isDir || naming.IsVideo(path) || naming.IsAudio(path) {
		return filepath.Clean(path), true
	}
	if naming.IsImage(path) {
		return filepath.Dir(path), true
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".nfo", ".lrc", ".cue", ".srt", ".ass", ".ssa", ".vtt", ".sub", ".idx":
		return filepath.Dir(path), true
	default:
		return "", false
	}
}

// flush scans changed paths after they have been quiet for the debounce period, plus any due periodic rescans.
func (w *Watcher) flush(ctx context.Context) {
	now := time.Now()
	for lib, pending := range w.pending {
		if now.Sub(pending.changedAt) >= w.Debounce {
			delete(w.pending, lib)
			targets := compactRemovals(pending.removed)
			paths := make([]string, 0, len(pending.paths))
			for path := range pending.paths {
				if _, err := os.Stat(path); err == nil {
					paths = append(paths, path)
				} else if !os.IsNotExist(err) {
					w.Log.Warn("could not stat changed path", "library", lib, "path", path, "error", err)
				}
			}
			paths = compactPaths(paths)
			if len(paths) > 0 || len(targets) > 0 {
				w.change(ctx, lib, targets, paths)
			}
		}
	}
	if w.Rescan > 0 {
		for lib, last := range w.lastScan {
			if now.Sub(last) >= w.Rescan {
				w.scan(ctx, lib, nil, "periodic rescan")
			}
		}
	}
}

func (w *Watcher) change(ctx context.Context, lib string, removed []models.PathRemoval, paths []string) {
	queued, err := w.Scanner.StartChange(ctx, models.PathChangeRequest{LibraryID: lib, Paths: paths, Removed: removed})
	if err != nil {
		w.Log.Warn("could not queue filesystem change", "library", lib, "error", err)
		return
	}
	if queued {
		w.Log.Info("filesystem change queued", "library", lib, "paths", len(paths), "removed", len(removed))
	}
}

func (w *Watcher) scan(ctx context.Context, lib string, paths []string, reason string) {
	w.lastScan[lib] = time.Now()
	queued, err := w.Scanner.StartScan(ctx, models.ScanRequest{LibraryID: lib, Paths: paths})
	if err != nil {
		w.Log.Warn("could not queue automatic scan", "library", lib, "error", err)
		return
	}
	if queued {
		w.Log.Info("automatic scan queued", "library", lib, "paths", paths, "reason", reason)
	}
}

func compactPaths(paths []string) []string {
	sort.Slice(paths, func(i, j int) bool {
		if len(paths[i]) == len(paths[j]) {
			return paths[i] < paths[j]
		}
		return len(paths[i]) < len(paths[j])
	})
	compacted := make([]string, 0, len(paths))
	for _, path := range paths {
		covered := false
		for _, parent := range compacted {
			rel, err := filepath.Rel(parent, path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				covered = true
				break
			}
		}
		if !covered {
			compacted = append(compacted, path)
		}
	}
	sort.Strings(compacted)
	return compacted
}

func compactRemovals(paths map[string]bool) []models.PathRemoval {
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) == len(ordered[j]) {
			return ordered[i] < ordered[j]
		}
		return len(ordered[i]) < len(ordered[j])
	})
	compacted := make([]models.PathRemoval, 0, len(ordered))
	for _, path := range ordered {
		recursive := paths[path]
		covered := false
		for i := range compacted {
			parent := compacted[i].Path
			rel, err := filepath.Rel(parent, path)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				continue
			}
			if rel == "." {
				compacted[i].Recursive = compacted[i].Recursive || recursive
				covered = true
				break
			}
			if compacted[i].Recursive {
				covered = true
				break
			}
		}
		if !covered {
			compacted = append(compacted, models.PathRemoval{Path: path, Recursive: recursive})
		}
	}
	sort.Slice(compacted, func(i, j int) bool { return compacted[i].Path < compacted[j].Path })
	return compacted
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
			if w.fs != nil {
				_ = w.fs.Remove(dir)
			}
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
