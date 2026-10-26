package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hibiken/asynq"

	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/flixerclient"
	"github.com/flixermedia/flixer-indexer/internal/images"
	"github.com/flixermedia/flixer-indexer/internal/lyrics"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/naming"
	"github.com/flixermedia/flixer-indexer/internal/nfo"
	"github.com/flixermedia/flixer-indexer/internal/probe"
	"github.com/flixermedia/flixer-indexer/internal/providers"
	"github.com/flixermedia/flixer-indexer/internal/providers/registry"
	"github.com/flixermedia/flixer-indexer/internal/scanner"
	"github.com/flixermedia/flixer-indexer/internal/tasklog"
	"github.com/flixermedia/flixer-indexer/internal/trickplay"
)

// handleScan walks a library and queues an identify task per item.
func (p *Pipeline) handleScan(ctx context.Context, t *asynq.Task) error {
	var req models.ScanRequest
	if err := json.Unmarshal(t.Payload(), &req); err != nil {
		return fmt.Errorf("bad payload: %w", asynq.SkipRetry)
	}
	lib, err := p.Library(ctx, req.LibraryID)
	if err != nil {
		return err
	}
	if req.TrickplayOnly {
		return p.handleTrickplayScan(ctx, lib)
	}
	if err := p.Progress.Start(ctx, lib.ID); err != nil {
		return err
	}
	p.logger(ctx).Info("scan started", "library", lib.ID, "name", lib.Name, "type", lib.Type)

	err = scanner.Scan(ctx, lib, func(w models.Work) error {
		w.Force = req.Force
		queued, err := p.Enqueue(ctx, config.QueueIdentify, TypeIdentify, w, maxRetry)
		if err != nil {
			return err
		}
		if queued {
			return p.Progress.Discovered(ctx, lib.ID)
		}
		return nil
	})
	if err != nil {
		_ = p.Progress.WalkFailed(context.WithoutCancel(ctx), lib.ID, err)
		return fmt.Errorf("scan %s: %w: %w", lib.ID, err, asynq.SkipRetry)
	}
	p.logger(ctx).Info("scan walk finished", "library", lib.ID)
	if err := p.Progress.WalkDone(ctx, lib.ID); err != nil {
		return err
	}
	// An empty walk completes immediately, with no item left to trigger the prune.
	p.pruneIfComplete(ctx, lib.ID)
	return nil
}

// handleRefresh re-describes a single path and runs it through the pipeline.
func (p *Pipeline) handleTrickplayScan(ctx context.Context, lib models.Library) error {
	p.logger(ctx).Info("trickplay backfill started", "library", lib.ID, "name", lib.Name)
	return scanner.Scan(ctx, lib, func(w models.Work) error {
		item := w.Item
		if (item.Type != models.ItemMovie && item.Type != models.ItemEpisode && item.Type != models.ItemVideo) || item.RootID == "" || item.RelativePath == "" {
			return nil
		}
		task := trickplay.Task{LibraryID: item.LibraryID, Path: item.Path, RootID: item.RootID, RelativePath: item.RelativePath}
		_, err := p.Enqueue(ctx, config.QueueTrickplay, TypeTrickplay, task, 1)
		if err != nil {
			p.logger(ctx).Warn("could not queue trickplay during backfill", "library", lib.ID, "error", err)
		}
		return nil
	})
}

func (p *Pipeline) handleTrickplay(ctx context.Context, t *asynq.Task) error {
	if p.Trickplay == nil {
		return fmt.Errorf("trickplay generator is unavailable: %w", asynq.SkipRetry)
	}
	var task trickplay.Task
	if err := json.Unmarshal(t.Payload(), &task); err != nil || task.Path == "" || task.RootID == "" || task.RelativePath == "" {
		return fmt.Errorf("bad trickplay payload: %w", asynq.SkipRetry)
	}
	if p.TaskLogs != nil && tasklog.InTask(ctx) {
		log := p.logger(ctx)
		ctx = trickplay.WithOutput(ctx, func(line string) { log.Info("ffmpeg: " + line) })
	}
	if err := p.Trickplay.Generate(ctx, task); err != nil {
		p.logger(ctx).Warn("trickplay generation failed", "library", task.LibraryID, "error", err)
		return err
	}
	return nil
}

func (p *Pipeline) handleRefresh(ctx context.Context, t *asynq.Task) error {
	var req models.RefreshRequest
	if err := json.Unmarshal(t.Payload(), &req); err != nil {
		return fmt.Errorf("bad payload: %w", asynq.SkipRetry)
	}
	lib, err := p.Library(ctx, req.LibraryID)
	if err != nil {
		return err
	}
	switch req.ItemType {
	case models.ItemSeries, models.ItemSeason, models.ItemAlbum:
		// A container refresh re-indexes every item beneath it.
		err := scanner.ScanBranch(ctx, lib, req.Path, func(w models.Work) error {
			w.Force, w.Untracked = true, true
			_, err := p.Enqueue(ctx, config.QueueIdentify, TypeIdentify, w, maxRetry)
			return err
		})
		if err != nil {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return nil
	}
	w, err := scanner.Describe(ctx, lib, req.Path, req.ItemType)
	if err != nil {
		return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
	}
	w.Force, w.Untracked = true, true
	_, err = p.Enqueue(ctx, config.QueueIdentify, TypeIdentify, w, maxRetry)
	return err
}

func (p *Pipeline) folderRoot(lib models.Library, path string) string {
	for _, f := range lib.Folders {
		if f.AbsolutePath != "" && isWithin(path, f.AbsolutePath) {
			return f.AbsolutePath
		}
	}
	return ""
}

func isWithin(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// nfoPath returns the first NFO file that exists for the item.
func nfoPath(item models.Item, root string) string {
	dir := filepath.Dir(item.Path)
	var candidates []string
	switch item.Type {
	case models.ItemSeries:
		candidates = []string{filepath.Join(item.Path, "tvshow.nfo")}
	case models.ItemSeason:
		candidates = []string{filepath.Join(item.Path, "season.nfo")}
	case models.ItemAlbum:
		candidates = []string{filepath.Join(item.Path, "album.nfo")}
	case models.ItemTrack:
		return ""
	default:
		candidates = []string{filepath.Join(dir, naming.Base(item.Path)+".nfo")}
		if item.Type == models.ItemMovie && !strings.EqualFold(filepath.Clean(dir), filepath.Clean(root)) {
			candidates = append(candidates, filepath.Join(dir, "movie.nfo"))
		}
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	return ""
}

// handleIdentify reads local information: NFO, artwork and, for music, tags.
func (p *Pipeline) handleIdentify(ctx context.Context, t *asynq.Task) error {
	w, err := decodeWork(t)
	if err != nil {
		return err
	}
	lib, err := p.Library(ctx, w.Item.LibraryID)
	if err != nil {
		return err
	}
	root := p.folderRoot(lib, w.Item.Path)
	item := &w.Item

	if path := nfoPath(*item, root); path != "" {
		data, err := nfo.Read(path)
		if err != nil {
			p.logger(ctx).Warn("unreadable nfo", "path", path, "error", err)
		} else {
			item.NFOPresent = true
			item.Metadata = data.Metadata
			item.ProviderIDs = providers.MergeIDs(item.ProviderIDs, data.IDs)
			w.NFOComplete = data.Complete()
		}
	}
	if item.Type == models.ItemSeason || item.Type == models.ItemEpisode {
		if path := filepath.Join(w.Hints.SeriesPath, "tvshow.nfo"); w.Hints.SeriesPath != "" {
			if data, err := nfo.Read(path); err == nil {
				w.Hints.SeriesIDs = providers.MergeIDs(w.Hints.SeriesIDs, data.IDs)
			}
		}
	}

	if local := images.FindLocal(*item, root); len(local) > 0 {
		item.Images = local
		item.LocalImagesUsed = true
	}

	if item.Type == models.ItemTrack {
		res, err := p.Probe.Probe(ctx, item.Path)
		if err != nil {
			p.logger(ctx).Warn("track probe failed", "path", item.Path, "error", err)
		} else {
			item.Technical = &res.Technical
			applyTags(item, res.Tags)
		}
	}

	_, err = p.Enqueue(ctx, config.QueueMetadata, TypeMetadata, w, maxRetry)
	return err
}

// ensureLyrics stores a missing .lrc under LyricsDir/<track id>.lrc, which works
// for read-only libraries. Failures only log; lyrics are optional.
func (p *Pipeline) ensureLyrics(ctx context.Context, item *models.Item, h models.Hints, id string) {
	m := item.Metadata
	q := lyrics.Query{
		Artist:      firstNonEmpty(m.Artist, h.Artist),
		AlbumArtist: m.AlbumArtist,
		Title:       firstNonEmpty(m.Title, h.Title),
		Album:       firstNonEmpty(m.Album, h.Album),
	}
	if item.Technical != nil {
		q.Duration = item.Technical.Duration
	}
	found, err := lyrics.Ensure(ctx, p.Lyrics, item.Path, p.LyricsDir, id, q)
	if err != nil {
		p.logger(ctx).Warn("lyrics unavailable", "path", item.Path, "error", err)
		return
	}
	if found {
		p.logger(ctx).Debug("lyrics ready", "path", item.Path, "id", id)
	}
}

// applyTags lets embedded audio tags override names derived from the path.
func applyTags(item *models.Item, tags map[string]string) {
	m := &item.Metadata
	set := func(dst *string, keys ...string) {
		for _, k := range keys {
			if v := strings.TrimSpace(tags[k]); v != "" {
				*dst = v
				return
			}
		}
	}
	set(&m.Title, "title")
	set(&m.Artist, "artist")
	set(&m.AlbumArtist, "album_artist", "albumartist")
	set(&m.Album, "album")
	set(&m.Label, "label", "publisher")
	if g := strings.TrimSpace(tags["genre"]); g != "" && len(m.Genres) == 0 {
		m.Genres = splitList(g)
	}
	if n, total := splitPair(tags["track"]); n > 0 {
		m.TrackNumber = n
		if total > 0 {
			m.TotalTracks = total
		}
	}
	if n, _ := splitPair(tags["disc"]); n > 0 {
		m.DiscNumber = n
	}
	for _, k := range []string{"date", "year", "originaldate"} {
		if v := tags[k]; len(v) >= 4 {
			if y, err := strconv.Atoi(v[:4]); err == nil {
				m.Year = y
				if len(v) >= 10 {
					m.PremiereDate = v[:10]
				}
				break
			}
		}
	}
	if id := strings.TrimSpace(tags["musicbrainz_trackid"]); id != "" {
		item.ProviderIDs["musicbrainz"] = id
	}
}

func splitPair(s string) (int, int) {
	a, b, _ := strings.Cut(strings.TrimSpace(s), "/")
	n, _ := strconv.Atoi(strings.TrimSpace(a))
	total, _ := strconv.Atoi(strings.TrimSpace(b))
	return n, total
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '/' || r == ',' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// handleMetadata asks remote providers to fill in what local files lacked.
func (p *Pipeline) handleMetadata(ctx context.Context, t *asynq.Task) error {
	w, err := decodeWork(t)
	if err != nil {
		return err
	}
	// A complete NFO skips metadata lookups, but episodes still need a provider still when no local thumb exists.
	if !(w.NFOComplete && !w.Force) || needsProviderArtwork(w.Item) {
		lib, err := p.Library(ctx, w.Item.LibraryID)
		if err != nil {
			return err
		}
		if reg := p.providers(lib); reg != nil {
			if err := p.lookup(ctx, reg, &w); err != nil {
				return err
			}
		}
	}
	_, err = p.Enqueue(ctx, config.QueueImages, TypeImages, w, maxRetry)
	return err
}

func needsProviderArtwork(item models.Item) bool {
	if item.Type != models.ItemEpisode {
		return false
	}
	for _, img := range item.Images {
		if img.Type == models.ImageThumb && img.Path != "" {
			return false
		}
	}
	return true
}

func (p *Pipeline) lookup(ctx context.Context, reg *registry.Registry, w *models.Work) error {
	item, h := &w.Item, w.Hints
	query := providers.Query{Title: firstNonEmpty(item.Metadata.Title, h.Title), Year: firstNonZero(item.Metadata.Year, h.Year), IDs: item.ProviderIDs}

	var res *providers.Result
	var err error
	switch item.Type {
	case models.ItemMovie:
		res, err = reg.Movie(ctx, query)
	case models.ItemSeries:
		res, err = reg.Series(ctx, query)
	case models.ItemAlbum:
		query.Title = firstNonEmpty(h.Album, query.Title)
		query.Artist = h.Artist
		res, err = reg.Album(ctx, query)
	case models.ItemSeason, models.ItemEpisode:
		ids, serr := p.seriesIDs(ctx, reg, h)
		if serr != nil {
			return serr
		}
		w.Hints.SeriesIDs = ids
		if item.SeasonNumber == nil || len(ids) == 0 {
			return nil
		}
		if item.Type == models.ItemSeason {
			res, err = reg.Season(ctx, ids, *item.SeasonNumber)
		} else if item.EpisodeNumber != nil {
			res, err = reg.Episode(ctx, ids, *item.SeasonNumber, *item.EpisodeNumber)
		}
	default:
		return nil
	}
	if err != nil {
		return err
	}
	if res == nil {
		return nil
	}
	item.ProviderIDs = providers.MergeIDs(item.ProviderIDs, res.IDs)
	providers.MergeMetadata(&item.Metadata, res.Meta)
	for _, img := range res.Images {
		if img.URL != "" {
			item.Images = append(item.Images, img)
		}
	}
	return nil
}

// seriesIDs resolves a series' provider ids for its seasons and episodes.
// Lookups are cached by the registry, so repeating this per episode is cheap.
func (p *Pipeline) seriesIDs(ctx context.Context, reg *registry.Registry, h models.Hints) (map[string]string, error) {
	ids := providers.MergeIDs(nil, h.SeriesIDs)
	if h.SeriesTitle == "" && len(ids) == 0 {
		return ids, nil
	}
	res, err := reg.Series(ctx, providers.Query{Title: h.SeriesTitle, Year: h.SeriesYear, IDs: ids})
	if err != nil {
		return nil, err
	}
	if res != nil {
		ids = providers.MergeIDs(ids, res.IDs)
	}
	return ids, nil
}

var imageQuota = map[string]int{
	models.ImagePoster: 1, models.ImageBackdrop: 3, models.ImageLogo: 1,
	models.ImageBanner: 1, models.ImageThumb: 1, models.ImageDisc: 1,
}

// handleImages downloads remote artwork for the types local files lack.
func (p *Pipeline) handleImages(ctx context.Context, t *asynq.Task) error {
	w, err := decodeWork(t)
	if err != nil {
		return err
	}
	lib, err := p.Library(ctx, w.Item.LibraryID)
	if err != nil {
		return err
	}
	item := &w.Item
	place := images.Placement{Library: lib, Item: *item, FolderRoot: p.folderRoot(lib, item.Path)}

	var local, candidates []models.Image
	have := map[string]bool{}
	for _, img := range item.Images {
		if img.Path != "" {
			local = append(local, img)
			have[img.Type] = true
		} else if img.URL != "" {
			candidates = append(candidates, img)
		}
	}

	if reg := p.providers(lib); reg != nil {
		ids, season := item.ProviderIDs, (*int)(nil)
		switch item.Type {
		case models.ItemSeason:
			ids, season = w.Hints.SeriesIDs, item.SeasonNumber
			fallthrough
		case models.ItemMovie, models.ItemSeries, models.ItemAlbum:
			candidates = append(candidates, reg.Images(ctx, item.Type, ids, season)...)
		}
	}

	saved := map[string]int{}
	seen := map[string]bool{}
	for _, c := range candidates {
		if have[c.Type] || seen[c.URL] || saved[c.Type] >= imageQuota[c.Type] {
			continue
		}
		if item.Type == models.ItemSeason && c.Season != nil && item.SeasonNumber != nil && *c.Season != *item.SeasonNumber {
			continue
		}
		seen[c.URL] = true
		img, err := p.Images.Save(ctx, place, c, saved[c.Type])
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.logger(ctx).Debug("image skipped", "path", item.Path, "provider", c.Provider, "error", err)
			continue
		}
		saved[c.Type]++
		local = append(local, img)
	}

	if item.Type == models.ItemAlbum && !have[models.ImagePoster] && saved[models.ImagePoster] == 0 {
		if img, ok := p.embeddedCover(ctx, place); ok {
			local = append(local, img)
		}
	}

	item.Images = local
	_, err = p.Enqueue(ctx, config.QueueProbe, TypeProbe, w, maxRetry)
	return err
}

func (p *Pipeline) embeddedCover(ctx context.Context, place images.Placement) (models.Image, bool) {
	var first string
	_ = filepath.WalkDir(place.Item.Path, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && naming.IsAudio(d.Name()) {
			first = path
			return filepath.SkipAll
		}
		return nil
	})
	if first == "" {
		return models.Image{}, false
	}
	data, err := p.Images.ExtractEmbedded(ctx, first)
	if err != nil {
		return models.Image{}, false
	}
	img, err := p.Images.SaveBytes(place, models.Image{Type: models.ImagePoster, Provider: "embedded"}, 0, data)
	return img, err == nil
}

// handleProbe adds technical details and persists the finished item.
func (p *Pipeline) handleProbe(ctx context.Context, t *asynq.Task) error {
	w, err := decodeWork(t)
	if err != nil {
		return err
	}
	item := &w.Item

	switch item.Type {
	case models.ItemMovie, models.ItemEpisode, models.ItemVideo, models.ItemTrack:
		if item.Technical == nil {
			res, err := p.Probe.Probe(ctx, item.Path)
			switch {
			case err == nil:
				item.Technical = &res.Technical
			case ctx.Err() != nil:
				return ctx.Err()
			default:
				retried, _ := asynq.GetRetryCount(ctx)
				if retried < maxRetry {
					return err
				}
				p.logger(ctx).Warn("probe failed; indexing without technical details", "path", item.Path, "error", err)
			}
		}
	}

	switch item.Type {
	case models.ItemMovie, models.ItemEpisode, models.ItemVideo:
		p.ensureSubtitles(ctx, item, w.Hints)
	}

	lib, err := p.Library(ctx, item.LibraryID)
	if err != nil {
		return err
	}
	finalize(&w, p.folderRoot(lib, item.Path))
	id, err := p.Client.UpsertItem(ctx, *item)
	if err != nil {
		if errors.Is(err, flixerclient.ErrPermanent) {
			return fmt.Errorf("%w: %w", err, asynq.SkipRetry)
		}
		return err
	}
	if item.Type == models.ItemTrack {
		p.ensureLyrics(ctx, item, w.Hints, id)
	}
	if p.TrickplayEnabled && (item.Type == models.ItemMovie || item.Type == models.ItemEpisode || item.Type == models.ItemVideo) && item.RootID != "" && item.RelativePath != "" {
		task := trickplay.Task{LibraryID: item.LibraryID, Path: item.Path, RootID: item.RootID, RelativePath: item.RelativePath}
		if _, enqueueErr := p.Enqueue(ctx, config.QueueTrickplay, TypeTrickplay, task, 1); enqueueErr != nil {
			p.logger(ctx).Warn("could not queue trickplay", "library", item.LibraryID, "error", enqueueErr)
		}
	}
	p.queueTheme(ctx, *item)
	if !w.Untracked {
		if err := p.Progress.Done(ctx, item.LibraryID); err != nil {
			return err
		}
		p.pruneIfComplete(ctx, item.LibraryID)
	}
	return nil
}

// pruneIfComplete removes items a finished scan did not find, so content that
// moved (another library folder) or disappeared does not linger. It only runs
// once per scan, and never for a scan with failures, since an item that failed
// to index would look missing.
func (p *Pipeline) pruneIfComplete(ctx context.Context, libraryID string) {
	startedAt, ok, err := p.Progress.ClaimPrune(ctx, libraryID)
	if err != nil || !ok {
		if err != nil {
			p.logger(ctx).Warn("prune check failed", "library", libraryID, "error", err)
		}
		return
	}
	removed, err := p.Client.PruneLibrary(context.WithoutCancel(ctx), libraryID, startedAt)
	if err != nil {
		p.logger(ctx).Warn("prune failed", "library", libraryID, "error", err)
		return
	}
	p.logger(ctx).Info("scan pruned", "library", libraryID, "removed", removed)
}

// finalize fills anything still empty from what the filesystem told us.
func finalize(w *models.Work, root string) {
	m, h := &w.Item.Metadata, w.Hints
	if strings.TrimSpace(m.Title) == "" {
		m.Title = h.Title
	}
	if m.Title == "" {
		m.Title = naming.Base(w.Item.Path)
	}
	if m.Year == 0 {
		m.Year = h.Year
	}
	if w.Item.ProviderIDs == nil {
		w.Item.ProviderIDs = map[string]string{}
	}
	if w.Item.Images == nil {
		w.Item.Images = []models.Image{}
	}
	models.AssignKeys(&w.Item, h.SeriesIDs, root)
	if m.Artist == "" {
		m.Artist = h.Artist
	}
	if m.Album == "" {
		m.Album = h.Album
	}
	if w.Item.Technical != nil && m.Runtime == 0 && w.Item.Technical.Duration > 0 {
		m.Runtime = int(w.Item.Technical.Duration/60 + 0.5)
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonZero(values ...int) int {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

var _ = probe.New
