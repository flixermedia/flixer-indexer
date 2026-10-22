// Package fanart implements Fanart.tv, an artwork-only provider.
package fanart

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
)

const defaultBase = "https://webservice.fanart.tv/v3"

type Provider struct {
	key  string
	base string
	http *httpx.Client
}

func New(key, userAgent string) *Provider {
	return &Provider{key: key, base: defaultBase, http: httpx.New("fanart", userAgent, 100*time.Millisecond)}
}

func (p *Provider) Name() string        { return "fanart" }
func (p *Provider) Stats() *httpx.Stats { return p.http.Stats }
func (p *Provider) WithBase(base string) *Provider {
	p.base = base
	return p
}

type art struct {
	URL    string `json:"url"`
	Lang   string `json:"lang"`
	Likes  string `json:"likes"`
	Season string `json:"season"`
}

// Which Fanart keys feed which image type.
var (
	movieKeys = map[string]string{
		"movieposter": models.ImagePoster, "moviebackground": models.ImageBackdrop,
		"hdmovielogo": models.ImageLogo, "movielogo": models.ImageLogo,
		"moviebanner": models.ImageBanner, "moviethumb": models.ImageThumb, "moviedisc": models.ImageDisc,
	}
	tvKeys = map[string]string{
		"tvposter": models.ImagePoster, "showbackground": models.ImageBackdrop,
		"hdtvlogo": models.ImageLogo, "clearlogo": models.ImageLogo,
		"tvbanner": models.ImageBanner, "tvthumb": models.ImageThumb, "seasonposter": models.ImagePoster,
	}
)

// Images fetches artwork for a movie (tmdb/imdb id), series (tvdb id), or
// album (MusicBrainz release-group id).
func (p *Provider) Images(ctx context.Context, kind string, ids map[string]string, season *int) ([]models.Image, error) {
	switch kind {
	case models.ItemMovie:
		id := ids["tmdb"]
		if id == "" {
			id = ids["imdb"]
		}
		return p.fetch(ctx, "/movies/"+id, id, movieKeys, season)
	case models.ItemSeries:
		return p.fetch(ctx, "/tv/"+ids["tvdb"], ids["tvdb"], tvKeys, nil)
	case models.ItemSeason:
		seasonKeys := map[string]string{"seasonposter": models.ImagePoster}
		return p.fetch(ctx, "/tv/"+ids["tvdb"], ids["tvdb"], seasonKeys, season)
	case models.ItemAlbum:
		return p.album(ctx, ids["musicbrainzReleaseGroup"])
	}
	return nil, nil
}

func (p *Provider) fetch(ctx context.Context, path, id string, keys map[string]string, season *int) ([]models.Image, error) {
	if id == "" {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	err := p.http.GetJSON(ctx, p.base+path+"?"+httpx.Query("api_key", p.key), nil, &raw)
	if errors.Is(err, httpx.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []models.Image
	for key, kind := range keys {
		var list []art
		if json.Unmarshal(raw[key], &list) != nil {
			continue
		}
		sort.SliceStable(list, func(i, j int) bool { return likes(list[i]) > likes(list[j]) })
		taken := 0
		for _, a := range list {
			if a.URL == "" || taken >= 3 {
				continue
			}
			img := models.Image{Type: kind, URL: a.URL, Provider: "fanart", Language: a.Lang, Score: likes(a)}
			if key == "seasonposter" {
				n, err := strconv.Atoi(a.Season)
				if err != nil || season == nil || n != *season {
					continue
				}
				img.Season = &n
			}
			taken++
			out = append(out, img)
		}
	}
	return out, nil
}

func (p *Provider) album(ctx context.Context, releaseGroup string) ([]models.Image, error) {
	if releaseGroup == "" {
		return nil, nil
	}
	var body struct {
		Albums map[string]struct {
			Cover []art `json:"albumcover"`
			CDArt []art `json:"cdart"`
		} `json:"albums"`
	}
	err := p.http.GetJSON(ctx, p.base+"/music/albums/"+releaseGroup+"?"+httpx.Query("api_key", p.key), nil, &body)
	if errors.Is(err, httpx.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []models.Image
	for _, album := range body.Albums {
		for _, a := range album.Cover {
			out = append(out, models.Image{Type: models.ImagePoster, URL: a.URL, Provider: "fanart", Score: likes(a)})
		}
		for _, a := range album.CDArt {
			out = append(out, models.Image{Type: models.ImageDisc, URL: a.URL, Provider: "fanart", Score: likes(a)})
		}
	}
	return out, nil
}

func likes(a art) float64 {
	n, _ := strconv.ParseFloat(a.Likes, 64)
	return n
}
