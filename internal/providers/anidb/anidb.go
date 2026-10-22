// Package anidb implements the AniDB HTTP API. AniDB only answers lookups by
// its own anime id (aid), so this provider enriches a series whose aid is
// already known from an NFO, a folder tag like [anidbid-123], or a previous
// provider. It also requires a registered client name, which is configured
// as the provider's API key.
package anidb

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

const (
	defaultBase = "http://api.anidb.net:9001/httpapi"
	imageBase   = "https://cdn.anidb.net/images/main/"
)

type Provider struct {
	client string
	base   string
	http   *httpx.Client
}

func New(client, userAgent string) *Provider {
	// AniDB bans clients that exceed one request every two seconds.
	return &Provider{client: client, base: defaultBase, http: httpx.New("anidb", userAgent, 4*time.Second)}
}

func (p *Provider) Name() string        { return "anidb" }
func (p *Provider) Stats() *httpx.Stats { return p.http.Stats }
func (p *Provider) WithBase(base string) *Provider {
	p.base = base
	return p
}

type anime struct {
	XMLName   xml.Name `xml:"anime"`
	ID        int      `xml:"id,attr"`
	StartDate string   `xml:"startdate"`
	EpCount   int      `xml:"episodecount"`
	Desc      string   `xml:"description"`
	Picture   string   `xml:"picture"`
	Titles    []struct {
		Type string `xml:"type,attr"`
		Lang string `xml:"lang,attr"`
		Name string `xml:",chardata"`
	} `xml:"titles>title"`
	Rating struct {
		Value string `xml:",chardata"`
	} `xml:"ratings>permanent"`
	Tags []struct {
		Name string `xml:"name"`
	} `xml:"tags>tag"`
}

func (p *Provider) Series(ctx context.Context, q providers.Query) (*providers.Result, error) {
	aid := q.IDs["anidb"]
	if aid == "" {
		return nil, httpx.ErrNotFound
	}
	query := httpx.Query("request", "anime", "client", p.client, "clientver", "1", "protover", "1", "aid", aid)
	body, err := p.http.Do(ctx, httpx.Request{URL: p.base + "?" + query, Headers: map[string]string{"Accept": "application/xml"}})
	if err != nil {
		return nil, err
	}
	// The API gzips responses whether or not the client asked for it.
	if reader, gzErr := gzip.NewReader(bytes.NewReader(body)); gzErr == nil {
		if body, err = io.ReadAll(io.LimitReader(reader, 8<<20)); err != nil {
			return nil, err
		}
	}
	var a anime
	if err := xml.Unmarshal(body, &a); err != nil || a.ID == 0 {
		return nil, httpx.ErrNotFound
	}

	meta := models.Metadata{
		Overview: strings.TrimSpace(a.Desc), TotalEpisodes: a.EpCount, Language: "ja",
		PremiereDate: a.StartDate,
	}
	for _, t := range a.Titles {
		switch {
		case t.Type == "main":
			meta.OriginalTitle = t.Name
			if meta.Title == "" {
				meta.Title = t.Name
			}
		case t.Type == "official" && t.Lang == "en":
			meta.Title = t.Name
		}
	}
	if len(a.StartDate) >= 4 {
		meta.Year, _ = strconv.Atoi(a.StartDate[:4])
	}
	if rating, err := strconv.ParseFloat(strings.TrimSpace(a.Rating.Value), 64); err == nil {
		meta.Ratings = map[string]float64{"anidb": rating}
	}
	for i, tag := range a.Tags {
		if i >= 10 {
			break
		}
		meta.Genres = append(meta.Genres, tag.Name)
	}

	res := &providers.Result{IDs: map[string]string{"anidb": strconv.Itoa(a.ID)}, Meta: meta}
	if a.Picture != "" {
		res.Images = append(res.Images, models.Image{Type: models.ImagePoster, URL: imageBase + a.Picture, Provider: "anidb"})
	}
	return res, nil
}
