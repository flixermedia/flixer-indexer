// Package theintrodb reads public TheIntroDB v4 media timings.
package theintrodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/models"
)

const defaultBaseURL = "https://api.theintrodb.org/v4"

var imdbIDPattern = regexp.MustCompile(`^tt[0-9]{7,8}$`)

var ErrRateLimited = errors.New("TheIntroDB rate limited the request")

type Client struct {
	baseURL string
	http    *http.Client
	keyMu   sync.RWMutex
	apiKey  string
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

func (c *Client) SetAPIKey(apiKey string) {
	c.keyMu.Lock()
	c.apiKey = strings.TrimSpace(apiKey)
	c.keyMu.Unlock()
}

type timestamp struct {
	StartMs    json.RawMessage `json:"startMs"`
	StartSnake json.RawMessage `json:"start_ms"`
	EndMs      json.RawMessage `json:"endMs"`
	EndSnake   json.RawMessage `json:"end_ms"`
}

type mediaResponse struct {
	Kind    string      `json:"kind"`
	Type    string      `json:"type"`
	Intro   []timestamp `json:"intro"`
	Credits []timestamp `json:"credits"`
}

// GetMedia uses the v4 public GET /media contract and normalizes its timestamps
// into the fields Flixer persists.
func (c *Client) GetMedia(ctx context.Context, item models.IntroSyncCandidate) (map[string]models.IntroTimeRange, error) {
	query := url.Values{}
	if value, ok := numericID(item.ProviderIDs["tvdb"], 100_000_000); ok {
		query.Set("tvdb_id", value)
	} else if value, ok := numericID(item.ProviderIDs["tmdb"], 10_000_000); ok {
		query.Set("tmdb_id", value)
	} else if value := strings.TrimSpace(item.ProviderIDs["imdb"]); imdbIDPattern.MatchString(value) {
		query.Set("imdb_id", value)
	} else {
		return map[string]models.IntroTimeRange{}, nil
	}

	switch item.Type {
	case models.ItemMovie:
		if item.DurationMs > 0 {
			query.Set("duration_ms", strconv.FormatInt(item.DurationMs, 10))
		}
	case models.ItemEpisode:
		if item.Season == nil || item.Episode == nil || *item.Season < 0 || *item.Episode < 0 {
			return map[string]models.IntroTimeRange{}, nil
		}
		query.Set("season", strconv.Itoa(*item.Season))
		query.Set("episode", strconv.Itoa(*item.Episode))
		if item.DurationMs > 0 {
			query.Set("duration_ms", strconv.FormatInt(item.DurationMs, 10))
		}
	default:
		return nil, errors.New("TheIntroDB sync only supports movies and episodes")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/media?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "flixer-indexer/1.0")
	c.keyMu.RLock()
	apiKey := c.apiKey
	c.keyMu.RUnlock()
	if apiKey != "" {
		request.Header.Set("apiKey", apiKey)
	}
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
		return nil, fmt.Errorf("TheIntroDB returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var result mediaResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode TheIntroDB response: %w", err)
	}
	if result.Kind != "" && result.Kind != "media" {
		return map[string]models.IntroTimeRange{}, nil
	}
	if result.Type != "" && result.Type != "movie" && result.Type != "tv" {
		return map[string]models.IntroTimeRange{}, nil
	}
	if item.Type == models.ItemMovie && result.Type == "tv" || item.Type == models.ItemEpisode && result.Type == "movie" {
		return map[string]models.IntroTimeRange{}, nil
	}

	segments := map[string]models.IntroTimeRange{}
	if segment, ok := firstRange(result.Intro, item.DurationMs); ok {
		segments["intro"] = segment
	}
	if segment, ok := firstRange(result.Credits, item.DurationMs); ok {
		segments["credits"] = segment
	}
	return segments, nil
}

func numericID(value string, max int64) (string, bool) {
	value = strings.TrimSpace(value)
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number <= 0 || number > max {
		return "", false
	}
	return strconv.FormatInt(number, 10), true
}

func firstRange(values []timestamp, durationMs int64) (models.IntroTimeRange, bool) {
	ranges := make([]models.IntroTimeRange, 0, len(values))
	for _, value := range values {
		startRaw := value.StartMs
		if len(startRaw) == 0 {
			startRaw = value.StartSnake
		}
		endRaw := value.EndMs
		if len(endRaw) == 0 {
			endRaw = value.EndSnake
		}
		start, validStart := readMillis(startRaw, 0)
		if !validStart || len(endRaw) == 0 {
			continue
		}
		var end int64
		if string(endRaw) == "null" {
			end = durationMs
		} else {
			var validEnd bool
			end, validEnd = readMillis(endRaw, 0)
			if !validEnd {
				continue
			}
		}
		if start < 0 || end <= start || end > 7*24*60*60*1000 {
			continue
		}
		ranges = append(ranges, models.IntroTimeRange{StartMs: start, EndMs: end})
	}
	if len(ranges) == 0 {
		return models.IntroTimeRange{}, false
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].StartMs < ranges[j].StartMs })
	return ranges[0], true
}

func readMillis(raw json.RawMessage, nullValue int64) (int64, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nullValue, len(raw) > 0
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}
