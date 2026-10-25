package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/hibiken/asynq"

	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/themes"
)

// SetThemesEnabled switches ThemerrDB theme downloads on or off.
func (p *Pipeline) SetThemesEnabled(enabled bool) { p.themesEnabled.Store(enabled) }

// ThemesEnabled reports whether theme downloads are on.
func (p *Pipeline) ThemesEnabled() bool { return p.Themes != nil && p.themesEnabled.Load() }

// ThemeTask builds a theme task for an indexed item, if it can have a theme.
func ThemeTask(item models.Item) (themes.Task, bool) {
	if item.Type != models.ItemMovie && item.Type != models.ItemSeries {
		return themes.Task{}, false
	}
	id := item.ProviderIDs["tmdb"]
	if id == "" || item.Path == "" || item.RootID == "" || item.RelativePath == "" {
		return themes.Task{}, false
	}
	return themes.Task{LibraryID: item.LibraryID, Path: item.Path, RootID: item.RootID, RelativePath: item.RelativePath, ItemType: item.Type, TMDBID: id}, true
}

func (p *Pipeline) queueTheme(ctx context.Context, item models.Item) {
	if !p.ThemesEnabled() {
		return
	}
	if task, ok := ThemeTask(item); ok {
		p.QueueTheme(ctx, task)
	}
}

// QueueTheme enqueues one theme download.
func (p *Pipeline) QueueTheme(ctx context.Context, task themes.Task) bool {
	queued, err := p.Enqueue(ctx, config.QueueThemes, TypeThemes, task, 2)
	if err != nil {
		p.logger(ctx).Warn("could not queue theme", "library", task.LibraryID, "error", err)
	}
	return queued
}

// FolderRoot is the library folder containing path, or "".
func (p *Pipeline) FolderRoot(lib models.Library, path string) string { return p.folderRoot(lib, path) }

func (p *Pipeline) handleThemes(ctx context.Context, t *asynq.Task) error {
	if !p.ThemesEnabled() {
		return nil
	}
	var task themes.Task
	if err := json.Unmarshal(t.Payload(), &task); err != nil || task.Path == "" || task.RootID == "" || task.RelativePath == "" || task.TMDBID == "" {
		return fmt.Errorf("bad theme payload: %w", asynq.SkipRetry)
	}
	lib, err := p.Library(ctx, task.LibraryID)
	if err != nil {
		return err
	}
	ok, err := p.Themes.Fetch(ctx, lib, task, p.folderRoot(lib, task.Path))
	if err != nil {
		p.logger(ctx).Warn("theme download failed", "library", task.LibraryID, "tmdb", task.TMDBID, "error", err)
		return err
	}
	if ok {
		p.logger(ctx).Info("theme downloaded", "library", task.LibraryID, "tmdb", task.TMDBID, "type", task.ItemType)
	}
	return nil
}
