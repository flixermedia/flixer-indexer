// Package tvdb implements TheTVDB (API v4): series, movies and episodes.
package tvdb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

const defaultBase = "https://api4.thetvdb.com/v4"

// TVDB artwork type ids.
var (
	seriesArt = map[int]string{1: models.ImageBanner, 2: models.ImagePoster, 3: models.ImageBackdrop, 23: models.ImageLogo}
	movieArt  = map[int]string{14: models.ImagePoster, 15: models.ImageBackdrop, 16: models.ImageBanner, 25: models.ImageLogo}
)

type Provider struct {
	key  string
	base string
	http *httpx.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func New(key, userAgent string) *Provider {
	return &Provider{key: key, base: defaultBase, http: httpx.New("tvdb", userAgent, 50*time.Millisecond)}
}

func (p *Provider) Name() string        { return "tvdb" }
func (p *Provider) Stats() *httpx.Stats { return p.http.Stats }
func (p *Provider) WithBase(base string) *Provider {
	p.base = base
	return p
}

func (p *Provider) bearer(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.token != "" && time.Now().Before(p.expires) {
		return p.token, nil
	}
	var login struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	err := p.http.JSON(ctx, httpx.Request{Method: "POST", URL: p.base + "/login", Body: map[string]string{"apikey": p.key}}, &login)
	if err != nil {
		return "", err
	}
	if login.Data.Token == "" {
		return "", fmt.Errorf("tvdb: login returned no token")
	}
	p.token, p.expires = login.Data.Token, time.Now().Add(24*time.Hour)
	return p.token, nil
}

func (p *Provider) get(ctx context.Context, path string, out any) error {
	token, err := p.bearer(ctx)
	if err != nil {
		return err
	}
	err = p.http.GetJSON(ctx, p.base+path, map[string]string{"Authorization": "Bearer " + token}, out)
	if err != nil && strings.Contains(err.Error(), "status 401") {
		p.mu.Lock()
		p.token = ""
		p.mu.Unlock()
	}
	return err
}

// flexString accepts a JSON string or number.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

type artwork struct {
	Image    string  `json:"image"`
	Type     int     `json:"type"`
	Score    float64 `json:"score"`
	Language string  `json:"language"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
}

type extended struct {
	ID              int                     `json:"id"`
	Name            string                  `json:"name"`
	Overview        string                  `json:"overview"`
	Year            flexString              `json:"year"`
	FirstAired      string                  `json:"firstAired"`
	Status          struct{ Name string }   `json:"status"`
	OriginalLang    string                  `json:"originalLanguage"`
	OriginalCountry string                  `json:"originalCountry"`
	Runtime         int                     `json:"runtime"`
	AverageRuntime  int                     `json:"averageRuntime"`
	Score           float64                 `json:"score"`
	Genres          []struct{ Name string } `json:"genres"`
	Studios         []struct{ Name string } `json:"studios"`
	Companies       []struct{ Name string } `json:"companies"`
	Artworks        []artwork               `json:"artworks"`
	RemoteIDs       []struct {
		ID         string `json:"id"`
		SourceName string `json:"sourceName"`
	} `json:"remoteIds"`
	Characters []struct {
		Name       string `json:"name"`
		PersonName string `json:"personName"`
		Type       int    `json:"type"`
		Sort       int    `json:"sort"`
		PersonImg  string `json:"personImgURL"`
	} `json:"characters"`
	FirstRelease struct{ Date string } `json:"first_release"`
	Seasons      []struct {
		Number int                   `json:"number"`
		Type   struct{ Type string } `json:"type"`
	} `json:"seasons"`
}

func (p *Provider) search(ctx context.Context, kind string, q providers.Query) (string, error) {
	if id := q.IDs["tvdb"]; id != "" {
		return id, nil
	}
	if q.Title == "" {
		return "", httpx.ErrNotFound
	}
	params := url.Values{"query": {q.Title}, "type": {kind}}
	if q.Year > 0 {
		params.Set("year", strconv.Itoa(q.Year))
	}
	var res struct {
		Data []struct {
			TVDBID flexString `json:"tvdb_id"`
		} `json:"data"`
	}
	if err := p.get(ctx, "/search?"+params.Encode(), &res); err != nil {
		return "", err
	}
	if len(res.Data) == 0 || res.Data[0].TVDBID == "" {
		return "", httpx.ErrNotFound
	}
	return string(res.Data[0].TVDBID), nil
}

func (p *Provider) Series(ctx context.Context, q providers.Query) (*providers.Result, error) {
	id, err := p.search(ctx, "series", q)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data extended `json:"data"`
	}
	if err := p.get(ctx, "/series/"+id+"/extended?short=false", &res); err != nil {
		return nil, err
	}
	d := res.Data
	meta := toMetadata(d)
	meta.PremiereDate = d.FirstAired
	if meta.Runtime == 0 {
		meta.Runtime = d.AverageRuntime
	}
	if meta.Year == 0 && len(d.FirstAired) >= 4 {
		meta.Year, _ = strconv.Atoi(d.FirstAired[:4])
	}
	seasons := 0
	for _, s := range d.Seasons {
		if s.Type.Type == "official" && s.Number > seasons {
			seasons = s.Number
		}
	}
	meta.TotalSeasons = seasons
	ids := remoteIDs(d)
	ids["tvdb"] = id
	return &providers.Result{IDs: ids, Meta: meta, Images: images(d.Artworks, seriesArt)}, nil
}

func (p *Provider) Movie(ctx context.Context, q providers.Query) (*providers.Result, error) {
	id, err := p.search(ctx, "movie", q)
	if err != nil {
		return nil, err
	}
	var res struct {
		Data extended `json:"data"`
	}
	if err := p.get(ctx, "/movies/"+id+"/extended?short=false", &res); err != nil {
		return nil, err
	}
	d := res.Data
	meta := toMetadata(d)
	meta.PremiereDate = d.FirstRelease.Date
	if meta.Year == 0 && len(meta.PremiereDate) >= 4 {
		meta.Year, _ = strconv.Atoi(meta.PremiereDate[:4])
	}
	if len(meta.Studios) == 0 {
		for _, s := range d.Studios {
			meta.Studios = append(meta.Studios, s.Name)
		}
	}
	ids := remoteIDs(d)
	ids["tvdb"] = id
	return &providers.Result{IDs: ids, Meta: meta, Images: images(d.Artworks, movieArt)}, nil
}

func (p *Provider) Episode(ctx context.Context, ids map[string]string, season, episode int, _ string) (*providers.Result, error) {
	id := ids["tvdb"]
	if id == "" {
		return nil, httpx.ErrNotFound
	}
	var res struct {
		Data struct {
			Episodes []struct {
				ID       int    `json:"id"`
				Name     string `json:"name"`
				Overview string `json:"overview"`
				Aired    string `json:"aired"`
				Runtime  int    `json:"runtime"`
				Image    string `json:"image"`
			} `json:"episodes"`
		} `json:"data"`
	}
	path := fmt.Sprintf("/series/%s/episodes/default?season=%d&episodeNumber=%d", id, season, episode)
	if err := p.get(ctx, path, &res); err != nil {
		return nil, err
	}
	if len(res.Data.Episodes) == 0 {
		return nil, httpx.ErrNotFound
	}
	e := res.Data.Episodes[0]
	out := &providers.Result{
		IDs:  map[string]string{"tvdbEpisode": strconv.Itoa(e.ID)},
		Meta: models.Metadata{Title: e.Name, Overview: e.Overview, PremiereDate: e.Aired, Runtime: e.Runtime},
	}
	if len(e.Aired) >= 4 {
		out.Meta.Year, _ = strconv.Atoi(e.Aired[:4])
	}
	if e.Image != "" {
		out.Images = append(out.Images, models.Image{Type: models.ImageThumb, URL: e.Image, Provider: "tvdb"})
	}
	return out, nil
}

func toMetadata(d extended) models.Metadata {
	meta := models.Metadata{
		Title: d.Name, Overview: d.Overview, Status: d.Status.Name,
		Language: d.OriginalLang, Country: d.OriginalCountry, Runtime: d.Runtime,
	}
	meta.Year, _ = strconv.Atoi(string(d.Year))
	for _, g := range d.Genres {
		meta.Genres = append(meta.Genres, g.Name)
	}
	for _, c := range d.Companies {
		meta.Studios = append(meta.Studios, c.Name)
	}
	if d.Score > 0 {
		meta.Ratings = map[string]float64{"tvdb": d.Score}
	}
	for _, c := range d.Characters {
		if c.Type != 3 || c.PersonName == "" || len(meta.Cast) >= 30 {
			continue
		}
		meta.Cast = append(meta.Cast, models.Person{Name: c.PersonName, Type: "actor", Role: c.Name, Order: c.Sort, Image: c.PersonImg})
	}
	sort.SliceStable(meta.Cast, func(i, j int) bool { return meta.Cast[i].Order < meta.Cast[j].Order })
	return meta
}

func remoteIDs(d extended) map[string]string {
	ids := map[string]string{}
	for _, r := range d.RemoteIDs {
		switch strings.ToLower(r.SourceName) {
		case "imdb":
			ids["imdb"] = r.ID
		case "themoviedb.com":
			ids["tmdb"] = r.ID
		}
	}
	return ids
}

func images(list []artwork, kinds map[int]string) []models.Image {
	sorted := append([]artwork(nil), list...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Score > sorted[j].Score })
	counts := map[string]int{}
	var out []models.Image
	for _, a := range sorted {
		kind, ok := kinds[a.Type]
		if !ok || a.Image == "" || counts[kind] >= 3 {
			continue
		}
		if a.Language != "" && a.Language != "eng" && kind == models.ImagePoster && counts[kind] > 0 {
			continue
		}
		counts[kind]++
		out = append(out, models.Image{Type: kind, URL: a.Image, Provider: "tvdb", Width: a.Width, Height: a.Height, Language: a.Language, Score: a.Score})
	}
	return out
}
