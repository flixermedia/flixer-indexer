// Package subtitles keeps sidecar subtitle files next to video files and
// fetches missing ones from OpenSubtitles.
//
// Sidecars are named <video base>.<language>[.<label>].srt, the convention
// Plex and Jellyfin both read, so files added by hand or by flixer are shared.
package subtitles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
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

// Target is a video plus the folder used for subtitles when the media folder
// cannot be written (for example a read-only network share). Fallback may be empty.
type Target struct {
	Video    string
	Fallback string
}

// FallbackDir returns the per-video folder under root, keyed like trickplay so
// flixer-streamer can find it from the playback token alone. Empty without ids.
func FallbackDir(root, rootID, relativePath string) string {
	if root == "" || rootID == "" || relativePath == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(rootID + "\n" + relativePath))
	return filepath.Join(root, hex.EncodeToString(sum[:])[:32])
}

// List returns the subtitle files beside the video and in the fallback folder,
// ordered by language then name.
func List(t Target) ([]Track, error) {
	tracks, err := listDir(filepath.Dir(t.Video), base(t.Video)+".")
	if err != nil {
		return nil, err
	}
	if t.Fallback != "" {
		stored, err := listDir(t.Fallback, base(t.Video)+".")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		for _, s := range stored {
			dup := false
			for _, existing := range tracks {
				dup = dup || existing.File == s.File
			}
			if !dup {
				tracks = append(tracks, s)
			}
		}
	}
	sort.Slice(tracks, func(i, j int) bool {
		if tracks[i].Language != tracks[j].Language {
			return tracks[i].Language < tracks[j].Language
		}
		return tracks[i].File < tracks[j].File
	})
	return tracks, nil
}

func listDir(dir, prefix string) ([]Track, error) {
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
// existing file is never overwritten; a numeric label is added instead. When the
// media folder is not writable the file goes to the fallback folder instead.
func Save(t Target, language string, data []byte) (string, error) {
	if !ValidLanguage(language) {
		return "", fmt.Errorf("%w: language %q", ErrInvalid, language)
	}
	text := toUTF8(data)
	path, err := saveIn(filepath.Dir(t.Video), t.Fallback, base(t.Video), language, text)
	if err != nil && t.Fallback != "" && unwritable(err) {
		if mkErr := os.MkdirAll(t.Fallback, 0o755); mkErr != nil {
			return "", mkErr
		}
		return saveIn(t.Fallback, filepath.Dir(t.Video), base(t.Video), language, text)
	}
	return path, err
}

func unwritable(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.EROFS)
}

// saveIn creates the first free numbered name in dir; other is a second folder
// whose names are also avoided so a stored file never shadows a sidecar.
func saveIn(dir, other, video, language, text string) (string, error) {
	for n := 1; n < 100; n++ {
		name := video + "." + language
		if n > 1 {
			name += fmt.Sprintf(".%d", n)
		}
		name += ".srt"
		if other != "" {
			if _, err := os.Stat(filepath.Join(other, name)); err == nil {
				continue
			}
		}
		path := filepath.Join(dir, name)
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

// Remove deletes one sidecar by file name, from the media folder or else the
// fallback folder. The name must be a subtitle that belongs to the video, so
// this cannot remove arbitrary files.
func Remove(t Target, file string) error {
	if file != filepath.Base(file) || !strings.HasPrefix(file, base(t.Video)+".") || !formats[strings.ToLower(filepath.Ext(file))] {
		return fmt.Errorf("%w: file %q", ErrInvalid, file)
	}
	err := os.Remove(filepath.Join(filepath.Dir(t.Video), file))
	if errors.Is(err, os.ErrNotExist) && t.Fallback != "" {
		return os.Remove(filepath.Join(t.Fallback, file))
	}
	return err
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
