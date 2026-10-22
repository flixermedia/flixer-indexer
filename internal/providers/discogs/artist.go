package discogs

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
)

const maxProfileRunes = 4000

// Artist is a Discogs artist page.
type Artist struct {
	Name     string
	RealName string
	Profile  string
	URL      string
	ImageURL string
}

// Artist finds the artist called name and returns their profile and primary image. Only a result whose
// name matches exactly is used, because a fuzzy match would attach another act's biography.
func (p *Provider) Artist(ctx context.Context, name string) (*Artist, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" {
		return nil, httpx.ErrNotFound
	}
	var found struct {
		Results []struct {
			ID    int    `json:"id"`
			Title string `json:"title"`
		} `json:"results"`
	}
	query := httpx.Query("type", "artist", "q", name, "per_page", "10")
	if err := p.http.GetJSON(ctx, p.base+"/database/search?"+query, p.headers(), &found); err != nil {
		return nil, err
	}
	id := 0
	for _, result := range found.Results {
		if strings.EqualFold(trimDisambiguation(result.Title), name) {
			id = result.ID
			break
		}
	}
	if id == 0 {
		return nil, httpx.ErrNotFound
	}

	var r struct {
		Name     string `json:"name"`
		RealName string `json:"realname"`
		Profile  string `json:"profile"`
		URI      string `json:"uri"`
		Images   []struct {
			Type string `json:"type"`
			URI  string `json:"uri"`
		} `json:"images"`
	}
	if err := p.http.GetJSON(ctx, p.base+"/artists/"+strconv.Itoa(id), p.headers(), &r); err != nil {
		return nil, err
	}
	artist := &Artist{Name: trimDisambiguation(r.Name), RealName: strings.TrimSpace(r.RealName), Profile: CleanMarkup(r.Profile), URL: r.URI}
	for _, img := range r.Images {
		if img.URI != "" && (artist.ImageURL == "" || img.Type == "primary") {
			artist.ImageURL = img.URI
		}
	}
	if artist.Profile == "" && artist.ImageURL == "" {
		return nil, httpx.ErrNotFound
	}
	return artist, nil
}

var (
	namedRef   = regexp.MustCompile(`\[[almrgt]=([^\]]+)\]`)
	idRef      = regexp.MustCompile(`\[[almrgt]\d+\]`)
	linkTag    = regexp.MustCompile(`(?is)\[url=[^\]]*\](.*?)\[/url\]`)
	bareLink   = regexp.MustCompile(`(?i)\[url=[^\]]*\]|\[/?url\]`)
	styleTag   = regexp.MustCompile(`(?i)\[/?[biuq]\]`)
	blankLines = regexp.MustCompile(`\n{3,}`)
)

// CleanMarkup turns Discogs' bracket markup ([a=Name], [url=...]text[/url], [b]) into plain text.
func CleanMarkup(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = linkTag.ReplaceAllString(text, "$1")
	text = namedRef.ReplaceAllString(text, "$1")
	text = idRef.ReplaceAllString(text, "")
	text = bareLink.ReplaceAllString(text, "")
	text = styleTag.ReplaceAllString(text, "")
	text = blankLines.ReplaceAllString(text, "\n\n")
	text = strings.TrimSpace(text)
	if runes := []rune(text); len(runes) > maxProfileRunes {
		text = strings.TrimSpace(string(runes[:maxProfileRunes])) + "…"
	}
	return text
}
