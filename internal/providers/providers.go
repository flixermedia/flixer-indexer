// Package providers defines what a metadata provider looks like and how their
// answers are combined. The providers themselves live in sub-packages.
package providers

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

// Query identifies what to look up.
type Query struct {
	Title    string            `json:"title,omitempty"`
	Year     int               `json:"year,omitempty"`
	IDs      map[string]string `json:"ids,omitempty"`
	Language string            `json:"language,omitempty"`
	// Artist is set for album lookups.
	Artist string `json:"artist,omitempty"`
	// RequireIDMatch prevents unverified title matches from contributing metadata.
	RequireIDMatch bool `json:"-"`
}

// Result is what a provider knows about an item.
type Result struct {
	IDs    map[string]string
	Meta   models.Metadata
	Images []models.Image
}

// Provider is the common part of every provider.
type Provider interface {
	Name() string
}

type MovieProvider interface {
	Provider
	Movie(ctx context.Context, q Query) (*Result, error)
}

type SeriesProvider interface {
	Provider
	Series(ctx context.Context, q Query) (*Result, error)
}

type SeasonProvider interface {
	Provider
	Season(ctx context.Context, ids map[string]string, season int, language string) (*Result, error)
}

type EpisodeProvider interface {
	Provider
	Episode(ctx context.Context, ids map[string]string, season, episode int, language string) (*Result, error)
}

type AlbumProvider interface {
	Provider
	Album(ctx context.Context, q Query) (*Result, error)
}

// ImageProvider supplies artwork for items that other providers identified.
// kind is one of the models.Item* constants.
type ImageProvider interface {
	Provider
	Images(ctx context.Context, kind string, ids map[string]string, season *int) ([]models.Image, error)
}

// MergeIDs adds the ids in src that dst lacks.
func MergeIDs(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = map[string]string{}
	}
	for k, v := range src {
		if v != "" && dst[k] == "" {
			dst[k] = v
		}
	}
	return dst
}

// MergeMetadata fills the empty fields of dst from src. dst always wins, which
// is how local NFO data beats remote providers and earlier providers beat
// later ones.
func MergeMetadata(dst *models.Metadata, src models.Metadata) {
	fill := func(d *string, s string) {
		if strings.TrimSpace(*d) == "" {
			*d = s
		}
	}
	fillInt := func(d *int, s int) {
		if *d == 0 {
			*d = s
		}
	}
	fill(&dst.Title, src.Title)
	fill(&dst.OriginalTitle, src.OriginalTitle)
	fill(&dst.SortTitle, src.SortTitle)
	fill(&dst.Overview, src.Overview)
	fill(&dst.Tagline, src.Tagline)
	fill(&dst.PremiereDate, src.PremiereDate)
	fill(&dst.ContentRating, src.ContentRating)
	fill(&dst.Status, src.Status)
	fill(&dst.Language, src.Language)
	fill(&dst.Country, src.Country)
	fill(&dst.Collection, src.Collection)
	fill(&dst.Artist, src.Artist)
	fill(&dst.AlbumArtist, src.AlbumArtist)
	fill(&dst.Album, src.Album)
	fill(&dst.Label, src.Label)
	fillInt(&dst.Year, src.Year)
	fillInt(&dst.Runtime, src.Runtime)
	fillInt(&dst.TrackNumber, src.TrackNumber)
	fillInt(&dst.DiscNumber, src.DiscNumber)
	fillInt(&dst.TotalTracks, src.TotalTracks)
	fillInt(&dst.TotalSeasons, src.TotalSeasons)
	fillInt(&dst.TotalEpisodes, src.TotalEpisodes)
	if len(dst.Genres) == 0 {
		dst.Genres = src.Genres
	}
	if len(dst.Studios) == 0 {
		dst.Studios = src.Studios
	}
	if len(dst.Tags) == 0 {
		dst.Tags = src.Tags
	}
	if len(dst.Cast) == 0 {
		dst.Cast = src.Cast
	} else {
		mergePersonImages(dst.Cast, src.Cast)
	}
	if len(dst.Crew) == 0 {
		dst.Crew = src.Crew
	} else {
		mergePersonImages(dst.Crew, src.Crew)
	}
	for source, rating := range src.Ratings {
		if dst.Ratings == nil {
			dst.Ratings = map[string]float64{}
		}
		if _, ok := dst.Ratings[source]; !ok {
			dst.Ratings[source] = rating
		}
	}
}

func AllowedPersonImage(image string) bool {
	parsed, err := url.Parse(strings.TrimSpace(image))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || len(image) > 500 {
		return false
	}
	switch parsed.Hostname() {
	case "image.tmdb.org", "artworks.thetvdb.com", "thetvdb.com", "www.thetvdb.com", "i.discogs.com":
		return true
	}
	return false
}

func mergePersonImages(existing, incoming []models.Person) {
	for index := range existing {
		for _, candidate := range incoming {
			if strings.TrimSpace(existing[index].Name) == "" || !strings.EqualFold(strings.TrimSpace(existing[index].Name), strings.TrimSpace(candidate.Name)) {
				continue
			}
			for _, image := range []string{candidate.Image, candidate.FallbackImage} {
				if AllowedPersonImage(image) && existing[index].FallbackImage == "" && image != existing[index].Image {
					existing[index].FallbackImage = strings.TrimSpace(image)
				}
			}
		}
	}
}

// Cache is a small in-memory TTL cache that also collapses concurrent
// lookups for one key, so a thousand episodes of one series ask a provider
// about the series once.
type Cache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	done    chan struct{}
	value   *Result
	err     error
	expires time.Time
}

func NewCache(ttl time.Duration) *Cache {
	return &Cache{ttl: ttl, entries: map[string]*cacheEntry{}}
}

// Do returns the cached result for key or runs fn. Errors are cached briefly
// so a failing provider is not hammered by every sibling task.
func (c *Cache) Do(key string, fn func() (*Result, error)) (*Result, error) {
	c.mu.Lock()
	if entry, ok := c.entries[key]; ok && time.Now().Before(entry.expires) {
		c.mu.Unlock()
		<-entry.done
		return entry.value, entry.err
	}
	entry := &cacheEntry{done: make(chan struct{}), expires: time.Now().Add(c.ttl)}
	c.entries[key] = entry
	if len(c.entries) > 5000 {
		c.evictLocked()
	}
	c.mu.Unlock()

	entry.value, entry.err = fn()
	if entry.err != nil {
		c.mu.Lock()
		entry.expires = time.Now().Add(15 * time.Second)
		c.mu.Unlock()
	}
	close(entry.done)
	return entry.value, entry.err
}

func (c *Cache) evictLocked() {
	now := time.Now()
	for key, entry := range c.entries {
		select {
		case <-entry.done:
			if now.After(entry.expires) {
				delete(c.entries, key)
			}
		default:
		}
	}
}
