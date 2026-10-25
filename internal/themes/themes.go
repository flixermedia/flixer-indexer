// Package themes downloads theme songs listed in ThemerrDB (the database behind
// LizardByte's Themerr) and decides where they are stored.
package themes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/mediaid"
	"github.com/flixermedia/flixer-indexer/internal/models"
)

const (
	// FileName is the theme file written beside media, the name Plex and Themerr use.
	FileName = "theme.mp3"

	defaultDBURL = "https://app.lizardbyte.dev/ThemerrDB"
	missMarker   = "theme.miss"
	missTTL      = 30 * 24 * time.Hour
	maxEntry     = 1 << 20
)

var videoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// Task asks for one item's theme. TMDBID and ItemType (movie or series) pick the ThemerrDB entry.
type Task struct {
	LibraryID    string `json:"libraryId"`
	Path         string `json:"path"`
	RootID       string `json:"rootId"`
	RelativePath string `json:"relativePath"`
	ItemType     string `json:"itemType"`
	TMDBID       string `json:"tmdbId"`
}

// Runner executes an external program; tests replace it.
type Runner func(ctx context.Context, name string, args []string) ([]byte, error)

// Fetcher downloads themes.
type Fetcher struct {
	// CacheDir is the root of the indexer's media cache.
	CacheDir  string
	FFmpeg    string
	UserAgent string
	HTTP      *http.Client
	DBURL     string
	YTDLP     *YTDLP
	Run       Runner
}

func NewFetcher(cacheDir, ffmpeg, userAgent string) *Fetcher {
	return &Fetcher{
		CacheDir: cacheDir, FFmpeg: ffmpeg, UserAgent: userAgent, HTTP: &http.Client{Timeout: 30 * time.Second}, DBURL: defaultDBURL,
		YTDLP: NewYTDLP(filepath.Join(cacheDir, "tools"), userAgent),
	}
}

// Paths returns where an item's theme may live: beside the media ("" when the
// item has no folder of its own) and under cache/theme/{media-id}.
func (f *Fetcher) Paths(lib models.Library, t Task, folderRoot string) (alongside, cached string) {
	key, err := mediaid.Key(t.RootID, t.RelativePath)
	if err != nil {
		return "", ""
	}
	cached = filepath.Join(f.CacheDir, "theme", key, FileName)
	dir := t.Path
	if t.ItemType == models.ItemMovie {
		dir = filepath.Dir(t.Path)
	}
	if dir == "" || (folderRoot != "" && filepath.Clean(dir) == filepath.Clean(folderRoot)) {
		return "", cached
	}
	return filepath.Join(dir, FileName), cached
}

// Locate returns the existing theme file for an item, or "".
func (f *Fetcher) Locate(lib models.Library, t Task, folderRoot string) string {
	alongside, cached := f.Paths(lib, t, folderRoot)
	if alongside != "" && isFile(alongside) {
		return alongside
	}
	if isFile(cached) {
		return cached
	}
	return ""
}

func isFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// Fetch downloads the theme unless one exists or ThemerrDB has none. It reports whether a file was written.
func (f *Fetcher) Fetch(ctx context.Context, lib models.Library, t Task, folderRoot string) (bool, error) {
	if t.TMDBID == "" || (t.ItemType != models.ItemMovie && t.ItemType != models.ItemSeries) {
		return false, nil
	}
	if _, err := mediaid.Key(t.RootID, t.RelativePath); err != nil {
		return false, fmt.Errorf("theme cache: %w", err)
	}
	if f.Locate(lib, t, folderRoot) != "" {
		return false, nil
	}
	alongside, cached := f.Paths(lib, t, folderRoot)
	marker := filepath.Join(filepath.Dir(cached), missMarker)
	if st, err := os.Stat(marker); err == nil && time.Since(st.ModTime()) < missTTL {
		return false, nil
	}

	videoURL, err := f.lookup(ctx, t)
	if err != nil {
		return false, err
	}
	if videoURL == "" {
		_ = os.MkdirAll(filepath.Dir(marker), 0o755)
		_ = os.WriteFile(marker, nil, 0o644)
		return false, nil
	}
	data, err := f.download(ctx, videoURL)
	if err != nil {
		return false, err
	}

	if lib.ImageStorage == models.StorageAlongside && alongside != "" {
		if err := writeAtomic(alongside, data); err == nil {
			return true, nil
		}
	}
	if err := writeAtomic(cached, data); err != nil {
		return false, err
	}
	return true, nil
}

// lookup returns the YouTube watch URL ThemerrDB lists for the item, or "" when there is none.
func (f *Fetcher) lookup(ctx context.Context, t Task) (string, error) {
	kind := "movies"
	if t.ItemType == models.ItemSeries {
		kind = "tv_shows"
	}
	base := f.DBURL
	if base == "" {
		base = defaultDBURL
	}
	endpoint := fmt.Sprintf("%s/%s/themoviedb/%s.json", strings.TrimRight(base, "/"), kind, url.PathEscape(t.TMDBID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("themerrdb: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("themerrdb: status %d", resp.StatusCode)
	}
	var entry struct {
		YouTubeThemeURL string `json:"youtube_theme_url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxEntry)).Decode(&entry); err != nil {
		return "", fmt.Errorf("themerrdb: decode entry: %w", err)
	}
	return NormalizeURL(entry.YouTubeThemeURL), nil
}

// NormalizeURL returns a canonical YouTube watch URL, or "" if raw is anything else.
// ThemerrDB is remote input, so only a bare video id may reach yt-dlp.
func NormalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	id := ""
	switch host {
	case "youtube.com", "m.youtube.com":
		if u.Path == "/watch" {
			id = u.Query().Get("v")
		}
	case "youtu.be":
		id = strings.TrimPrefix(u.Path, "/")
	}
	if !videoID.MatchString(id) {
		return ""
	}
	return "https://www.youtube.com/watch?v=" + id
}

func (f *Fetcher) download(ctx context.Context, videoURL string) ([]byte, error) {
	bin, err := f.YTDLP.Ensure(ctx)
	if err != nil {
		return nil, fmt.Errorf("yt-dlp unavailable: %w", err)
	}
	if err := os.MkdirAll(f.CacheDir, 0o755); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(f.CacheDir, ".theme-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	args := []string{
		"--no-playlist", "--no-progress", "--no-warnings", "--no-config",
		"--max-filesize", "40M", "-x", "--audio-format", "mp3", "--audio-quality", "5",
		"-o", filepath.Join(work, "theme.%(ext)s"),
	}
	if ffmpeg, err := exec.LookPath(f.FFmpeg); err == nil {
		args = append(args, "--ffmpeg-location", ffmpeg)
	}
	args = append(args, "--", videoURL)

	run := f.Run
	if run == nil {
		run = runCommand
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if out, err := run(runCtx, bin, args); err != nil {
		return nil, fmt.Errorf("yt-dlp failed: %w: %s", err, tail(string(out)))
	}
	data, err := os.ReadFile(filepath.Join(work, FileName))
	if err != nil || len(data) == 0 {
		return nil, errors.New("yt-dlp produced no audio")
	}
	return data, nil
}

func runCommand(ctx context.Context, name string, args []string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[len(s)-300:]
	}
	return s
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
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, dest); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
