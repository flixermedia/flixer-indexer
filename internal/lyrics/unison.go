package lyrics

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type unisonAttribution struct {
	SourceURL     string `json:"sourceUrl"`
	SubmitterName string `json:"submitterName"`
	SubmitterURL  string `json:"submitterUrl"`
}

func (f *Fetcher) fetchUnison(ctx context.Context, q Query) (string, error) {
	artists := artistVariants(q.Artist, q.AlbumArtist)
	if strings.TrimSpace(q.Title) == "" || len(artists) == 0 {
		return "", ErrNotFound
	}
	f.mu.Lock()
	if time.Now().Before(f.unisonLimitedUntil) {
		f.mu.Unlock()
		return "", fmt.Errorf("unison: rate limited")
	}
	at := f.unisonNext
	if at.Before(time.Now()) {
		at = time.Now()
	}
	f.unisonNext = at.Add(requestGap)
	f.mu.Unlock()
	if err := sleep(ctx, time.Until(at)); err != nil {
		return "", err
	}
	params := url.Values{"song": {strings.TrimSpace(q.Title)}, "artist": {artists[0]}}
	if q.Album != "" {
		params.Set("album", q.Album)
	}
	if q.Duration > 0 {
		params.Set("duration", strconv.FormatFloat(q.Duration, 'f', 3, 64))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(f.UnisonBaseURL, "/")+"/lyrics?"+params.Encode(), nil)
	if err != nil {
		return "", err
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		f.mu.Lock()
		f.unisonLimitedUntil = time.Now().Add(rateLimitDelay(resp.Header.Get("Retry-After")))
		f.mu.Unlock()
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unison: unexpected status %d", resp.StatusCode)
	}
	var result struct {
		Success bool `json:"success"`
		Data    struct {
			VideoID   string `json:"videoId"`
			Format    string `json:"format"`
			SyncType  string `json:"syncType"`
			Lyrics    string `json:"lyrics"`
			Submitter struct {
				DisplayName string `json:"displayName"`
				KeyID       string `json:"keyId"`
			} `json:"submitter"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&result); err != nil {
		return "", fmt.Errorf("unison: decode response: %w", err)
	}
	data := result.Data
	if !result.Success || strings.TrimSpace(data.Lyrics) == "" || data.SyncType == "plain" {
		return "", ErrNotFound
	}
	text := data.Lyrics
	switch data.Format {
	case "lrc":
	case "ttml":
		text, err = ttmlToLRC(data.Lyrics)
		if err != nil {
			return "", fmt.Errorf("unison: parse TTML: %w", err)
		}
	default:
		return "", ErrNotFound
	}
	if !lrcTimestamp.MatchString(text) {
		return "", ErrNotFound
	}
	if data.VideoID == "" || data.Submitter.DisplayName == "" || data.Submitter.KeyID == "" {
		return "", fmt.Errorf("unison: missing required attribution")
	}
	credit, err := json.Marshal(unisonAttribution{
		SourceURL:     "https://unison.betterlyrics.org/song/" + url.PathEscape(data.VideoID),
		SubmitterName: data.Submitter.DisplayName,
		SubmitterURL:  "https://unison.betterlyrics.org/curator/" + url.PathEscape(data.Submitter.KeyID),
	})
	if err != nil {
		return "", err
	}
	return "[unison:" + base64.StdEncoding.EncodeToString(credit) + "]\n" + text, nil
}
