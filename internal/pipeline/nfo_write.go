package pipeline

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/nfo"
)

func (p *Pipeline) WriteNFO(ctx context.Context, request models.NFOWriteRequest) error {
	if request.LibraryID == "" || request.Path == "" {
		return errors.New("libraryId and media path are required")
	}
	library, err := p.Library(ctx, request.LibraryID)
	if err != nil {
		return err
	}
	root := p.folderRoot(library, request.Path)
	if root == "" {
		return errors.New("media path is outside the configured library folders")
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	mediaInfo, err := os.Lstat(request.Path)
	if err != nil {
		return err
	}
	if mediaInfo.Mode()&os.ModeSymlink != 0 || !mediaInfo.Mode().IsRegular() && !mediaInfo.IsDir() {
		return errors.New("media path is not a regular file or directory")
	}
	canonicalMedia, err := filepath.EvalSymlinks(request.Path)
	if err != nil || !isWithin(canonicalMedia, canonicalRoot) {
		return errors.New("media path resolves outside the configured library root")
	}
	item := models.Item{Type: request.ItemType, Path: request.Path}
	candidates := nfoCandidates(item, root)
	if len(candidates) == 0 {
		return errors.New("this media type does not support NFO sidecars")
	}
	root = filepath.Clean(root)
	target := candidates[0]
	for _, candidate := range candidates {
		if !isWithin(candidate, root) {
			return errors.New("NFO path is outside the configured library folder")
		}
		canonicalParent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
		if err != nil || !isWithin(canonicalParent, canonicalRoot) {
			return errors.New("NFO parent resolves outside the configured library root")
		}
		if _, err := os.Lstat(candidate); err == nil {
			target = candidate
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	rootElement := ""
	switch request.ItemType {
	case models.ItemMovie, models.ItemVideo:
		rootElement = "movie"
	case models.ItemSeries:
		rootElement = "tvshow"
	case models.ItemSeason:
		rootElement = "season"
	case models.ItemEpisode:
		rootElement = "episodedetails"
	case models.ItemAlbum:
		rootElement = "album"
	default:
		return errors.New("this media type does not support NFO sidecars")
	}
	return nfo.Write(target, rootElement, request.Fields, request.ProviderIDs, request.ClearProviderIDs)
}
