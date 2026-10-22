// Package discogs implements the Discogs provider, the lowest-priority source
// for album metadata.
package discogs

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

const defaultBase = "https://api.discogs.com"

type Provider struct {
	token string
	base  string
	http  *httpx.Client
}

func New(token, userAgent string) *Provider {
	// Authenticated clients may make 60 requests a minute.
	return &Provider{token: token, base: defaultBase, http: httpx.New("discogs", userAgent, time.Second)}
}

func (p *Provider) Name() string        { return "discogs" }
func (p *Provider) Stats() *httpx.Stats { return p.http.Stats }
func (p *Provider) WithBase(base string) *Provider {
	p.base = base
	return p
}

func (p *Provider) headers() map[string]string {
	return map[string]string{"Authorization": "Discogs token=" + p.token}
}

func (p *Provider) Album(ctx context.Context, q providers.Query) (*providers.Result, error) {
	id := q.IDs["discogs"]
	if id == "" {
		if q.Title == "" {
			return nil, httpx.ErrNotFound
		}
		var found struct {
			Results []struct {
				ID int `json:"id"`
			} `json:"results"`
		}
		query := httpx.Query("type", "release", "release_title", q.Title, "artist", q.Artist, "per_page", "1")
		if err := p.http.GetJSON(ctx, p.base+"/database/search?"+query, p.headers(), &found); err != nil {
			return nil, err
		}
		if len(found.Results) == 0 {
			return nil, httpx.ErrNotFound
		}
		id = strconv.Itoa(found.Results[0].ID)
	}

	var r struct {
		ID      int      `json:"id"`
		Title   string   `json:"title"`
		Year    int      `json:"year"`
		Genres  []string `json:"genres"`
		Styles  []string `json:"styles"`
		Country string   `json:"country"`
		Notes   string   `json:"notes"`
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Tracklist []struct {
			Type string `json:"type_"`
		} `json:"tracklist"`
		Images []struct {
			Type   string `json:"type"`
			URI    string `json:"uri"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		} `json:"images"`
	}
	if err := p.http.GetJSON(ctx, p.base+"/releases/"+id, p.headers(), &r); err != nil {
		return nil, err
	}

	meta := models.Metadata{
		Title: r.Title, Album: r.Title, Year: r.Year, Country: r.Country,
		Overview: strings.TrimSpace(r.Notes), Genres: append(r.Genres, r.Styles...),
	}
	if len(r.Artists) > 0 {
		meta.Artist = trimDisambiguation(r.Artists[0].Name)
		meta.AlbumArtist = meta.Artist
	}
	if len(r.Labels) > 0 {
		meta.Label = r.Labels[0].Name
	}
	for _, t := range r.Tracklist {
		if t.Type == "track" {
			meta.TotalTracks++
		}
	}
	res := &providers.Result{IDs: map[string]string{"discogs": strconv.Itoa(r.ID)}, Meta: meta}
	for _, img := range r.Images {
		if img.Type == "primary" && img.URI != "" {
			res.Images = append(res.Images, models.Image{Type: models.ImagePoster, URL: img.URI, Provider: "discogs", Width: img.Width, Height: img.Height})
			break
		}
	}
	return res, nil
}

// trimDisambiguation drops Discogs' "Name (2)" suffix for homonymous artists.
func trimDisambiguation(name string) string {
	if i := strings.LastIndex(name, " ("); i > 0 && strings.HasSuffix(name, ")") {
		if _, err := strconv.Atoi(name[i+2 : len(name)-1]); err == nil {
			return name[:i]
		}
	}
	return name
}
