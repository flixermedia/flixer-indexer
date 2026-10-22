// Package scanner walks library folders and describes what it finds as items.
// It only looks at file names and structure; metadata comes later.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/naming"
)

// Emit receives each discovered item. Returning an error aborts the scan.
type Emit func(models.Work) error

var errFound = errors.New("found")

type scan struct {
	ctx    context.Context
	lib    models.Library
	folder models.Folder
	emit   Emit
	// target limits the walk to the branch containing this path.
	target string
}

// Scan walks every folder of the library.
func Scan(ctx context.Context, lib models.Library, emit Emit) error {
	for _, f := range lib.Folders {
		if f.AbsolutePath == "" {
			continue
		}
		if _, err := os.Stat(f.AbsolutePath); err != nil {
			return fmt.Errorf("library folder %s unavailable: %w", f.AbsolutePath, err)
		}
		s := &scan{ctx: ctx, lib: lib, folder: f, emit: emit}
		if err := s.run(); err != nil {
			return err
		}
	}
	return nil
}

// Describe builds the work item for a single known path and type.
func Describe(ctx context.Context, lib models.Library, path, itemType string) (models.Work, error) {
	var found models.Work
	capture := func(w models.Work) error {
		if samePath(w.Item.Path, path) && w.Item.Type == itemType {
			found = w
			return errFound
		}
		return nil
	}
	for _, f := range lib.Folders {
		if f.AbsolutePath == "" || !within(path, f.AbsolutePath) {
			continue
		}
		s := &scan{ctx: ctx, lib: lib, folder: f, emit: capture, target: path}
		if err := s.run(); err != nil && !errors.Is(err, errFound) {
			return found, err
		}
		if found.Item.Path != "" {
			return found, nil
		}
	}
	return found, fmt.Errorf("%s %s not found in library %s", itemType, path, lib.ID)
}

func (s *scan) run() error {
	switch s.lib.Type {
	case models.LibraryMovies:
		return s.movies(s.folder.AbsolutePath, true)
	case models.LibraryTVShows:
		return s.shows(s.folder.AbsolutePath)
	case models.LibraryMusic:
		return s.music(s.folder.AbsolutePath)
	case models.LibraryHomeVideos:
		return s.homeVideos(s.folder.AbsolutePath)
	default:
		return s.mixed()
	}
}

func (s *scan) wanted(p string) bool {
	return s.target == "" || within(p, s.target) || within(s.target, p)
}

func (s *scan) entries(dir string) []os.DirEntry {
	list, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name()) < strings.ToLower(list[j].Name()) })
	return list
}

func (s *scan) base(typ, path string) models.Item {
	item := models.Item{
		LibraryID:   s.lib.ID,
		Type:        typ,
		Path:        path,
		RootID:      s.folder.RootID,
		ProviderIDs: map[string]string{},
		Images:      []models.Image{},
	}
	if rel, err := filepath.Rel(s.folder.AbsolutePath, path); err == nil {
		item.RelativePath = strings.Trim(filepath.ToSlash(filepath.Join(s.folder.Path, rel)), "/.")
	}
	if info, err := os.Stat(path); err == nil {
		item.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
		if !info.IsDir() {
			item.SizeBytes = info.Size()
		}
	}
	return item
}

func (s *scan) send(w models.Work) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return s.emit(w)
}

func mergeIDs(dst map[string]string, srcs ...map[string]string) {
	for _, src := range srcs {
		for k, v := range src {
			if _, ok := dst[k]; !ok {
				dst[k] = v
			}
		}
	}
}

// movies emits one movie per video. A video inside its own folder takes the
// folder name as its title hint.
func (s *scan) movies(dir string, isRoot bool) error {
	if !s.wanted(dir) {
		return nil
	}
	for _, e := range s.entries(dir) {
		p := filepath.Join(dir, e.Name())
		if strings.HasPrefix(e.Name(), ".") || naming.IsExtra(e.Name()) {
			continue
		}
		if e.IsDir() {
			if err := s.movies(p, false); err != nil {
				return err
			}
			continue
		}
		if !naming.IsVideo(e.Name()) || !s.wanted(p) {
			continue
		}
		if err := s.send(s.movie(p, !isRoot)); err != nil {
			return err
		}
	}
	return nil
}

func (s *scan) movie(file string, inFolder bool) models.Work {
	name := naming.Base(file)
	parsed := naming.ParseTitle(name)
	ids := map[string]string{}
	if inFolder {
		dirParsed := naming.ParseTitle(filepath.Base(filepath.Dir(file)))
		if dirParsed.Title != "" && (parsed.Title == "" || strings.Contains(strings.ToLower(name), strings.ToLower(dirParsed.Title)) || parsed.Year == 0) {
			parsed.Title = dirParsed.Title
			if dirParsed.Year != 0 {
				parsed.Year = dirParsed.Year
			}
		}
		mergeIDs(ids, dirParsed.IDs)
	}
	mergeIDs(ids, parsed.IDs)
	item := s.base(models.ItemMovie, file)
	item.ProviderIDs = ids
	return models.Work{Item: item, Hints: models.Hints{Title: parsed.Title, Year: parsed.Year}}
}

func (s *scan) homeVideos(dir string) error {
	if !s.wanted(dir) {
		return nil
	}
	for _, e := range s.entries(dir) {
		p := filepath.Join(dir, e.Name())
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.IsDir() {
			if err := s.homeVideos(p); err != nil {
				return err
			}
			continue
		}
		if !naming.IsVideo(e.Name()) || !s.wanted(p) {
			continue
		}
		item := s.base(models.ItemVideo, p)
		item.ParentPath = dir
		parsed := naming.ParseTitle(naming.Base(p))
		title := parsed.Title
		if title == "" {
			title = naming.Base(p)
		}
		item.ProviderIDs = parsed.IDs
		if err := s.send(models.Work{Item: item, Hints: models.Hints{Title: title, Year: parsed.Year}}); err != nil {
			return err
		}
	}
	return nil
}

// shows treats each direct child directory as a series.
func (s *scan) shows(root string) error {
	for _, e := range s.entries(root) {
		p := filepath.Join(root, e.Name())
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || naming.IsExtra(e.Name()) || !s.wanted(p) {
			continue
		}
		if err := s.series(p); err != nil {
			return err
		}
	}
	return nil
}

func (s *scan) series(dir string) error {
	parsed := naming.ParseTitle(filepath.Base(dir))
	item := s.base(models.ItemSeries, dir)
	item.ProviderIDs = parsed.IDs
	hints := models.Hints{Title: parsed.Title, Year: parsed.Year}
	if err := s.send(models.Work{Item: item, Hints: hints}); err != nil {
		return err
	}

	seriesHints := models.Hints{
		SeriesTitle: parsed.Title, SeriesYear: parsed.Year, SeriesPath: dir, SeriesIDs: parsed.IDs,
	}
	seasonPaths := map[int]string{}
	var walk func(d string, season *int) error
	walk = func(d string, season *int) error {
		for _, e := range s.entries(d) {
			p := filepath.Join(d, e.Name())
			if strings.HasPrefix(e.Name(), ".") || naming.IsExtra(e.Name()) {
				continue
			}
			if e.IsDir() {
				sub := season
				if n, ok := naming.SeasonFromDir(e.Name()); ok {
					sub = &n
				}
				if s.wanted(p) {
					if err := walk(p, sub); err != nil {
						return err
					}
				}
				continue
			}
			if !naming.IsVideo(e.Name()) || !s.wanted(p) {
				continue
			}
			ep := naming.ParseEpisode(naming.Base(p), season)
			if !ep.Matched {
				continue
			}
			n := 1
			if ep.Season != nil {
				n = *ep.Season
			}
			seasonPath, seen := seasonPaths[n]
			if !seen {
				seasonPath = filepath.Join(dir, fmt.Sprintf("Season %d", n))
				if season != nil {
					seasonPath = d
				}
				seasonPaths[n] = seasonPath
				if err := s.sendSeason(seasonPath, n, seriesHints); err != nil {
					return err
				}
			}
			if err := s.sendEpisode(p, seasonPath, n, ep, seriesHints); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(dir, nil)
}

func (s *scan) sendSeason(path string, n int, series models.Hints) error {
	item := s.base(models.ItemSeason, path)
	item.ParentPath = series.SeriesPath
	item.SeasonNumber = &n
	item.ProviderIDs = map[string]string{}
	return s.send(models.Work{Item: item, Hints: series})
}

func (s *scan) sendEpisode(file, seasonPath string, season int, ep naming.Episode, series models.Hints) error {
	item := s.base(models.ItemEpisode, file)
	item.ParentPath = seasonPath
	item.SeasonNumber = &season
	num := ep.Number
	item.EpisodeNumber = &num
	item.EpisodeEnd = ep.EndNum
	item.ProviderIDs = naming.ProviderIDs(filepath.Base(file))
	h := series
	h.Title = ep.Title
	return s.send(models.Work{Item: item, Hints: h})
}

// music treats a directory containing audio files as an album. Disc N
// subfolders fold into the album above them.
func (s *scan) music(dir string) error {
	if !s.wanted(dir) {
		return nil
	}
	list := s.entries(dir)
	var audio []string
	for _, e := range list {
		if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") && naming.IsAudio(e.Name()) {
			audio = append(audio, filepath.Join(dir, e.Name()))
		}
	}
	isRoot := samePath(dir, s.folder.AbsolutePath)
	if (len(audio) > 0 || hasDiscFolders(dir, list)) && !isRoot {
		if err := s.album(dir, list); err != nil {
			return err
		}
		return nil
	}
	for _, e := range list {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			if err := s.music(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *scan) album(dir string, list []os.DirEntry) error {
	// A Disc N folder is part of its parent's album.
	if _, ok := naming.DiscFromDir(filepath.Base(dir)); ok {
		parent := filepath.Dir(dir)
		if !samePath(parent, s.folder.AbsolutePath) {
			return nil
		}
	}
	parsed := naming.ParseAlbum(filepath.Base(dir))
	artist := parsed.Artist
	if parentDir := filepath.Dir(dir); artist == "" && !samePath(parentDir, s.folder.AbsolutePath) {
		artist = filepath.Base(parentDir)
	}
	item := s.base(models.ItemAlbum, dir)
	item.ProviderIDs = parsed.IDs
	hints := models.Hints{Title: parsed.Title, Year: parsed.Year, Artist: artist, Album: parsed.Title}
	if err := s.send(models.Work{Item: item, Hints: hints}); err != nil {
		return err
	}

	trackHints := models.Hints{Artist: artist, Album: parsed.Title, SeriesPath: dir}
	var tracks func(d string, disc int) error
	tracks = func(d string, disc int) error {
		for _, e := range s.entries(d) {
			p := filepath.Join(d, e.Name())
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if e.IsDir() {
				if n, ok := naming.DiscFromDir(e.Name()); ok {
					if err := tracks(p, n); err != nil {
						return err
					}
				}
				continue
			}
			if !naming.IsAudio(e.Name()) || !s.wanted(p) {
				continue
			}
			tr := naming.ParseTrack(naming.Base(p))
			ti := s.base(models.ItemTrack, p)
			ti.ParentPath = dir
			d := disc
			if tr.Disc > 0 {
				d = tr.Disc
			}
			ti.Metadata = models.Metadata{Title: tr.Title, TrackNumber: tr.Number, DiscNumber: d}
			h := trackHints
			h.Title = tr.Title
			if err := s.send(models.Work{Item: ti, Hints: h}); err != nil {
				return err
			}
		}
		return nil
	}
	_ = list
	return tracks(dir, 0)
}

// mixed decides per top-level entry: audio folders are albums, folders with
// seasons or episode-numbered files are series, everything else is movies.
func (s *scan) mixed() error {
	root := s.folder.AbsolutePath
	for _, e := range s.entries(root) {
		p := filepath.Join(root, e.Name())
		if strings.HasPrefix(e.Name(), ".") || naming.IsExtra(e.Name()) || !s.wanted(p) {
			continue
		}
		if !e.IsDir() {
			if naming.IsVideo(e.Name()) {
				if err := s.send(s.movie(p, false)); err != nil {
					return err
				}
			}
			continue
		}
		var err error
		switch classify(p) {
		case models.LibraryMusic:
			err = s.musicDir(p)
		case models.LibraryTVShows:
			err = s.series(p)
		default:
			err = s.movies(p, false)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *scan) musicDir(dir string) error {
	// Artist/Album layouts are handled by treating the folder as a music root.
	if hasFiles(dir, naming.IsAudio) {
		return s.album(dir, s.entries(dir))
	}
	return s.music(dir)
}

func classify(dir string) string {
	list, _ := os.ReadDir(dir)
	audio, video := 0, 0
	for _, e := range list {
		switch {
		case e.IsDir():
			if _, ok := naming.SeasonFromDir(e.Name()); ok {
				return models.LibraryTVShows
			}
			if hasFiles(filepath.Join(dir, e.Name()), naming.IsAudio) {
				audio++
			}
		case naming.IsAudio(e.Name()):
			audio++
		case naming.IsVideo(e.Name()):
			video++
			if naming.ParseEpisode(naming.Base(e.Name()), nil).Matched {
				return models.LibraryTVShows
			}
		}
	}
	if audio > 0 && video == 0 {
		return models.LibraryMusic
	}
	return models.LibraryMovies
}

func hasFiles(dir string, match func(string) bool) bool {
	list, _ := os.ReadDir(dir)
	for _, e := range list {
		if !e.IsDir() && match(e.Name()) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// within reports whether child is parent or lies below it.
func within(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func hasDiscFolders(dir string, list []os.DirEntry) bool {
	for _, e := range list {
		if _, ok := naming.DiscFromDir(e.Name()); ok && e.IsDir() && hasFiles(filepath.Join(dir, e.Name()), naming.IsAudio) {
			return true
		}
	}
	return false
}
