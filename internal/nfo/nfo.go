// Package nfo reads Kodi/Jellyfin style .nfo files, which hold locally curated
// metadata that always beats what remote providers say.
package nfo

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

const maxSize = 4 << 20

// Data is what an NFO file declared.
type Data struct {
	Metadata models.Metadata
	IDs      map[string]string
	Season   *int
	Episode  *int
}

type document struct {
	Title         string   `xml:"title"`
	OriginalTitle string   `xml:"originaltitle"`
	SortTitle     string   `xml:"sorttitle"`
	Plot          string   `xml:"plot"`
	Outline       string   `xml:"outline"`
	Review        string   `xml:"review"`
	Tagline       string   `xml:"tagline"`
	Year          string   `xml:"year"`
	Premiered     string   `xml:"premiered"`
	Aired         string   `xml:"aired"`
	ReleaseDate   string   `xml:"releasedate"`
	MPAA          string   `xml:"mpaa"`
	Certification string   `xml:"certification"`
	Runtime       string   `xml:"runtime"`
	Status        string   `xml:"status"`
	Label         string   `xml:"label"`
	Studios       []string `xml:"studio"`
	Genres        []string `xml:"genre"`
	Tags          []string `xml:"tag"`
	Directors     []string `xml:"director"`
	Writers       []string `xml:"credits"`
	Countries     []string `xml:"country"`
	Artists       []string `xml:"artist"`
	Set           struct {
		Name string `xml:"name"`
	} `xml:"set"`
	Rating  string `xml:"rating"`
	Ratings []struct {
		Name  string `xml:"name,attr"`
		Max   string `xml:"max,attr"`
		Value string `xml:"value"`
	} `xml:"ratings>rating"`
	ID        string `xml:"id"`
	TMDBID    string `xml:"tmdbid"`
	IMDBID    string `xml:"imdbid"`
	TVDBID    string `xml:"tvdbid"`
	AniDBID   string `xml:"anidbid"`
	UniqueIDs []struct {
		Type  string `xml:"type,attr"`
		Value string `xml:",chardata"`
	} `xml:"uniqueid"`
	Actors []struct {
		Name  string `xml:"name"`
		Role  string `xml:"role"`
		Order string `xml:"order"`
		Thumb string `xml:"thumb"`
	} `xml:"actor"`
	Season  string `xml:"season"`
	Episode string `xml:"episode"`
}

// Read parses the NFO file at path.
func Read(path string) (*Data, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxSize))
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse parses NFO content.
func Parse(raw []byte) (*Data, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	// Kodi writes a plain-text URL after the XML for some scrapers; the
	// decoder only reads the first element, so that trailer is ignored.
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	decoder.Strict = false
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	var doc document
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse nfo: %w", err)
	}
	return convert(doc), nil
}

func convert(d document) *Data {
	meta := models.Metadata{
		Title:         strings.TrimSpace(d.Title),
		OriginalTitle: strings.TrimSpace(d.OriginalTitle),
		SortTitle:     strings.TrimSpace(d.SortTitle),
		Overview:      firstNonEmpty(d.Plot, d.Review, d.Outline),
		Tagline:       strings.TrimSpace(d.Tagline),
		ContentRating: firstNonEmpty(d.MPAA, d.Certification),
		Status:        strings.TrimSpace(d.Status),
		Label:         strings.TrimSpace(d.Label),
		Genres:        trimAll(d.Genres),
		Studios:       trimAll(d.Studios),
		Tags:          trimAll(d.Tags),
		Collection:    strings.TrimSpace(d.Set.Name),
	}
	if countries := trimAll(d.Countries); len(countries) > 0 {
		meta.Country = countries[0]
	}
	if artists := trimAll(d.Artists); len(artists) > 0 {
		meta.Artist, meta.AlbumArtist = artists[0], artists[0]
	}
	meta.PremiereDate = firstNonEmpty(d.Premiered, d.Aired, d.ReleaseDate)
	meta.Year = atoi(d.Year)
	if meta.Year == 0 && len(meta.PremiereDate) >= 4 {
		meta.Year = atoi(meta.PremiereDate[:4])
	}
	if fields := strings.Fields(d.Runtime); len(fields) > 0 {
		meta.Runtime = atoi(fields[0])
	}

	for i, a := range d.Actors {
		if strings.TrimSpace(a.Name) == "" {
			continue
		}
		order := i
		if a.Order != "" {
			order = atoi(a.Order)
		}
		meta.Cast = append(meta.Cast, models.Person{Name: strings.TrimSpace(a.Name), Type: "actor", Role: strings.TrimSpace(a.Role), Order: order, Image: strings.TrimSpace(a.Thumb)})
	}
	for _, name := range trimAll(d.Directors) {
		meta.Crew = append(meta.Crew, models.Person{Name: name, Type: "director"})
	}
	for _, name := range trimAll(d.Writers) {
		meta.Crew = append(meta.Crew, models.Person{Name: name, Type: "writer"})
	}

	ratings := map[string]float64{}
	for _, r := range d.Ratings {
		if value, err := strconv.ParseFloat(strings.TrimSpace(r.Value), 64); err == nil && value > 0 {
			ratings[strings.ToLower(firstNonEmpty(r.Name, "default"))] = scaleToTen(value, r.Max)
		}
	}
	if value, err := strconv.ParseFloat(strings.TrimSpace(d.Rating), 64); err == nil && value > 0 && len(ratings) == 0 {
		ratings["default"] = value
	}
	if len(ratings) > 0 {
		meta.Ratings = ratings
	}

	ids := map[string]string{}
	add := func(kind, value string) {
		if value = strings.TrimSpace(value); value != "" {
			ids[strings.ToLower(kind)] = value
		}
	}
	for _, u := range d.UniqueIDs {
		add(u.Type, u.Value)
	}
	add("tmdb", d.TMDBID)
	add("imdb", d.IMDBID)
	add("tvdb", d.TVDBID)
	add("anidb", d.AniDBID)
	if id := strings.TrimSpace(d.ID); strings.HasPrefix(id, "tt") {
		add("imdb", id)
	}

	data := &Data{Metadata: meta, IDs: ids}
	if d.Season != "" {
		n := atoi(d.Season)
		data.Season = &n
	}
	if d.Episode != "" {
		n := atoi(d.Episode)
		data.Episode = &n
	}
	return data
}

// Complete reports whether the NFO already carries enough that asking remote
// providers is unnecessary.
func (d *Data) Complete() bool {
	m := d.Metadata
	return m.Title != "" && m.Overview != "" && m.Year > 0 && len(m.Genres) > 0
}

func scaleToTen(value float64, max string) float64 {
	if m, err := strconv.ParseFloat(max, 64); err == nil && m > 0 && m != 10 {
		return value / m * 10
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func trimAll(values []string) []string {
	var out []string
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}
