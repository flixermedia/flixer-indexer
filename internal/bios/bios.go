// Package bios looks up short biographies for people (music artists, actors, directors) on Wikipedia.
package bios

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
)

const (
	defaultBase = "https://en.wikipedia.org/api/rest_v1/page/summary/"
	maxBioRunes = 2500
	// KindArtist is a music artist; any other kind is treated as a film/TV person.
	KindArtist = "artist"
)

// Bio is a person's description and biography.
type Bio struct {
	Description string `json:"description"`
	Biography   string `json:"biography"`
	URL         string `json:"url"`
	Source      string `json:"source"`
	ImageURL    string `json:"imageUrl,omitempty"`
}

type Client struct {
	base string
	http *httpx.Client
}

func New(userAgent string) *Client {
	return &Client{base: defaultBase, http: httpx.New("wikipedia", userAgent, 250*time.Millisecond)}
}

func (c *Client) WithBase(base string) *Client {
	c.base = base
	return c
}

type summary struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Extract     string `json:"extract"`
	ContentURLs struct {
		Desktop struct {
			Page string `json:"page"`
		} `json:"desktop"`
	} `json:"content_urls"`
}

var (
	artistSuffixes = []string{"", " (musician)", " (singer)", " (singer-songwriter)", " (rapper)", " (band)", " (American band)", " (group)"}
	filmSuffixes   = []string{"", " (actor)", " (actress)", " (film director)", " (director)", " (filmmaker)", " (producer)", " (screenwriter)", " (writer)"}
	artistWords    = []string{"singer", "musician", "rapper", "songwriter", "band", "duo", "group", "composer", "vocalist", "guitarist", "dj", "record producer", "pianist", "drummer"}
	filmWords      = []string{"actor", "actress", "director", "filmmaker", "producer", "screenwriter", "writer", "comedian", "presenter", "cinematographer", "composer", "showrunner", "animator", "personality"}
)

// Lookup finds the Wikipedia article for name. It only accepts an article whose title is the person's name and
// whose one-line description matches the kind of work, so namesakes are skipped. It returns httpx.ErrNotFound
// when nothing fits.
func (c *Client) Lookup(ctx context.Context, name, kind string) (*Bio, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || len(name) > 200 {
		return nil, httpx.ErrNotFound
	}
	suffixes, words := filmSuffixes, filmWords
	if kind == KindArtist {
		suffixes, words = artistSuffixes, artistWords
	}
	for _, suffix := range suffixes {
		var s summary
		title := strings.ReplaceAll(name+suffix, " ", "_")
		err := c.http.GetJSON(ctx, c.base+url.PathEscape(title), nil, &s)
		if errors.Is(err, httpx.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if s.Type != "standard" || !strings.EqualFold(stripQualifier(s.Title), name) || !mentions(s.Description, words) {
			continue
		}
		bio := truncate(strings.TrimSpace(s.Extract))
		if bio == "" {
			continue
		}
		return &Bio{Description: s.Description, Biography: bio, URL: s.ContentURLs.Desktop.Page, Source: "Wikipedia"}, nil
	}
	return nil, httpx.ErrNotFound
}

func stripQualifier(title string) string {
	if i := strings.LastIndex(title, " ("); i > 0 && strings.HasSuffix(title, ")") {
		return title[:i]
	}
	return title
}

func mentions(description string, words []string) bool {
	description = strings.ToLower(description)
	for _, word := range words {
		if strings.Contains(description, word) {
			return true
		}
	}
	return false
}

func truncate(text string) string {
	runes := []rune(text)
	if len(runes) <= maxBioRunes {
		return text
	}
	return strings.TrimSpace(string(runes[:maxBioRunes])) + "…"
}
