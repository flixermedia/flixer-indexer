// Package httpx is the HTTP client every metadata provider shares. It paces
// requests per provider, backs off politely on 429/503, counts what happened
// for the admin dashboard, and keeps credentials out of error messages.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ErrNotFound is returned for 404 responses.
var ErrNotFound = errors.New("not found")

// Stats counts a provider's traffic.
type Stats struct {
	Requests    atomic.Int64
	Errors      atomic.Int64
	RateLimited atomic.Int64
}

// Snapshot is a JSON-friendly copy of Stats.
type Snapshot struct {
	Requests    int64 `json:"requests"`
	Errors      int64 `json:"errors"`
	RateLimited int64 `json:"rateLimited"`
}

func (s *Stats) Snapshot() Snapshot {
	return Snapshot{s.Requests.Load(), s.Errors.Load(), s.RateLimited.Load()}
}

// Client performs requests for one provider.
type Client struct {
	Name      string
	UserAgent string
	// Interval is the minimum time between two requests.
	Interval time.Duration
	// MaxRetries is how many times a 429/503/network error is retried.
	MaxRetries int
	Stats      *Stats
	HTTP       *http.Client

	mu   sync.Mutex
	next time.Time
}

// New returns a client with sensible defaults.
func New(name, userAgent string, interval time.Duration) *Client {
	return &Client{
		Name: name, UserAgent: userAgent, Interval: interval, MaxRetries: 3,
		Stats: &Stats{}, HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) wait(ctx context.Context) error {
	if c.Interval <= 0 {
		return nil
	}
	c.mu.Lock()
	now := time.Now()
	at := c.next
	if at.Before(now) {
		at = now
	}
	c.next = at.Add(c.Interval)
	c.mu.Unlock()

	select {
	case <-time.After(time.Until(at)):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Request describes one call.
type Request struct {
	Method  string
	URL     string
	Headers map[string]string
	Body    any // marshalled as JSON when set
}

// Do performs the request and returns the response body.
func (c *Client) Do(ctx context.Context, r Request) ([]byte, error) {
	method := r.Method
	if method == "" {
		method = http.MethodGet
	}
	var payload []byte
	if r.Body != nil {
		var err error
		if payload, err = json.Marshal(r.Body); err != nil {
			return nil, err
		}
	}

	var lastErr error
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if err := c.wait(ctx); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, r.URL, bytes.NewReader(payload))
		if err != nil {
			return nil, c.redact(err)
		}
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range r.Headers {
			req.Header.Set(k, v)
		}

		c.Stats.Requests.Add(1)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			c.Stats.Errors.Add(1)
			lastErr = c.redact(err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.sleep(ctx, backoff(attempt, 0))
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()

		switch {
		case resp.StatusCode == http.StatusNotFound:
			return nil, ErrNotFound
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable:
			c.Stats.RateLimited.Add(1)
			lastErr = fmt.Errorf("%s: status %d", c.Name, resp.StatusCode)
			c.sleep(ctx, backoff(attempt, retryAfter(resp)))
			continue
		case resp.StatusCode < 200 || resp.StatusCode > 299:
			c.Stats.Errors.Add(1)
			return nil, fmt.Errorf("%s: status %d", c.Name, resp.StatusCode)
		case readErr != nil:
			c.Stats.Errors.Add(1)
			return nil, readErr
		}
		return body, nil
	}
	return nil, lastErr
}

// GetJSON fetches url and decodes the JSON response into out.
func (c *Client) GetJSON(ctx context.Context, rawURL string, headers map[string]string, out any) error {
	return c.JSON(ctx, Request{URL: rawURL, Headers: headers}, out)
}

// JSON performs r and decodes the JSON response into out.
func (c *Client) JSON(ctx context.Context, r Request, out any) error {
	body, err := c.Do(ctx, r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		c.Stats.Errors.Add(1)
		return fmt.Errorf("%s: decode response: %w", c.Name, err)
	}
	return nil
}

func (c *Client) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

// redact strips URLs (which may carry API keys) out of transport errors.
func (c *Client) redact(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s: %s request failed: %w", c.Name, urlErr.Op, urlErr.Err)
	}
	return fmt.Errorf("%s: %w", c.Name, err)
}

func backoff(attempt int, hinted time.Duration) time.Duration {
	if hinted > 0 {
		if hinted > 30*time.Second {
			hinted = 30 * time.Second
		}
		return hinted
	}
	d := time.Duration(500*(1<<attempt)) * time.Millisecond
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

func retryAfter(resp *http.Response) time.Duration {
	value := resp.Header.Get("Retry-After")
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}

// Query builds a query string, skipping empty values.
func Query(pairs ...string) string {
	values := url.Values{}
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			values.Set(pairs[i], pairs[i+1])
		}
	}
	return values.Encode()
}
