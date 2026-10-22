// Package tmdb implements The Movie Database provider: movies, series,
// seasons, episodes, people, and artwork.
package tmdb

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

const (
	defaultBase  = "https://api.themoviedb.org/3"
	imageBase    = "https://image.tmdb.org/t/p/original"
	profileBase  = "https://image.tmdb.org/t/p/w185"
	maxCast      = 30
	maxImagesPer = 3
)

type Provider struct {
	key    string
	base   string
	http   *httpx.Client
	images string
}

func New(key, userAgent string) *Provider {
	return &Provider{
		key: key, base: defaultBase, images: imageBase,
		http: httpx.New("tmdb", userAgent, 30*time.Millisecond),
	}
}

func (p *Provider) Name() string                   { return "tmdb" }
func (p *Provider) Stats() *httpx.Stats            { return p.http.Stats }
func (p *Provider) WithBase(base string) *Provider { p.base = base; return p }

func (p *Provider) get(ctx context.Context, path string, params []string, out any) error {
	values := url.Values{}
	headers := map[string]string{}
	// v4 read-access tokens are JWTs and go in a header; v3 keys go in the query.
	if strings.HasPrefix(p.key, "eyJ") {
		headers["Authorization"] = "Bearer " + p.key
	} else {
		values.Set("api_key", p.key)
	}
	for i := 0; i+1 < len(params); i += 2 {
		if params[i+1] != "" {
			values.Set(params[i], params[i+1])
		}
	}
	return p.http.GetJSON(ctx, p.base+path+"?"+values.Encode(), headers, out)
}

type genre struct {
	Name string `json:"name"`
}

type company struct {
	Name string `json:"name"`
}

type castMember struct {
	Name        string `json:"name"`
	Character   string `json:"character"`
	Order       int    `json:"order"`
	ProfilePath string `json:"profile_path"`
	// Roles is how aggregate (series) credits report characters.
	Roles []struct {
		Character string `json:"character"`
	} `json:"roles"`
}

// crewMember handles both plain credits (job) and aggregate credits (jobs).
type crewMember struct {
	Name string `json:"name"`
	Job  string `json:"job"`
	Jobs []struct {
		Job string `json:"job"`
	} `json:"jobs"`
	ProfilePath string `json:"profile_path"`
}

type credits struct {
	Cast       []castMember `json:"cast"`
	Crew       []crewMember `json:"crew"`
	GuestStars []castMember `json:"guest_stars"`
}

type image struct {
	FilePath string  `json:"file_path"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Lang     string  `json:"iso_639_1"`
	Vote     float64 `json:"vote_average"`
}

type imageSet struct {
	Posters   []image `json:"posters"`
	Backdrops []image `json:"backdrops"`
	Logos     []image `json:"logos"`
	Stills    []image `json:"stills"`
}

type externalIDs struct {
	IMDB string `json:"imdb_id"`
	TVDB int    `json:"tvdb_id"`
}

type movieDetails struct {
	ID               int       `json:"id"`
	IMDBID           string    `json:"imdb_id"`
	Title            string    `json:"title"`
	OriginalTitle    string    `json:"original_title"`
	OriginalLanguage string    `json:"original_language"`
	Overview         string    `json:"overview"`
	Tagline          string    `json:"tagline"`
	ReleaseDate      string    `json:"release_date"`
	Runtime          int       `json:"runtime"`
	Status           string    `json:"status"`
	VoteAverage      float64   `json:"vote_average"`
	Genres           []genre   `json:"genres"`
	Companies        []company `json:"production_companies"`
	Countries        []struct {
		ISO string `json:"iso_3166_1"`
	} `json:"production_countries"`
	Collection *struct {
		Name string `json:"name"`
	} `json:"belongs_to_collection"`
	Credits  credits  `json:"credits"`
	Images   imageSet `json:"images"`
	Keywords struct {
		Keywords []genre `json:"keywords"`
	} `json:"keywords"`
	ReleaseDates struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Dates   []struct {
				Certification string `json:"certification"`
			} `json:"release_dates"`
		} `json:"results"`
	} `json:"release_dates"`
}

type seriesDetails struct {
	ID               int         `json:"id"`
	Name             string      `json:"name"`
	OriginalName     string      `json:"original_name"`
	OriginalLanguage string      `json:"original_language"`
	Overview         string      `json:"overview"`
	Tagline          string      `json:"tagline"`
	FirstAirDate     string      `json:"first_air_date"`
	Status           string      `json:"status"`
	VoteAverage      float64     `json:"vote_average"`
	Seasons          int         `json:"number_of_seasons"`
	Episodes         int         `json:"number_of_episodes"`
	RunTimes         []int       `json:"episode_run_time"`
	Genres           []genre     `json:"genres"`
	Networks         []company   `json:"networks"`
	Companies        []company   `json:"production_companies"`
	OriginCountry    []string    `json:"origin_country"`
	Credits          credits     `json:"aggregate_credits"`
	Images           imageSet    `json:"images"`
	ExternalIDs      externalIDs `json:"external_ids"`
	ContentRatings   struct {
		Results []struct {
			Country string `json:"iso_3166_1"`
			Rating  string `json:"rating"`
		} `json:"results"`
	} `json:"content_ratings"`
}

type searchResults struct {
	Results []struct {
		ID int `json:"id"`
	} `json:"results"`
	MovieResults []struct {
		ID int `json:"id"`
	} `json:"movie_results"`
	TVResults []struct {
		ID int `json:"id"`
	} `json:"tv_results"`
}

func (p *Provider) resolveID(ctx context.Context, kind string, q providers.Query) (string, error) {
	if id := q.IDs["tmdb"]; id != "" {
		return id, nil
	}
	if imdb := q.IDs["imdb"]; imdb != "" {
		var found searchResults
		if err := p.get(ctx, "/find/"+url.PathEscape(imdb), []string{"external_source", "imdb_id"}, &found); err == nil {
			list := found.MovieResults
			if kind == "tv" {
				list = found.TVResults
			}
			if len(list) > 0 {
				return strconv.Itoa(list[0].ID), nil
			}
		}
	}
	if q.Title == "" {
		return "", httpx.ErrNotFound
	}
	yearKey := "year"
	if kind == "tv" {
		yearKey = "first_air_date_year"
	}
	year := ""
	if q.Year > 0 {
		year = strconv.Itoa(q.Year)
	}
	var found searchResults
	if err := p.get(ctx, "/search/"+kind, []string{"query", q.Title, yearKey, year, "language", q.Language}, &found); err != nil {
		return "", err
	}
	if len(found.Results) == 0 && year != "" {
		// A wrong year in the folder name should not hide the title.
		if err := p.get(ctx, "/search/"+kind, []string{"query", q.Title, "language", q.Language}, &found); err != nil {
			return "", err
		}
	}
	if len(found.Results) == 0 {
		return "", httpx.ErrNotFound
	}
	return strconv.Itoa(found.Results[0].ID), nil
}

func (p *Provider) Movie(ctx context.Context, q providers.Query) (*providers.Result, error) {
	id, err := p.resolveID(ctx, "movie", q)
	if err != nil {
		return nil, err
	}
	var d movieDetails
	if err := p.get(ctx, "/movie/"+id, []string{
		"language", q.Language,
		"append_to_response", "credits,images,keywords,release_dates",
		"include_image_language", imageLanguages(q.Language),
	}, &d); err != nil {
		return nil, err
	}

	meta := models.Metadata{
		Title: d.Title, OriginalTitle: d.OriginalTitle, Overview: d.Overview, Tagline: d.Tagline,
		PremiereDate: d.ReleaseDate, Year: yearOf(d.ReleaseDate), Runtime: d.Runtime,
		Genres: names(d.Genres), Studios: companyNames(d.Companies), Status: d.Status,
		Language: d.OriginalLanguage,
	}
	if len(d.Countries) > 0 {
		meta.Country = d.Countries[0].ISO
	}
	if d.Collection != nil {
		meta.Collection = d.Collection.Name
	}
	for _, k := range d.Keywords.Keywords {
		meta.Tags = append(meta.Tags, k.Name)
	}
	if d.VoteAverage > 0 {
		meta.Ratings = map[string]float64{"tmdb": d.VoteAverage}
	}
	meta.ContentRating = movieCertification(d)
	meta.Cast, meta.Crew = people(d.Credits.Cast, d.Credits.Crew)

	ids := map[string]string{"tmdb": strconv.Itoa(d.ID)}
	if d.IMDBID != "" {
		ids["imdb"] = d.IMDBID
	}
	return &providers.Result{IDs: ids, Meta: meta, Images: p.pickImages(d.Images)}, nil
}

func (p *Provider) Series(ctx context.Context, q providers.Query) (*providers.Result, error) {
	id, err := p.resolveID(ctx, "tv", q)
	if err != nil {
		return nil, err
	}
	var d seriesDetails
	if err := p.get(ctx, "/tv/"+id, []string{
		"language", q.Language,
		"append_to_response", "aggregate_credits,images,external_ids,content_ratings",
		"include_image_language", imageLanguages(q.Language),
	}, &d); err != nil {
		return nil, err
	}

	meta := models.Metadata{
		Title: d.Name, OriginalTitle: d.OriginalName, Overview: d.Overview, Tagline: d.Tagline,
		PremiereDate: d.FirstAirDate, Year: yearOf(d.FirstAirDate), Status: d.Status,
		Genres: names(d.Genres), Studios: companyNames(d.Networks), Language: d.OriginalLanguage,
		TotalSeasons: d.Seasons, TotalEpisodes: d.Episodes,
	}
	if len(meta.Studios) == 0 {
		meta.Studios = companyNames(d.Companies)
	}
	if len(d.RunTimes) > 0 {
		meta.Runtime = d.RunTimes[0]
	}
	if len(d.OriginCountry) > 0 {
		meta.Country = d.OriginCountry[0]
	}
	if d.VoteAverage > 0 {
		meta.Ratings = map[string]float64{"tmdb": d.VoteAverage}
	}
	for _, r := range d.ContentRatings.Results {
		if r.Country == "US" {
			meta.ContentRating = r.Rating
			break
		}
	}
	for i, c := range d.Credits.Cast {
		if i >= maxCast {
			break
		}
		role := c.Character
		if len(c.Roles) > 0 {
			role = c.Roles[0].Character
		}
		meta.Cast = append(meta.Cast, models.Person{Name: c.Name, Type: "actor", Role: role, Order: i, Image: profile(c.ProfilePath)})
	}
	_, meta.Crew = people(nil, d.Credits.Crew)

	ids := map[string]string{"tmdb": strconv.Itoa(d.ID)}
	if d.ExternalIDs.IMDB != "" {
		ids["imdb"] = d.ExternalIDs.IMDB
	}
	if d.ExternalIDs.TVDB > 0 {
		ids["tvdb"] = strconv.Itoa(d.ExternalIDs.TVDB)
	}
	return &providers.Result{IDs: ids, Meta: meta, Images: p.pickImages(d.Images)}, nil
}

func (p *Provider) Season(ctx context.Context, ids map[string]string, season int, language string) (*providers.Result, error) {
	id := ids["tmdb"]
	if id == "" {
		return nil, httpx.ErrNotFound
	}
	var d struct {
		ID           int    `json:"id"`
		Name         string `json:"name"`
		Overview     string `json:"overview"`
		AirDate      string `json:"air_date"`
		PosterPath   string `json:"poster_path"`
		SeasonNumber int    `json:"season_number"`
		Episodes     []struct {
			ID int `json:"id"`
		} `json:"episodes"`
	}
	if err := p.get(ctx, "/tv/"+id+"/season/"+strconv.Itoa(season), []string{"language", language}, &d); err != nil {
		return nil, err
	}
	res := &providers.Result{
		IDs: map[string]string{},
		Meta: models.Metadata{
			Title: d.Name, Overview: d.Overview, PremiereDate: d.AirDate,
			Year: yearOf(d.AirDate), TotalEpisodes: len(d.Episodes),
		},
	}
	if d.PosterPath != "" {
		n := season
		res.Images = append(res.Images, models.Image{Type: models.ImagePoster, URL: p.images + d.PosterPath, Provider: "tmdb", Season: &n})
	}
	return res, nil
}

func (p *Provider) Episode(ctx context.Context, ids map[string]string, season, episode int, language string) (*providers.Result, error) {
	id := ids["tmdb"]
	if id == "" {
		return nil, httpx.ErrNotFound
	}
	var d struct {
		ID          int          `json:"id"`
		Name        string       `json:"name"`
		Overview    string       `json:"overview"`
		AirDate     string       `json:"air_date"`
		Runtime     int          `json:"runtime"`
		VoteAverage float64      `json:"vote_average"`
		StillPath   string       `json:"still_path"`
		Crew        []crewMember `json:"crew"`
		GuestStars  []castMember `json:"guest_stars"`
	}
	path := fmt.Sprintf("/tv/%s/season/%d/episode/%d", id, season, episode)
	if err := p.get(ctx, path, []string{"language", language}, &d); err != nil {
		return nil, err
	}
	meta := models.Metadata{
		Title: d.Name, Overview: d.Overview, PremiereDate: d.AirDate,
		Year: yearOf(d.AirDate), Runtime: d.Runtime,
	}
	if d.VoteAverage > 0 {
		meta.Ratings = map[string]float64{"tmdb": d.VoteAverage}
	}
	meta.Cast, meta.Crew = people(d.GuestStars, d.Crew)
	res := &providers.Result{IDs: map[string]string{"tmdbEpisode": strconv.Itoa(d.ID)}, Meta: meta}
	if d.StillPath != "" {
		res.Images = append(res.Images, models.Image{Type: models.ImageThumb, URL: p.images + d.StillPath, Provider: "tmdb"})
	}
	return res, nil
}

// Images lets TMDB act as an artwork source for items another provider identified.
func (p *Provider) Images(ctx context.Context, kind string, ids map[string]string, _ *int) ([]models.Image, error) {
	id := ids["tmdb"]
	if id == "" {
		return nil, nil
	}
	path := "/movie/" + id + "/images"
	if kind == models.ItemSeries {
		path = "/tv/" + id + "/images"
	}
	var set imageSet
	if err := p.get(ctx, path, []string{"include_image_language", imageLanguages("")}, &set); err != nil {
		return nil, err
	}
	return p.pickImages(set), nil
}

func (p *Provider) pickImages(set imageSet) []models.Image {
	var out []models.Image
	add := func(kind string, list []image, limit int) {
		sorted := append([]image(nil), list...)
		sort.SliceStable(sorted, func(i, j int) bool { return rank(sorted[i]) > rank(sorted[j]) })
		for i, img := range sorted {
			if i >= limit {
				break
			}
			out = append(out, models.Image{
				Type: kind, URL: p.images + img.FilePath, Provider: "tmdb",
				Width: img.Width, Height: img.Height, Language: img.Lang, Score: img.Vote,
			})
		}
	}
	add(models.ImagePoster, set.Posters, maxImagesPer)
	add(models.ImageBackdrop, set.Backdrops, maxImagesPer)
	add(models.ImageLogo, set.Logos, 1)
	return out
}

// rank prefers English or language-neutral artwork, then the community vote.
func rank(img image) float64 {
	score := img.Vote
	if img.Lang == "en" || img.Lang == "" {
		score += 100
	}
	return score
}

func imageLanguages(language string) string {
	lang := strings.SplitN(language, "-", 2)[0]
	if lang == "" || lang == "en" {
		return "en,null"
	}
	return lang + ",en,null"
}

func yearOf(date string) int {
	if len(date) < 4 {
		return 0
	}
	year, _ := strconv.Atoi(date[:4])
	return year
}

func names(list []genre) []string {
	out := make([]string, 0, len(list))
	for _, g := range list {
		out = append(out, g.Name)
	}
	return out
}

func companyNames(list []company) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Name)
	}
	return out
}

func profile(path string) string {
	if path == "" {
		return ""
	}
	return profileBase + path
}

var crewJobs = map[string]string{
	"Director": "director", "Writer": "writer", "Screenplay": "writer", "Story": "writer",
	"Producer": "producer", "Executive Producer": "producer", "Original Music Composer": "composer",
	"Director of Photography": "cinematographer",
}

func people(cast []castMember, crew []crewMember) ([]models.Person, []models.Person) {
	var actors, crews []models.Person
	for i, c := range cast {
		if i >= maxCast {
			break
		}
		actors = append(actors, models.Person{Name: c.Name, Type: "actor", Role: c.Character, Order: i, Image: profile(c.ProfilePath)})
	}
	seen := map[string]bool{}
	for _, c := range crew {
		jobs := []string{c.Job}
		for _, j := range c.Jobs {
			jobs = append(jobs, j.Job)
		}
		for _, job := range jobs {
			kind, ok := crewJobs[job]
			if !ok || seen[c.Name+"|"+kind] {
				continue
			}
			seen[c.Name+"|"+kind] = true
			crews = append(crews, models.Person{Name: c.Name, Type: kind, Role: job, Image: profile(c.ProfilePath)})
		}
	}
	return actors, crews
}

func movieCertification(d movieDetails) string {
	for _, region := range d.ReleaseDates.Results {
		if region.Country != "US" {
			continue
		}
		for _, date := range region.Dates {
			if date.Certification != "" {
				return date.Certification
			}
		}
	}
	return ""
}
