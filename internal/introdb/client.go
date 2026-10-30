// Package introdb reads public IntroDB segment timings.
package introdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

const defaultBaseURL = "https://api.introdb.app"

var imdbIDPattern = regexp.MustCompile(`^tt[0-9]{7,8}$`)

var ErrRateLimited = errors.New("IntroDB rate limited the request")

type Client struct {
	baseURL string
	http    *http.Client
}

func New(baseURL string) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 12 * time.Second},
	}
}

type segment struct {
	StartMs *int64 `json:"start_ms"`
	EndMs   *int64 `json:"end_ms"`
}

type segmentsResponse struct {
	Intro *segment `json:"intro"`
	Outro *segment `json:"outro"`
}

// GetMedia reads public IntroDB timings using an IMDb ID.
func (c *Client) GetMedia(ctx context.Context, item models.IntroSyncCandidate) (map[string]models.IntroTimeRange, error) {
	imdbID := strings.TrimSpace(item.ProviderIDs["imdb"])
	if !imdbIDPattern.MatchString(imdbID) {
		return map[string]models.IntroTimeRange{}, nil
	}

	query := url.Values{"imdb_id": []string{imdbID}}
	switch item.Type {
	case models.ItemMovie:
		query.Set("is_movie", "true")
	case models.ItemEpisode:
		if item.Season == nil || item.Episode == nil || *item.Season < 1 || *item.Episode < 1 {
			return map[string]models.IntroTimeRange{}, nil
		}
		query.Set("season", strconv.Itoa(*item.Season))
		query.Set("episode", strconv.Itoa(*item.Episode))
	default:
		return nil, errors.New("IntroDB sync only supports movies and episodes")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/segments?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "flixer-indexer/1.0")
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return map[string]models.IntroTimeRange{}, nil
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if response.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: returned status %d", ErrRateLimited, response.StatusCode)
		}
		return nil, fmt.Errorf("IntroDB returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var result segmentsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode IntroDB response: %w", err)
	}

	segments := map[string]models.IntroTimeRange{}
	if timing, ok := validRange(result.Intro); ok {
		segments["intro"] = timing
	}
	if timing, ok := validRange(result.Outro); ok {
		segments["credits"] = timing
	}
	return segments, nil
}

func validRange(value *segment) (models.IntroTimeRange, bool) {
	if value == nil || value.StartMs == nil || value.EndMs == nil {
		return models.IntroTimeRange{}, false
	}
	if *value.StartMs < 0 || *value.EndMs <= *value.StartMs || *value.EndMs > 7*24*60*60*1000 {
		return models.IntroTimeRange{}, false
	}
	return models.IntroTimeRange{StartMs: *value.StartMs, EndMs: *value.EndMs}, true
}
