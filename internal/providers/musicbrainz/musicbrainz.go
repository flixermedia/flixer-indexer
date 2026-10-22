// Package musicbrainz implements MusicBrainz album metadata and the Cover Art
// Archive artwork source that belongs to it.
package musicbrainz

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

const (
	defaultBase  = "https://musicbrainz.org/ws/2"
	defaultCover = "https://coverartarchive.org"
	minScore     = 85
)

type Provider struct {
	base string
	http *httpx.Client
}

func New(userAgent string) *Provider {
	// MusicBrainz allows one request per second per client.
	return &Provider{base: defaultBase, http: httpx.New("musicbrainz", userAgent, 1100*time.Millisecond)}
}

func (p *Provider) Name() string        { return "musicbrainz" }
func (p *Provider) Stats() *httpx.Stats { return p.http.Stats }
func (p *Provider) WithBase(base string) *Provider {
	p.base = base
	return p
}

type release struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Date    string `json:"date"`
	Country string `json:"country"`
	Score   int    `json:"score"`
	Credit  []struct {
		Name string `json:"name"`
	} `json:"artist-credit"`
	LabelInfo []struct {
		Label struct{ Name string } `json:"label"`
	} `json:"label-info"`
	Genres []struct{ Name string } `json:"genres"`
	Media  []struct {
		TrackCount int `json:"track-count"`
	} `json:"media"`
	ReleaseGroup struct {
		ID string `json:"id"`
	} `json:"release-group"`
}

func (p *Provider) Album(ctx context.Context, q providers.Query) (*providers.Result, error) {
	id := q.IDs["musicbrainz"]
	if id == "" {
		if q.Title == "" {
			return nil, httpx.ErrNotFound
		}
		search := `release:"` + escape(q.Title) + `"`
		if q.Artist != "" {
			search += ` AND artist:"` + escape(q.Artist) + `"`
		}
		var found struct {
			Releases []release `json:"releases"`
		}
		if err := p.http.GetJSON(ctx, p.base+"/release?"+httpx.Query("query", search, "limit", "1", "fmt", "json"), nil, &found); err != nil {
			return nil, err
		}
		if len(found.Releases) == 0 || found.Releases[0].Score < minScore {
			return nil, httpx.ErrNotFound
		}
		id = found.Releases[0].ID
	}

	var r release
	err := p.http.GetJSON(ctx, p.base+"/release/"+id+"?inc=artist-credits+labels+genres+release-groups&fmt=json", nil, &r)
	if err != nil {
		return nil, err
	}

	meta := models.Metadata{Album: r.Title, Title: r.Title, PremiereDate: r.Date, Country: r.Country}
	if len(r.Date) >= 4 {
		meta.Year, _ = strconv.Atoi(r.Date[:4])
	}
	if len(r.Credit) > 0 {
		meta.AlbumArtist = r.Credit[0].Name
		meta.Artist = r.Credit[0].Name
	}
	if len(r.LabelInfo) > 0 {
		meta.Label = r.LabelInfo[0].Label.Name
	}
	for _, g := range r.Genres {
		meta.Genres = append(meta.Genres, g.Name)
	}
	for _, m := range r.Media {
		meta.TotalTracks += m.TrackCount
	}
	ids := map[string]string{"musicbrainz": r.ID}
	if r.ReleaseGroup.ID != "" {
		ids["musicbrainzReleaseGroup"] = r.ReleaseGroup.ID
	}
	return &providers.Result{IDs: ids, Meta: meta}, nil
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// CoverArt serves front covers from the Cover Art Archive.
type CoverArt struct {
	base string
	http *httpx.Client
}

func NewCoverArt(userAgent string) *CoverArt {
	return &CoverArt{base: defaultCover, http: httpx.New("coverartarchive", userAgent, 200*time.Millisecond)}
}

func (c *CoverArt) Name() string        { return "coverartarchive" }
func (c *CoverArt) Stats() *httpx.Stats { return c.http.Stats }
func (c *CoverArt) WithBase(base string) *CoverArt {
	c.base = base
	return c
}

func (c *CoverArt) Images(ctx context.Context, kind string, ids map[string]string, _ *int) ([]models.Image, error) {
	if kind != models.ItemAlbum {
		return nil, nil
	}
	var paths []string
	if ids["musicbrainz"] != "" {
		paths = append(paths, "/release/"+ids["musicbrainz"])
	}
	if ids["musicbrainzReleaseGroup"] != "" {
		paths = append(paths, "/release-group/"+ids["musicbrainzReleaseGroup"])
	}
	for _, path := range paths {
		var body struct {
			Images []struct {
				Front bool   `json:"front"`
				Image string `json:"image"`
			} `json:"images"`
		}
		err := c.http.GetJSON(ctx, c.base+path, nil, &body)
		if errors.Is(err, httpx.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, img := range body.Images {
			if img.Front && img.Image != "" {
				return []models.Image{{Type: models.ImagePoster, URL: img.Image, Provider: "coverartarchive"}}, nil
			}
		}
	}
	return nil, nil
}
