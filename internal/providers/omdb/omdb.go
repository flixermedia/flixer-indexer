// Package omdb implements the OMDb provider, which mirrors IMDb data. It is
// mostly useful for ratings and as a fallback for sparse entries.
package omdb

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

const defaultBase = "https://www.omdbapi.com/"

type Provider struct {
	key  string
	base string
	http *httpx.Client
}

func New(key, userAgent string) *Provider {
	return &Provider{key: key, base: defaultBase, http: httpx.New("omdb", userAgent, 50*time.Millisecond)}
}

func (p *Provider) Name() string        { return "omdb" }
func (p *Provider) Stats() *httpx.Stats { return p.http.Stats }
func (p *Provider) WithBase(base string) *Provider {
	p.base = base
	return p
}

type response struct {
	Response   string `json:"Response"`
	Error      string `json:"Error"`
	Title      string `json:"Title"`
	Year       string `json:"Year"`
	Rated      string `json:"Rated"`
	Released   string `json:"Released"`
	Runtime    string `json:"Runtime"`
	Genre      string `json:"Genre"`
	Director   string `json:"Director"`
	Writer     string `json:"Writer"`
	Actors     string `json:"Actors"`
	Plot       string `json:"Plot"`
	Language   string `json:"Language"`
	Country    string `json:"Country"`
	Poster     string `json:"Poster"`
	IMDBRating string `json:"imdbRating"`
	IMDBID     string `json:"imdbID"`
	Production string `json:"Production"`
	Season     string `json:"Season"`
	Episode    string `json:"Episode"`
	TotalSeas  string `json:"totalSeasons"`
}

func (p *Provider) fetch(ctx context.Context, params url.Values) (*response, error) {
	params.Set("apikey", p.key)
	params.Set("plot", "full")
	var r response
	if err := p.http.GetJSON(ctx, p.base+"?"+params.Encode(), nil, &r); err != nil {
		return nil, err
	}
	if r.Response == "False" {
		return nil, httpx.ErrNotFound
	}
	return &r, nil
}

func (p *Provider) lookup(ctx context.Context, kind string, q providers.Query) (*providers.Result, error) {
	params := url.Values{}
	switch {
	case q.IDs["imdb"] != "":
		params.Set("i", q.IDs["imdb"])
	case q.Title != "":
		params.Set("t", q.Title)
		params.Set("type", kind)
		if q.Year > 0 {
			params.Set("y", strconv.Itoa(q.Year))
		}
	default:
		return nil, httpx.ErrNotFound
	}
	r, err := p.fetch(ctx, params)
	if err != nil {
		return nil, err
	}
	return toResult(r), nil
}

func (p *Provider) Movie(ctx context.Context, q providers.Query) (*providers.Result, error) {
	return p.lookup(ctx, "movie", q)
}

func (p *Provider) Series(ctx context.Context, q providers.Query) (*providers.Result, error) {
	return p.lookup(ctx, "series", q)
}

func (p *Provider) Episode(ctx context.Context, ids map[string]string, season, episode int, _ string) (*providers.Result, error) {
	imdb := ids["imdb"]
	if imdb == "" {
		return nil, httpx.ErrNotFound
	}
	r, err := p.fetch(ctx, url.Values{"i": {imdb}, "Season": {strconv.Itoa(season)}, "Episode": {strconv.Itoa(episode)}})
	if err != nil {
		return nil, err
	}
	res := toResult(r)
	res.IDs = map[string]string{"imdbEpisode": r.IMDBID}
	return res, nil
}

func toResult(r *response) *providers.Result {
	meta := models.Metadata{
		Title: clean(r.Title), Overview: clean(r.Plot), ContentRating: clean(r.Rated),
		Genres: split(r.Genre), Studios: split(r.Production), Country: firstOf(r.Country),
		Language: firstOf(r.Language), PremiereDate: premiere(r.Released),
	}
	if len(r.Year) >= 4 {
		meta.Year, _ = strconv.Atoi(r.Year[:4])
	}
	if fields := strings.Fields(r.Runtime); len(fields) > 0 {
		meta.Runtime, _ = strconv.Atoi(fields[0])
	}
	if rating, err := strconv.ParseFloat(r.IMDBRating, 64); err == nil {
		meta.Ratings = map[string]float64{"imdb": rating}
	}
	for i, name := range split(r.Actors) {
		meta.Cast = append(meta.Cast, models.Person{Name: name, Type: "actor", Order: i})
	}
	for _, name := range split(r.Director) {
		meta.Crew = append(meta.Crew, models.Person{Name: name, Type: "director"})
	}
	for _, name := range split(stripParens(r.Writer)) {
		meta.Crew = append(meta.Crew, models.Person{Name: name, Type: "writer"})
	}
	res := &providers.Result{IDs: map[string]string{}, Meta: meta}
	if r.IMDBID != "" {
		res.IDs["imdb"] = r.IMDBID
	}
	if poster := clean(r.Poster); poster != "" {
		res.Images = append(res.Images, models.Image{Type: models.ImagePoster, URL: poster, Provider: "omdb"})
	}
	return res
}

// clean maps OMDb's "N/A" placeholder to empty.
func clean(s string) string {
	if s == "N/A" {
		return ""
	}
	return strings.TrimSpace(s)
}

func split(s string) []string {
	var out []string
	for _, part := range strings.Split(clean(s), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func firstOf(s string) string {
	if list := split(s); len(list) > 0 {
		return list[0]
	}
	return ""
}

func stripParens(s string) string {
	for {
		open := strings.Index(s, "(")
		close := strings.Index(s, ")")
		if open < 0 || close < open {
			return s
		}
		s = s[:open] + s[close+1:]
	}
}

// premiere converts OMDb's "15 Jul 2010" into an ISO date.
func premiere(s string) string {
	t, err := time.Parse("02 Jan 2006", clean(s))
	if err != nil {
		return ""
	}
	return t.Format("2006-01-02")
}
