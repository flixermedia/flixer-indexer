// Package images finds artwork next to media, downloads remote artwork and
// decides where it is stored.
package images

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	_ "golang.org/x/image/webp"

	"github.com/flixermedia/flixer-indexer/internal/mediaid"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/naming"
)

const (
	maxImageBytes = 25 << 20
	minDimension  = 32
)

// Store downloads and places artwork.
type Store struct {
	CachePath string
	FFmpeg    string
	UserAgent string
	HTTP      *http.Client
}

func NewStore(cachePath, ffmpeg, userAgent string) *Store {
	return &Store{CachePath: cachePath, FFmpeg: ffmpeg, UserAgent: userAgent, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

var typeNames = map[string]string{
	"poster": models.ImagePoster, "folder": models.ImagePoster, "cover": models.ImagePoster, "default": models.ImagePoster, "front": models.ImagePoster,
	"album_cover": models.ImagePoster, "album-cover": models.ImagePoster, "albumcover": models.ImagePoster, "albumart": models.ImagePoster, "album": models.ImagePoster,
	"fanart": models.ImageBackdrop, "backdrop": models.ImageBackdrop, "background": models.ImageBackdrop, "art": models.ImageBackdrop,
	"logo": models.ImageLogo, "clearlogo": models.ImageLogo,
	"banner": models.ImageBanner,
	"thumb":  models.ImageThumb, "landscape": models.ImageThumb,
	"disc": models.ImageDisc, "cdart": models.ImageDisc,
}

// classify maps a base name such as "fanart2" to an image type.
func classify(name string) (string, bool) {
	name = strings.ToLower(strings.TrimRight(name, "0123456789"))
	t, ok := typeNames[name]
	return t, ok
}

// FindLocal returns artwork that already sits next to the item.
func FindLocal(item models.Item, folderRoot string) []models.Image {
	var out []models.Image
	add := func(dir, prefix string, seasonOnly *int) {
		list, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range list {
			if e.IsDir() || !naming.IsImage(e.Name()) {
				continue
			}
			base := naming.Base(e.Name())
			if prefix != "" {
				rest, ok := strings.CutPrefix(strings.ToLower(base), strings.ToLower(prefix))
				if !ok {
					continue
				}
				base = strings.TrimLeft(rest, "-. _")
				if base == "" {
					base = "thumb"
					if item.Type != models.ItemEpisode && item.Type != models.ItemVideo {
						base = "poster"
					}
				}
			}
			if t, ok := classify(base); ok {
				out = append(out, models.Image{Type: t, Path: filepath.Join(dir, e.Name()), Provider: "local", Season: seasonOnly})
			}
		}
	}

	switch item.Type {
	case models.ItemSeries, models.ItemAlbum:
		add(item.Path, "", nil)
	case models.ItemSeason:
		if st, err := os.Stat(item.Path); err == nil && st.IsDir() {
			add(item.Path, "", item.SeasonNumber)
		}
		if item.SeasonNumber != nil && item.ParentPath != "" {
			add(item.ParentPath, seasonPrefix(*item.SeasonNumber), item.SeasonNumber)
		}
	default:
		dir := filepath.Dir(item.Path)
		add(dir, naming.Base(item.Path), nil)
		if item.Type == models.ItemMovie && !sameDir(dir, folderRoot) {
			add(dir, "", nil)
		}
	}
	return dedupe(out)
}

func dedupe(in []models.Image) []models.Image {
	seen := map[string]bool{}
	var out []models.Image
	for _, i := range in {
		if !seen[i.Path] {
			seen[i.Path] = true
			out = append(out, i)
		}
	}
	return out
}

func seasonPrefix(n int) string {
	if n == 0 {
		return "season-specials"
	}
	return fmt.Sprintf("season%02d", n)
}

func sameDir(a, b string) bool { return b != "" && filepath.Clean(a) == filepath.Clean(b) }

// Placement says where a downloaded image goes.
type Placement struct {
	Library    models.Library
	Item       models.Item
	FolderRoot string
}

// Save downloads img and writes it per the library's storage policy. index
// distinguishes multiple images of one type. Alongside storage falls back to
// the cache when the media folder is read-only.
func (s *Store) Save(ctx context.Context, p Placement, img models.Image, index int) (models.Image, error) {
	if _, err := mediaid.Key(p.Item.RootID, p.Item.RelativePath); err == nil {
		path, cfg, ok, err := s.cachedImage(p, img.Type, index)
		if err != nil {
			return img, err
		}
		if ok {
			if p.Library.ImageStorage == models.StorageAlongside {
				data, err := os.ReadFile(path)
				if err != nil {
					return img, err
				}
				dest := alongsidePath(p, img, index, filepath.Ext(path)[1:])
				if err := writeAtomic(dest, data); err == nil {
					path = dest
				}
			}
			img.Path, img.URL = path, ""
			img.Width, img.Height = cfg.Width, cfg.Height
			return img, nil
		}
	}
	data, ext, cfg, err := s.fetch(ctx, img.URL)
	if err != nil {
		return img, err
	}
	img.Width, img.Height = cfg.Width, cfg.Height
	return s.place(p, img, index, ext, data)
}

// SaveBytes stores already-downloaded image bytes (e.g. embedded album art).
func (s *Store) SaveBytes(p Placement, img models.Image, index int, data []byte) (models.Image, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return img, fmt.Errorf("not an image: %w", err)
	}
	img.Width, img.Height = cfg.Width, cfg.Height
	return s.place(p, img, index, extFor(format), data)
}

func (s *Store) place(p Placement, img models.Image, index int, ext string, data []byte) (models.Image, error) {
	if p.Library.ImageStorage == models.StorageAlongside {
		dest := alongsidePath(p, img, index, ext)
		if err := writeAtomic(dest, data); err == nil {
			img.Path, img.URL = dest, ""
			return img, nil
		}
	}
	dest, err := s.cachePath(p, img.Type, index, ext)
	if err != nil {
		return img, err
	}
	if err := ensureCacheDirectory(filepath.Dir(dest)); err != nil {
		return img, err
	}
	if err := writeAtomic(dest, data); err != nil {
		return img, err
	}
	img.Path, img.URL = dest, ""
	return img, nil
}

func (s *Store) cachedImage(p Placement, imgType string, index int) (string, image.Config, bool, error) {
	for _, ext := range []string{"jpg", "jpeg", "png", "webp", "gif"} {
		path, err := s.cachePath(p, imgType, index, ext)
		if err != nil {
			return "", image.Config{}, false, err
		}
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", image.Config{}, false, err
		}
		cfg, _, decodeErr := image.DecodeConfig(file)
		closeErr := file.Close()
		if closeErr != nil {
			return "", image.Config{}, false, closeErr
		}
		if decodeErr == nil {
			return path, cfg, true, nil
		}
	}
	return "", image.Config{}, false, nil
}

func (s *Store) fetch(ctx context.Context, url string) ([]byte, string, image.Config, error) {
	var cfg image.Config
	if url == "" {
		return nil, "", cfg, errors.New("image has no url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", cfg, err
	}
	if s.UserAgent != "" {
		req.Header.Set("User-Agent", s.UserAgent)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, "", cfg, fmt.Errorf("download image: %s", redact(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", cfg, fmt.Errorf("download image: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", cfg, err
	}
	if len(data) > maxImageBytes {
		return nil, "", cfg, errors.New("image too large")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", cfg, fmt.Errorf("downloaded file is not an image: %w", err)
	}
	if cfg.Width < minDimension || cfg.Height < minDimension {
		return nil, "", cfg, fmt.Errorf("image too small (%dx%d)", cfg.Width, cfg.Height)
	}
	return data, extFor(format), cfg, nil
}

// redact keeps query strings (which can carry API keys) out of error text.
func redact(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "?"); i >= 0 {
		if j := strings.IndexAny(msg[i:], " \""); j >= 0 {
			return msg[:i] + msg[i+j:]
		}
		return msg[:i]
	}
	return msg
}

func extFor(format string) string {
	if format == "jpeg" {
		return "jpg"
	}
	return format
}

func alongsidePath(p Placement, img models.Image, index int, ext string) string {
	suffix := ""
	if index > 0 {
		suffix = fmt.Sprint(index)
	}
	name := img.Type + suffix
	item := p.Item
	switch item.Type {
	case models.ItemSeries, models.ItemAlbum:
		return filepath.Join(item.Path, name+"."+ext)
	case models.ItemSeason:
		n := 0
		if item.SeasonNumber != nil {
			n = *item.SeasonNumber
		}
		return filepath.Join(item.ParentPath, seasonPrefix(n)+"-"+name+"."+ext)
	}
	dir := filepath.Dir(item.Path)
	if item.Type == models.ItemMovie && !sameDir(dir, p.FolderRoot) {
		return filepath.Join(dir, name+"."+ext)
	}
	return filepath.Join(dir, naming.Base(item.Path)+"-"+name+"."+ext)
}

func (s *Store) cachePath(p Placement, imgType string, index int, ext string) (string, error) {
	key, err := mediaid.Key(p.Item.RootID, p.Item.RelativePath)
	if err != nil {
		return "", fmt.Errorf("cache artwork: %w", err)
	}
	name := imgType
	if index > 0 {
		name += fmt.Sprint(index)
	}
	return filepath.Join(s.CachePath, "images", key, name+"."+ext), nil
}

func writeAtomic(dest string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".flixer-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}

	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, dest); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

func ensureCacheDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o020 == 0 || info.Mode()&os.ModeSetgid == 0 {
		return os.Chmod(directory, 0o2775)
	}
	return nil
}

// ExtractEmbedded pulls an audio file's attached picture out as JPEG bytes.
func (s *Store) ExtractEmbedded(ctx context.Context, file string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.FFmpeg, "-v", "error", "-i", file, "-an", "-map", "0:v:0", "-frames:v", "1", "-f", "image2pipe", "-c:v", "mjpeg", "-")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("extract embedded art: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if out.Len() == 0 {
		return nil, errors.New("no embedded art")
	}
	return out.Bytes(), nil
}
