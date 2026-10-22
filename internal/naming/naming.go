// Package naming extracts what it can from file and folder names, following
// the conventions Jellyfin and Plex document for movies, TV and music.
package naming

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var videoExt = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".wmv": true,
	".mpg": true, ".mpeg": true, ".ts": true, ".m2ts": true, ".webm": true, ".flv": true,
	".iso": false, ".ogv": true, ".3gp": true, ".divx": true,
}

var audioExt = map[string]bool{
	".mp3": true, ".flac": true, ".m4a": true, ".aac": true, ".ogg": true, ".opus": true,
	".wav": true, ".wma": true, ".alac": true, ".ape": true, ".wv": true, ".aiff": true, ".aif": true,
}

var imageExt = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true}

func IsVideo(name string) bool { return videoExt[strings.ToLower(filepath.Ext(name))] }
func IsAudio(name string) bool { return audioExt[strings.ToLower(filepath.Ext(name))] }
func IsImage(name string) bool { return imageExt[strings.ToLower(filepath.Ext(name))] }

// Base returns the file name without its extension.
func Base(name string) string {
	name = filepath.Base(name)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

var (
	idTag       = regexp.MustCompile(`(?i)[\[{(]\s*(tmdb|imdb|tvdb|anidb|anilist|musicbrainz[a-z]*|mbid)(?:id)?[-=:\s]+([A-Za-z0-9-]+)\s*[\]})]`)
	yearPattern = regexp.MustCompile(`(?:^|[\s(\[.])((?:19|20)\d{2})(?:[\s)\].]|$)`)
	releaseTag  = regexp.MustCompile(`(?i)(?:^|[\s.\[(_-])(2160p|1080[pi]|720p|576p|480p|4k|uhd|blu-?ray|bdrip|brrip|webrip|web-?dl|hdtv|dvdrip|dvdscr|remux|x26[45]|h\.?26[45]|hevc|avc|xvid|hdr10\+?|hdr|dovi|dv|10bit|repack|proper|extended|unrated|imax|dts(?:-hd)?|truehd|atmos|aac\d*|ac3|ddp?\d?)(?:$|[\s.\])_-])`)
	separators  = " \t-._([{"
	spaceRun    = regexp.MustCompile(`\s{2,}`)

	episodeSxE   = regexp.MustCompile(`(?i)s(\d{1,3})[\s._-]*e(\d{1,4})(?:[\s._-]*(?:e|-e?)\s*(\d{1,4}))?`)
	episodeNxNN  = regexp.MustCompile(`(?i)(?:^|[^0-9a-z])(\d{1,2})x(\d{2,3})(?:-x?(\d{2,3}))?(?:[^0-9a-z]|$)`)
	episodeBare  = regexp.MustCompile(`^(\d{1,3})(?:\s*[-.]\s*|\s+)`)
	episodeWord  = regexp.MustCompile(`(?i)(?:^|[^0-9a-z])e(?:p(?:isode)?)?[\s._-]?(\d{1,3})(?:[^0-9a-z]|$)`)
	seasonDir    = regexp.MustCompile(`(?i)^(?:season|series|staffel|saison|temporada)[\s._-]*(\d{1,3})$|^s(\d{1,3})$`)
	discDir      = regexp.MustCompile(`(?i)^(?:disc|disk|cd)[\s._-]*(\d{1,2})$`)
	trackFile    = regexp.MustCompile(`^(?:(\d{1,2})[-.](\d{1,3})|(\d{1,3}))\s*[-._ ]+\s*(.+)$`)
	yearLeading  = regexp.MustCompile(`^((?:19|20)\d{2})\s*-\s*(.+)$`)
	artistAlbum  = regexp.MustCompile(`^(.+?)\s+-\s+(.+)$`)
	trailingYear = regexp.MustCompile(`\s*[(\[]((?:19|20)\d{2})[)\]]\s*$`)
)

// Parsed is the outcome of parsing a movie, series or album name.
type Parsed struct {
	Title string
	Year  int
	IDs   map[string]string
}

// ProviderIDs reads Jellyfin-style id tags such as [tmdbid-603] or {imdb-tt0133093}.
func ProviderIDs(name string) map[string]string {
	ids := map[string]string{}
	for _, m := range idTag.FindAllStringSubmatch(name, -1) {
		key := strings.ToLower(m[1])
		if strings.HasPrefix(key, "musicbrainz") || key == "mbid" {
			key = "musicbrainz"
		}
		ids[key] = m[2]
	}
	return ids
}

// ParseTitle parses "Title (Year) [tmdbid-1]" and release-style names like
// "Title.Year.1080p.BluRay.x264".
func ParseTitle(name string) Parsed {
	parsed := Parsed{IDs: ProviderIDs(name)}
	name = idTag.ReplaceAllString(name, " ")
	name = strings.TrimSpace(name)

	if !strings.Contains(name, " ") && strings.ContainsAny(name, "._") {
		name = strings.NewReplacer(".", " ", "_", " ").Replace(name)
	}

	cut := len(name)
	if matches := yearPattern.FindAllStringSubmatchIndex(name, -1); len(matches) > 0 {
		for i := len(matches) - 1; i >= 0; i-- {
			m := matches[i]
			if before := strings.Trim(name[:m[2]], separators); before != "" {
				parsed.Year, _ = strconv.Atoi(name[m[2]:m[3]])
				cut = m[2]
				break
			}
		}
	}
	if loc := releaseTag.FindStringIndex(name); loc != nil && loc[0] < cut && strings.Trim(name[:loc[0]], separators) != "" {
		cut = loc[0]
	}
	title := strings.Trim(name[:cut], separators)
	title = strings.TrimRight(title, " -.([{")
	parsed.Title = spaceRun.ReplaceAllString(title, " ")
	return parsed
}

// Episode is the season and episode numbers found in a file name.
type Episode struct {
	Season  *int
	Number  int
	EndNum  *int
	Title   string
	Matched bool
}

// ParseEpisode reads season and episode numbers from a file's base name. The
// season of the containing folder (from SeasonFromDir) is used when the file
// name only carries an episode number.
func ParseEpisode(base string, dirSeason *int) Episode {
	if m := episodeSxE.FindStringSubmatchIndex(base); m != nil {
		return episodeFrom(base, m, atoiPtr(base, m[2], m[3]))
	}
	if m := episodeNxNN.FindStringSubmatchIndex(base); m != nil {
		return episodeFrom(base, m, atoiPtr(base, m[2], m[3]))
	}
	if dirSeason != nil {
		if m := episodeBare.FindStringSubmatchIndex(base); m != nil {
			return episodeFrom(base, []int{m[0], m[1], -1, -1, m[2], m[3], -1, -1}, dirSeason)
		}
		if m := episodeWord.FindStringSubmatchIndex(base); m != nil {
			return episodeFrom(base, []int{m[0], m[1], -1, -1, m[2], m[3], -1, -1}, dirSeason)
		}
	}
	return Episode{}
}

func episodeFrom(base string, m []int, season *int) Episode {
	// The episode number is always the second capture group of the pattern, but
	// bare patterns pass it in the same slot so the indexes line up.
	number, _ := strconv.Atoi(base[m[4]:m[5]])
	ep := Episode{Season: season, Number: number, Matched: true}
	if len(m) > 7 && m[6] >= 0 && m[7] >= 0 {
		if end, err := strconv.Atoi(base[m[6]:m[7]]); err == nil && end > number {
			ep.EndNum = &end
		}
	}
	rest := base[m[1]:]
	ep.Title = cleanEpisodeTitle(rest)
	return ep
}

func atoiPtr(s string, start, end int) *int {
	n, err := strconv.Atoi(s[start:end])
	if err != nil {
		return nil
	}
	return &n
}

func cleanEpisodeTitle(rest string) string {
	if loc := releaseTag.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	if !strings.Contains(rest, " ") {
		rest = strings.NewReplacer(".", " ", "_", " ").Replace(rest)
	}
	return spaceRun.ReplaceAllString(strings.Trim(rest, separators+")]}"), " ")
}

// SeasonFromDir recognises "Season 02", "S02" and "Specials".
func SeasonFromDir(name string) (int, bool) {
	name = strings.TrimSpace(name)
	if strings.EqualFold(name, "specials") {
		return 0, true
	}
	m := seasonDir.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	value := m[1]
	if value == "" {
		value = m[2]
	}
	n, err := strconv.Atoi(value)
	return n, err == nil
}

// DiscFromDir recognises "Disc 1" and "CD2" folders inside an album.
func DiscFromDir(name string) (int, bool) {
	m := discDir.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// Track is the disc, number and title read from an audio file name.
type Track struct {
	Disc, Number int
	Title        string
}

// ParseTrack reads "01 - Title", "01. Title" and "1-02 Title".
func ParseTrack(base string) Track {
	m := trackFile.FindStringSubmatch(base)
	if m == nil {
		return Track{Title: strings.TrimSpace(base)}
	}
	track := Track{Title: strings.TrimSpace(m[4])}
	if m[1] != "" {
		track.Disc, _ = strconv.Atoi(m[1])
		track.Number, _ = strconv.Atoi(m[2])
	} else {
		track.Number, _ = strconv.Atoi(m[3])
	}
	return track
}

// Album is an album name optionally carrying its artist and year.
type Album struct {
	Artist, Title string
	Year          int
	IDs           map[string]string
}

// ParseAlbum reads "Album (2001)", "2001 - Album" and "Artist - Album (2001)".
func ParseAlbum(name string) Album {
	album := Album{IDs: ProviderIDs(name)}
	name = strings.TrimSpace(idTag.ReplaceAllString(name, " "))
	if m := trailingYear.FindStringSubmatch(name); m != nil {
		album.Year, _ = strconv.Atoi(m[1])
		name = strings.TrimSpace(trailingYear.ReplaceAllString(name, ""))
	}
	if m := yearLeading.FindStringSubmatch(name); m != nil {
		album.Year, _ = strconv.Atoi(m[1])
		name = m[2]
	}
	if m := artistAlbum.FindStringSubmatch(name); m != nil {
		album.Artist, name = strings.TrimSpace(m[1]), m[2]
	}
	album.Title = strings.TrimSpace(name)
	return album
}

// IsExtra reports whether a video file name or folder is bonus material that
// should not be indexed as a feature.
func IsExtra(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "extras", "extra", "trailers", "featurettes", "behind the scenes", "deleted scenes",
		"interviews", "scenes", "shorts", "samples", "sample", "other", "@eadir", "metadata", ".actors":
		return true
	}
	for _, suffix := range []string{"-trailer", "-sample", "-featurette", "-behindthescenes",
		"-deletedscene", "-interview", "-scene", "-short", "-other", ".trailer", ".sample"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return lower == "sample" || lower == "trailer"
}
