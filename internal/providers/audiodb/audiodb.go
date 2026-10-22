// Package audiodb implements TheAudioDB for album metadata and artwork.
package audiodb

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

const defaultBase = "https://www.theaudiodb.com/api/v1/json"

type Provider struct {
	key  string
	base string
	http *httpx.Client
}

func New(key, userAgent string) *Provider {
	return &Provider{key: key, base: defaultBase, http: httpx.New("audiodb", userAgent, 500*time.Millisecond)}
}

func (p *Provider) Name() string        { return "audiodb" }
func (p *Provider) Stats() *httpx.Stats { return p.http.Stats }
func (p *Provider) WithBase(base string) *Provider {
	p.base = base
	return p
}

type album struct {
	ID          string `json:"idAlbum"`
	Name        string `json:"strAlbum"`
	Artist      string `json:"strArtist"`
	Year        string `json:"intYearReleased"`
	Genre       string `json:"strGenre"`
	Label       string `json:"strLabel"`
	Description string `json:"strDescriptionEN"`
	Thumb       string `json:"strAlbumThumb"`
	Back        string `json:"strAlbumThumbBack"`
	CDArt       string `json:"strAlbumCDart"`
	MBID        string `json:"strMusicBrainzID"`
	MBGroupID   string `json:"strMusicBrainzAlbumID"`
	Score       string `json:"intScore"`
}

func (p *Provider) Album(ctx context.Context, q providers.Query) (*providers.Result, error) {
	if q.Title == "" || q.Artist == "" {
		return nil, httpx.ErrNotFound
	}
	var body struct {
		Album []album `json:"album"`
	}
	endpoint := p.base + "/" + p.key + "/searchalbum.php?" + httpx.Query("s", q.Artist, "a", q.Title)
	if err := p.http.GetJSON(ctx, endpoint, nil, &body); err != nil {
		return nil, err
	}
	if len(body.Album) == 0 {
		return nil, httpx.ErrNotFound
	}
	a := body.Album[0]

	meta := models.Metadata{
		Title: a.Name, Album: a.Name, Artist: a.Artist, AlbumArtist: a.Artist,
		Overview: strings.TrimSpace(a.Description), Label: a.Label,
	}
	meta.Year, _ = strconv.Atoi(a.Year)
	if a.Genre != "" {
		meta.Genres = []string{a.Genre}
	}
	if score, err := strconv.ParseFloat(a.Score, 64); err == nil && score > 0 {
		meta.Ratings = map[string]float64{"audiodb": score}
	}
	ids := map[string]string{"audiodb": a.ID}
	if a.MBGroupID != "" {
		ids["musicbrainzReleaseGroup"] = a.MBGroupID
	}
	res := &providers.Result{IDs: ids, Meta: meta}
	if a.Thumb != "" {
		res.Images = append(res.Images, models.Image{Type: models.ImagePoster, URL: a.Thumb, Provider: "audiodb"})
	}
	if a.CDArt != "" {
		res.Images = append(res.Images, models.Image{Type: models.ImageDisc, URL: a.CDArt, Provider: "audiodb"})
	}
	return res, nil
}
