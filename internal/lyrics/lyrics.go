// Package lyrics finds LRC lyrics next to audio files and downloads missing
// ones from lrclib.net, saving them next to the audio, or under DATA_DIR/cache/lyrics/<track id>.lrc when that folder is read-only.
package lyrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	defaultBase = "https://lrclib.net/api"
	maxBody     = 2 << 20
)

// Fetcher downloads lyrics from an lrclib-compatible API.
type Fetcher struct {
	BaseURL   string
	UserAgent string
	HTTP      *http.Client

	mu   sync.Mutex
	next time.Time
}

const (
	requestGap     = 300 * time.Millisecond
	maxRateRetries = 4
	maxRetryWait   = 30 * time.Second
)

// pace spaces requests out so a burst of new tracks does not trip lrclib's rate limit.
func (f *Fetcher) pace(ctx context.Context) error {
	f.mu.Lock()
	now := time.Now()
	at := f.next
	if at.Before(now) {
		at = now
	}
	f.next = at.Add(requestGap)
	f.mu.Unlock()
	return sleep(ctx, time.Until(at))
}

// holdOff pushes every queued request back after lrclib answers 429.
func (f *Fetcher) holdOff(d time.Duration) {
	f.mu.Lock()
	if until := time.Now().Add(d); until.After(f.next) {
		f.next = until
	}
	f.mu.Unlock()
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func retryAfter(h string, attempt int) time.Duration {
	wait := 2 * time.Second << attempt
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		wait = time.Duration(secs) * time.Second
	}
	return min(wait, maxRetryWait)
}

func NewFetcher(userAgent string) *Fetcher {
	return &Fetcher{BaseURL: defaultBase, UserAgent: userAgent, HTTP: &http.Client{Timeout: 20 * time.Second}}
}

// Query identifies a track.
type Query struct {
	Artist, AlbumArtist, Title, Album string
	Duration                          float64
}

// Path returns the sidecar LRC path for an audio file.
func Path(audio string) string {
	return strings.TrimSuffix(audio, filepath.Ext(audio)) + ".lrc"
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// StorePath is where downloaded lyrics for a track live, so read-only libraries still get them.
// It returns "" when the id or directory is unusable.
func StorePath(dir, id string) string {
	if dir == "" || !safeID.MatchString(id) {
		return ""
	}
	return filepath.Join(dir, id+".lrc")
}

func nonEmpty(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Size() > 0
}

// Exists reports whether a sidecar lyrics file is already present.
func Exists(audio string) bool {
	st, err := os.Stat(Path(audio))
	return err == nil && !st.IsDir() && st.Size() > 0
}

type response struct {
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	SyncedLyrics string  `json:"syncedLyrics"`
	Instrumental bool    `json:"instrumental"`
}

func (r response) usable() bool {
	return !r.Instrumental && strings.TrimSpace(r.SyncedLyrics) != ""
}

// ErrNotFound means the service has no synced lyrics for the track.
var ErrNotFound = errors.New("lyrics not found")

// maxDurationDiff is how far (seconds) a search result's length may differ from
// the file's before it is assumed to be a different recording.
const maxDurationDiff = 5.0

var (
	bracketed = regexp.MustCompile(`\s*[\(\[\{][^\)\]\}]*[\)\]\}]`)
	dashTail  = regexp.MustCompile(`\s+-\s+(remaster(ed)?|\d{4}|live|mono|stereo|single|radio|album|version|edit|acoustic|demo|bonus|explicit|clean|feat|ft)\b.*$`)
	featTail  = regexp.MustCompile(`(?i)\s+(feat\.?|ft\.?|featuring)\s+.*$`)
	trackNum  = regexp.MustCompile(`^\s*\d{1,3}\s*[-._)]\s*`)
)

// cleanTitle drops decorations that rarely match lrclib's titles: "(Album Version)",
// "- Remastered 2011", "feat. X" and a leading track number.
func cleanTitle(s string) string {
	s = trackNum.ReplaceAllString(s, "")
	s = bracketed.ReplaceAllString(s, "")
	s = dashTail.ReplaceAllString(strings.ToLower(s), "")
	s = featTail.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// norm reduces text to lowercase letters and digits for comparing names.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// artistVariants lists artist names to try: as given, then the first of a
// "A; B", "A, B", "A & B" or "A feat. B" credit.
func artistVariants(names ...string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if k := norm(s); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}
	for _, n := range names {
		add(n)
		if i := strings.IndexAny(n, ";,/&"); i > 0 {
			add(n[:i])
		}
		add(featTail.ReplaceAllString(n, ""))
	}
	return out
}

func titleVariants(title string) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range []string{strings.TrimSpace(title), cleanTitle(title)} {
		if k := norm(t); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, t)
		}
	}
	return out
}

func (f *Fetcher) get(ctx context.Context, endpoint string, params url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		if err := f.pace(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.BaseURL, "/")+endpoint+"?"+params.Encode(), nil)
		if err != nil {
			return err
		}
		if f.UserAgent != "" {
			req.Header.Set("User-Agent", f.UserAgent)
		}
		resp, err := f.HTTP.Do(req)
		if err != nil {
			return err
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			resp.Body.Close()
			return ErrNotFound
		case resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateRetries:
			wait := retryAfter(resp.Header.Get("Retry-After"), attempt)
			resp.Body.Close()
			f.holdOff(wait)
			continue
		case resp.StatusCode != http.StatusOK:
			resp.Body.Close()
			if resp.StatusCode >= 500 {
				f.holdOff(retryAfter("", 1))
			}
			return fmt.Errorf("lrclib: unexpected status %d", resp.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out)
		resp.Body.Close()
		return err
	}
}

// exact asks lrclib for the one record matching the tags; it is the most
// precise lookup but misses whenever the album or length differ slightly.
func (f *Fetcher) exact(ctx context.Context, artist, title string, q Query, withAlbum bool) (string, error) {
	params := url.Values{}
	params.Set("artist_name", artist)
	params.Set("track_name", title)
	if withAlbum && q.Album != "" {
		params.Set("album_name", q.Album)
	}
	if q.Duration > 0 {
		params.Set("duration", strconv.Itoa(int(q.Duration+0.5)))
	}
	var out response
	if err := f.get(ctx, "/get", params, &out); err != nil {
		return "", err
	}
	if !out.usable() {
		return "", ErrNotFound
	}
	return out.SyncedLyrics, nil
}

// search looks up candidates and picks the best plausible one: the title and
// artist must agree and, when both lengths are known, the recording must be
// about as long as the file.
func (f *Fetcher) search(ctx context.Context, params url.Values, artists []string, title string, q Query) (string, error) {
	var results []response
	if err := f.get(ctx, "/search", params, &results); err != nil {
		return "", err
	}
	wantTitle := norm(cleanTitle(title))
	best, bestScore := "", -1.0
	for _, r := range results {
		if !r.usable() {
			continue
		}
		gotTitle := norm(cleanTitle(r.TrackName))
		if gotTitle == "" || (gotTitle != wantTitle && !strings.Contains(gotTitle, wantTitle) && !strings.Contains(wantTitle, gotTitle)) {
			continue
		}
		gotArtist := norm(r.ArtistName)
		artistScore := 0.0
		for _, a := range artists {
			na := norm(a)
			switch {
			case na == gotArtist:
				artistScore = max(artistScore, 3)
			case na != "" && gotArtist != "" && (strings.Contains(gotArtist, na) || strings.Contains(na, gotArtist)):
				artistScore = max(artistScore, 1)
			}
		}
		if artistScore == 0 {
			continue
		}
		score := artistScore
		if gotTitle == wantTitle {
			score += 2
		}
		if q.Album != "" && norm(r.AlbumName) == norm(q.Album) {
			score++
		}
		if q.Duration > 0 && r.Duration > 0 {
			diff := math.Abs(r.Duration - q.Duration)
			if diff > maxDurationDiff {
				continue
			}
			score += (maxDurationDiff - diff) / maxDurationDiff * 2
		}
		if score > bestScore {
			best, bestScore = r.SyncedLyrics, score
		}
	}
	if best == "" {
		return "", ErrNotFound
	}
	return best, nil
}

// Fetch returns synced lyrics for the track. It tries the exact lookup, then
// searches with progressively looser names (decorations removed, other artist
// credits), and finally a title-only search checked against the artist.
func (f *Fetcher) Fetch(ctx context.Context, q Query) (string, error) {
	artists := artistVariants(q.Artist, q.AlbumArtist)
	titles := titleVariants(q.Title)
	if len(titles) == 0 || len(artists) == 0 {
		return "", ErrNotFound
	}
	var firstErr error
	try := func(text string, err error) (string, bool) {
		if err == nil {
			return text, true
		}
		if !errors.Is(err, ErrNotFound) && firstErr == nil {
			firstErr = err
		}
		return "", false
	}

	for _, withAlbum := range []bool{true, false} {
		if text, ok := try(f.exact(ctx, artists[0], titles[0], q, withAlbum)); ok {
			return text, nil
		}
	}
	for _, artist := range artists {
		for _, title := range titles {
			params := url.Values{}
			params.Set("artist_name", artist)
			params.Set("track_name", title)
			if text, ok := try(f.search(ctx, params, artists, q.Title, q)); ok {
				return text, nil
			}
		}
	}
	params := url.Values{}
	params.Set("q", titles[len(titles)-1])
	if text, ok := try(f.search(ctx, params, artists, q.Title, q)); ok {
		return text, nil
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", ErrNotFound
}

// Ensure makes sure lyrics exist for the track, either as a sidecar next to
// the audio or in the store directory under the track's id. It returns true
// when lyrics are available afterwards. A download is attempted only when f is
// set, and is saved next to the audio when that folder is writable, else to the store.
func Ensure(ctx context.Context, f *Fetcher, audio, storeDir, id string, q Query) (bool, error) {
	store := StorePath(storeDir, id)
	if Exists(audio) || (store != "" && nonEmpty(store)) {
		return true, nil
	}
	if f == nil {
		return false, nil
	}
	text, err := f.Fetch(ctx, q)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Next to the audio is preferred so the lyrics travel with the library; a read-only share falls back to the store.
	sidecarErr := writeAtomic(filepath.Dir(audio), Path(audio), text)
	if sidecarErr == nil {
		return true, nil
	}
	if store == "" {
		return false, sidecarErr
	}
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return false, err
	}
	if err := writeAtomic(storeDir, store, text); err != nil {
		return false, err
	}
	return true, nil
}

func writeAtomic(dir, dest, text string) error {
	tmp, err := os.CreateTemp(dir, ".lrc-*")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(text)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), dest)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}
