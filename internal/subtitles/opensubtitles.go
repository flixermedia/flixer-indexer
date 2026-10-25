package subtitles

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBase  = "https://api.opensubtitles.com/api/v1"
	maxSubtitle  = 5 << 20
	maxJSON      = 4 << 20
	quotaBackoff = time.Hour
)

var (
	// ErrNotConfigured means no OpenSubtitles API key is set.
	ErrNotConfigured = errors.New("opensubtitles is not configured")
	// ErrQuota means the download quota (or rate limit) was reached.
	ErrQuota = errors.New("opensubtitles quota reached")
	// ErrNotFound means nothing matched.
	ErrNotFound = errors.New("no subtitles found")
	// ErrCredentials means downloading needs an OpenSubtitles account that is missing or rejected.
	ErrCredentials = errors.New("opensubtitles credentials missing or rejected")
)

// Client talks to the OpenSubtitles REST API. Searching needs only an API key;
// downloading also needs a user account.
type Client struct {
	APIKey    string
	Username  string
	Password  string
	UserAgent string
	BaseURL   string
	HTTP      *http.Client

	mu           sync.Mutex
	token        string
	tokenBase    string
	tokenExpires time.Time
	blockedUntil time.Time
}

func NewClient(apiKey, username, password, userAgent string) *Client {
	return &Client{
		APIKey: apiKey, Username: username, Password: password, UserAgent: userAgent,
		BaseURL: defaultBase, HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// Query describes what to look for. Zero fields are left out.
type Query struct {
	Text      string
	Year      int
	Season    int
	Episode   int
	Episodic  bool
	IMDB      string
	TMDB      string
	ParentIMD string
	ParentTMD string
	Hash      string
	Languages []string
}

// Result is one downloadable subtitle.
type Result struct {
	FileID          int     `json:"fileId"`
	Language        string  `json:"language"`
	Release         string  `json:"release"`
	FileName        string  `json:"fileName"`
	Downloads       int     `json:"downloads"`
	HearingImpaired bool    `json:"hearingImpaired"`
	HashMatch       bool    `json:"hashMatch"`
	Rating          float64 `json:"rating"`
	Uploader        string  `json:"uploader,omitempty"`
}

func digits(id string) string {
	id = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "tt")
	if _, err := strconv.Atoi(id); err != nil {
		return ""
	}
	return strings.TrimLeft(id, "0")
}

func (c *Client) do(ctx context.Context, method, endpoint string, body any, out any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Api-Key", c.APIKey)
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token != "" && strings.HasSuffix(endpoint, "/download") {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusNotAcceptable:
		c.block()
		return resp, ErrQuota
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return resp, fmt.Errorf("opensubtitles: %s %s returned status %d", method, endpoint, resp.StatusCode)
	}
	if out != nil {
		return resp, json.NewDecoder(io.LimitReader(resp.Body, maxJSON)).Decode(out)
	}
	return resp, nil
}

func (c *Client) block() {
	c.mu.Lock()
	c.blockedUntil = time.Now().Add(quotaBackoff)
	c.mu.Unlock()
}

func (c *Client) blocked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Before(c.blockedUntil)
}

func (c *Client) base() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tokenBase != "" && time.Now().Before(c.tokenExpires) {
		return c.tokenBase
	}
	return strings.TrimRight(c.BaseURL, "/")
}

// login exchanges the account for a token; OpenSubtitles requires one to download.
func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	valid := c.token != "" && time.Now().Before(c.tokenExpires)
	c.mu.Unlock()
	if valid {
		return nil
	}
	if c.Username == "" || c.Password == "" {
		return fmt.Errorf("%w: OPENSUBTITLES_USERNAME and OPENSUBTITLES_PASSWORD are required to download", ErrCredentials)
	}
	var out struct {
		Token   string `json:"token"`
		BaseURL string `json:"base_url"`
	}
	resp, err := c.do(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/login", map[string]string{"username": c.Username, "password": c.Password}, &out)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest) {
			return fmt.Errorf("%w: login returned status %d", ErrCredentials, resp.StatusCode)
		}
		return err
	}
	if out.Token == "" {
		return errors.New("opensubtitles: login returned no token")
	}
	c.mu.Lock()
	c.token = out.Token
	c.tokenBase = strings.TrimRight(c.BaseURL, "/")
	if host := strings.TrimSpace(out.BaseURL); host != "" {
		c.tokenBase = "https://" + strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://") + "/api/v1"
	}
	c.tokenExpires = time.Now().Add(20 * time.Hour)
	c.mu.Unlock()
	return nil
}

type searchResponse struct {
	Data []struct {
		Attributes struct {
			Language        string  `json:"language"`
			Release         string  `json:"release"`
			DownloadCount   int     `json:"download_count"`
			HearingImpaired bool    `json:"hearing_impaired"`
			HashMatch       bool    `json:"moviehash_match"`
			AITranslated    bool    `json:"ai_translated"`
			Ratings         float64 `json:"ratings"`
			Uploader        struct {
				Name string `json:"name"`
			} `json:"uploader"`
			Files []struct {
				FileID   int    `json:"file_id"`
				FileName string `json:"file_name"`
			} `json:"files"`
		} `json:"attributes"`
	} `json:"data"`
}

// Search returns subtitles ranked best first: hash matches, then the most downloaded.
func (c *Client) Search(ctx context.Context, q Query) ([]Result, error) {
	if c == nil || c.APIKey == "" {
		return nil, ErrNotConfigured
	}
	if c.blocked() {
		return nil, ErrQuota
	}
	params := url.Values{}
	if len(q.Languages) > 0 {
		params.Set("languages", strings.ToLower(strings.Join(q.Languages, ",")))
	}
	if q.Episodic {
		params.Set("type", "episode")
	} else {
		params.Set("type", "movie")
	}
	set := func(key, value string) {
		if value != "" {
			params.Set(key, value)
		}
	}
	if q.Year > 0 && !q.Episodic {
		params.Set("year", strconv.Itoa(q.Year))
	}
	if q.Season > 0 {
		params.Set("season_number", strconv.Itoa(q.Season))
	}
	if q.Episode > 0 {
		params.Set("episode_number", strconv.Itoa(q.Episode))
	}
	set("imdb_id", digits(q.IMDB))
	set("tmdb_id", digits(q.TMDB))
	set("parent_imdb_id", digits(q.ParentIMD))
	set("parent_tmdb_id", digits(q.ParentTMD))
	set("moviehash", q.Hash)
	// Provider ids are exact, so free text is only a fallback.
	if params.Get("imdb_id") == "" && params.Get("tmdb_id") == "" && params.Get("parent_imdb_id") == "" && params.Get("parent_tmdb_id") == "" {
		set("query", strings.ToLower(strings.TrimSpace(q.Text)))
	}
	params.Set("ai_translated", "exclude")

	var out searchResponse
	if _, err := c.do(ctx, http.MethodGet, c.base()+"/subtitles?"+params.Encode(), nil, &out); err != nil {
		return nil, err
	}
	var results []Result
	for _, d := range out.Data {
		a := d.Attributes
		if len(a.Files) == 0 || a.AITranslated {
			continue
		}
		results = append(results, Result{
			FileID: a.Files[0].FileID, Language: a.Language, Release: a.Release, FileName: a.Files[0].FileName,
			Downloads: a.DownloadCount, HearingImpaired: a.HearingImpaired, HashMatch: a.HashMatch,
			Rating: a.Ratings, Uploader: a.Uploader.Name,
		})
	}
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.HashMatch != b.HashMatch {
			return a.HashMatch
		}
		if a.HearingImpaired != b.HearingImpaired {
			return !a.HearingImpaired
		}
		return a.Downloads > b.Downloads
	})
	return results, nil
}

// Download fetches the subtitle text for a search result.
func (c *Client) Download(ctx context.Context, fileID int) ([]byte, error) {
	if c == nil || c.APIKey == "" {
		return nil, ErrNotConfigured
	}
	if c.blocked() {
		return nil, ErrQuota
	}
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	var out struct {
		Link string `json:"link"`
	}
	if _, err := c.do(ctx, http.MethodPost, c.base()+"/download", map[string]any{"file_id": fileID, "sub_format": "srt"}, &out); err != nil {
		return nil, err
	}
	link, err := url.Parse(out.Link)
	if err != nil || link.Scheme != "https" && link.Scheme != "http" {
		return nil, errors.New("opensubtitles: download returned an invalid link")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opensubtitles: subtitle file returned status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSubtitle+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxSubtitle {
		return nil, errors.New("opensubtitles: subtitle file is too large")
	}
	return data, nil
}

// Fetch downloads the best subtitle for one language and stores it beside the video.
func (c *Client) Fetch(ctx context.Context, target Target, q Query, language string) (string, error) {
	q.Languages = []string{language}
	results, err := c.Search(ctx, q)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "", ErrNotFound
	}
	data, err := c.Download(ctx, results[0].FileID)
	if err != nil {
		return "", err
	}
	return Save(target, language, data)
}

// Hash computes the OpenSubtitles movie hash: the file size plus the 64-bit
// little-endian words of the first and last 64 KiB.
func Hash(path string) (string, error) {
	const chunk = 64 << 10
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() < chunk*2 {
		return "", errors.New("file too small to hash")
	}
	sum := uint64(info.Size())
	buf := make([]byte, chunk)
	for _, offset := range []int64{0, info.Size() - chunk} {
		if _, err := f.ReadAt(buf, offset); err != nil {
			return "", err
		}
		for i := 0; i < chunk; i += 8 {
			sum += binary.LittleEndian.Uint64(buf[i : i+8])
		}
	}
	return fmt.Sprintf("%016x", sum), nil
}
