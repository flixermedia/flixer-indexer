// Package anilist implements the AniList provider for anime series.
package anilist

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

const defaultEndpoint = "https://graphql.anilist.co"

const mediaQuery = `
query ($id: Int, $search: String, $year: Int) {
  Media(id: $id, search: $search, seasonYear: $year, type: ANIME) {
    id idMal
    title { romaji english native }
    description(asHtml: false)
    startDate { year month day }
    status format genres episodes duration averageScore countryOfOrigin
    studios(isMain: true) { nodes { name } }
    coverImage { extraLarge }
    bannerImage
  }
}`

type Provider struct {
	endpoint string
	http     *httpx.Client
}

func New(userAgent string) *Provider {
	// AniList allows 90 requests a minute.
	return &Provider{endpoint: defaultEndpoint, http: httpx.New("anilist", userAgent, 700*time.Millisecond)}
}

func (p *Provider) Name() string        { return "anilist" }
func (p *Provider) Stats() *httpx.Stats { return p.http.Stats }
func (p *Provider) WithEndpoint(endpoint string) *Provider {
	p.endpoint = endpoint
	return p
}

type media struct {
	ID    int `json:"id"`
	IDMal int `json:"idMal"`
	Title struct {
		Romaji, English, Native string
	} `json:"title"`
	Description string                         `json:"description"`
	StartDate   struct{ Year, Month, Day int } `json:"startDate"`
	Status      string                         `json:"status"`
	Format      string                         `json:"format"`
	Genres      []string                       `json:"genres"`
	Episodes    int                            `json:"episodes"`
	Duration    int                            `json:"duration"`
	Score       float64                        `json:"averageScore"`
	Country     string                         `json:"countryOfOrigin"`
	Studios     struct {
		Nodes []struct{ Name string } `json:"nodes"`
	} `json:"studios"`
	CoverImage  struct{ ExtraLarge string } `json:"coverImage"`
	BannerImage string                      `json:"bannerImage"`
}

func (p *Provider) Series(ctx context.Context, q providers.Query) (*providers.Result, error) {
	vars := map[string]any{}
	switch {
	case q.IDs["anilist"] != "":
		id, err := strconv.Atoi(q.IDs["anilist"])
		if err != nil {
			return nil, httpx.ErrNotFound
		}
		vars["id"] = id
	case q.Title != "":
		vars["search"] = q.Title
		// A wrong year must not hide a match, so the year is only a hint tried first.
		if q.Year > 0 {
			vars["year"] = q.Year
		}
	default:
		return nil, httpx.ErrNotFound
	}

	m, err := p.query(ctx, vars)
	if err != nil && vars["year"] != nil {
		delete(vars, "year")
		m, err = p.query(ctx, vars)
	}
	if err != nil {
		return nil, err
	}

	meta := models.Metadata{
		Title: first(m.Title.English, m.Title.Romaji), OriginalTitle: first(m.Title.Native, m.Title.Romaji),
		Overview: cleanDescription(m.Description), Genres: m.Genres, Runtime: m.Duration,
		Status: m.Status, Country: m.Country, TotalEpisodes: m.Episodes, Language: "ja",
		Year: m.StartDate.Year,
	}
	if m.StartDate.Year > 0 && m.StartDate.Month > 0 && m.StartDate.Day > 0 {
		meta.PremiereDate = fmt.Sprintf("%04d-%02d-%02d", m.StartDate.Year, m.StartDate.Month, m.StartDate.Day)
	}
	if m.Score > 0 {
		meta.Ratings = map[string]float64{"anilist": m.Score / 10}
	}
	for _, s := range m.Studios.Nodes {
		meta.Studios = append(meta.Studios, s.Name)
	}

	res := &providers.Result{IDs: map[string]string{"anilist": strconv.Itoa(m.ID)}, Meta: meta}
	if m.IDMal > 0 {
		res.IDs["mal"] = strconv.Itoa(m.IDMal)
	}
	if m.CoverImage.ExtraLarge != "" {
		res.Images = append(res.Images, models.Image{Type: models.ImagePoster, URL: m.CoverImage.ExtraLarge, Provider: "anilist"})
	}
	if m.BannerImage != "" {
		res.Images = append(res.Images, models.Image{Type: models.ImageBanner, URL: m.BannerImage, Provider: "anilist"})
	}
	return res, nil
}

func (p *Provider) query(ctx context.Context, vars map[string]any) (*media, error) {
	var body struct {
		Data struct {
			Media *media `json:"Media"`
		} `json:"data"`
	}
	err := p.http.JSON(ctx, httpx.Request{
		Method: "POST", URL: p.endpoint,
		Body: map[string]any{"query": mediaQuery, "variables": vars},
	}, &body)
	if err != nil {
		return nil, err
	}
	if body.Data.Media == nil {
		return nil, httpx.ErrNotFound
	}
	return body.Data.Media, nil
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

func cleanDescription(s string) string {
	s = strings.ReplaceAll(s, "<br>", "\n")
	s = strings.ReplaceAll(s, "<br/>", "\n")
	return strings.TrimSpace(html.UnescapeString(htmlTag.ReplaceAllString(s, "")))
}

func first(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
