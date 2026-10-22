// Package subtitles keeps sidecar subtitle files next to video files and
// fetches missing ones from OpenSubtitles.
//
// Sidecars are named <video base>.<language>[.<label>].srt, the convention
// Plex and Jellyfin both read, so files added by hand or by flixer are shared.
package subtitles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Track is a subtitle file beside a video.
type Track struct {
	File     string `json:"file"`
	Language string `json:"language"`
	Label    string `json:"label,omitempty"`
	Format   string `json:"format"`
}

var (
	// ErrInvalid is returned for unusable language codes or file names.
	ErrInvalid  = errors.New("invalid subtitle request")
	langPattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:[-_][A-Za-z0-9]{2,4})?$`)
	formats     = map[string]bool{".srt": true, ".vtt": true}
)

// iso6392 maps three-letter codes seen in container tags to two-letter codes.
var iso6392 = map[string]string{
	"eng": "en", "fre": "fr", "fra": "fr", "ger": "de", "deu": "de", "spa": "es", "ita": "it", "por": "pt",
	"rus": "ru", "jpn": "ja", "kor": "ko", "chi": "zh", "zho": "zh", "dut": "nl", "nld": "nl", "swe": "sv",
	"nor": "no", "dan": "da", "fin": "fi", "pol": "pl", "tur": "tr", "ara": "ar", "hin": "hi", "heb": "he",
	"gre": "el", "ell": "el", "cze": "cs", "ces": "cs", "hun": "hu", "tha": "th", "vie": "vi", "ind": "id",
	"ukr": "uk", "rum": "ro", "ron": "ro",
}

// Primary reduces a language tag to its lowercase two-letter form where known
// ("eng" and "en-US" both become "en").
func Primary(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if i := strings.IndexAny(code, "-_"); i > 0 {
		code = code[:i]
	}
	if short, ok := iso6392[code]; ok {
		return short
	}
	return code
}

// ValidLanguage reports whether code is safe to put in a file name.
func ValidLanguage(code string) bool { return langPattern.MatchString(code) }

func base(video string) string {
	return strings.TrimSuffix(filepath.Base(video), filepath.Ext(video))
}

// List returns the subtitle files beside video, ordered by language then name.
func List(video string) ([]Track, error) {
	dir := filepath.Dir(video)
	prefix := base(video) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var tracks []Track
	for _, e := range entries {
		name := e.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if e.IsDir() || !formats[ext] || !strings.HasPrefix(name, prefix) {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(name, prefix), filepath.Ext(name)), ".")
		track := Track{File: name, Format: strings.TrimPrefix(ext, "."), Language: "und"}
		if ValidLanguage(parts[0]) {
			track.Language = parts[0]
			parts = parts[1:]
		}
		track.Label = strings.Join(parts, " ")
		tracks = append(tracks, track)
	}
	sort.Slice(tracks, func(i, j int) bool {
		if tracks[i].Language != tracks[j].Language {
			return tracks[i].Language < tracks[j].Language
		}
		return tracks[i].File < tracks[j].File
	})
	return tracks, nil
}

// Has reports whether a sidecar for the language already exists.
func Has(tracks []Track, language string) bool {
	want := Primary(language)
	for _, t := range tracks {
		if Primary(t.Language) == want {
			return true
		}
	}
	return false
}

// Save writes data as a new sidecar for the language and returns its path. An
// existing file is never overwritten; a numeric label is added instead.
func Save(video, language string, data []byte) (string, error) {
	if !ValidLanguage(language) {
		return "", fmt.Errorf("%w: language %q", ErrInvalid, language)
	}
	text := toUTF8(data)
	dir := filepath.Dir(video)
	for n := 1; n < 100; n++ {
		name := base(video) + "." + language
		if n > 1 {
			name += fmt.Sprintf(".%d", n)
		}
		path := filepath.Join(dir, name+".srt")
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.WriteString(text)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			_ = os.Remove(path)
			return "", werr
		}
		return path, nil
	}
	return "", fmt.Errorf("%w: too many subtitle files", ErrInvalid)
}

// Remove deletes one sidecar by file name. The name must be a subtitle that
// belongs to video, so this cannot remove arbitrary files.
func Remove(video, file string) error {
	if file != filepath.Base(file) || !strings.HasPrefix(file, base(video)+".") || !formats[strings.ToLower(filepath.Ext(file))] {
		return fmt.Errorf("%w: file %q", ErrInvalid, file)
	}
	return os.Remove(filepath.Join(filepath.Dir(video), file))
}

// toUTF8 strips a byte order mark and reinterprets non-UTF-8 text as Latin-1,
// the most common legacy encoding for downloaded subtitles.
func toUTF8(data []byte) string {
	data = []byte(strings.TrimPrefix(string(data), "\ufeff"))
	if utf8.Valid(data) {
		return string(data)
	}
	runes := make([]rune, len(data))
	for i, b := range data {
		runes[i] = rune(b)
	}
	return string(runes)
}
