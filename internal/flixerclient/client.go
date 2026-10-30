// Package flixerclient talks to flixer's internal indexer API. flixer owns the
// database, so every read of configuration and every write of media goes
// through here.
package flixerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/auth"
	"github.com/flixermedia/flixer-indexer/internal/models"
)

// Endpoints flixer is expected to expose.
const (
	librariesPath = "/api/indexer/libraries"
	settingsPath  = "/api/indexer/settings"
	itemsPath     = "/api/indexer/media/items"
	introSyncPath = "/api/indexer/media/intro-sync"
	introSavePath = "/api/indexer/media/intro-segments"
)

// API is everything the indexer needs from flixer.
type API interface {
	Libraries(ctx context.Context) ([]models.Library, error)
	Settings(ctx context.Context) (models.Settings, error)
	// UpsertItem creates or updates an item and returns flixer's id for it.
	UpsertItem(ctx context.Context, item models.Item) (string, error)
	// IntroSyncCandidates pages through indexed movies or episodes for a backfill.
	IntroSyncCandidates(ctx context.Context, itemType, libraryID, afterID string, limit int) ([]models.IntroSyncCandidate, string, error)
	// ExistingIntroSegments reports stored intro and credits timings for an item.
	ExistingIntroSegments(ctx context.Context, itemID string) (models.ExistingIntroSegments, error)
	// UpsertIntroSegments replaces TheIntroDB's stored timings for an item.
	UpsertIntroSegments(ctx context.Context, update models.IntroSegmentUpdate) error
	// PruneLibrary deletes the library's items not seen since the given scan
	// start (RFC 3339) and returns how many were removed.
	PruneLibrary(ctx context.Context, libraryID, scanStartedAt string) (int, error)
}

// ErrPermanent marks a response that retrying will not fix.
var ErrPermanent = errors.New("flixer rejected the request")

// Client is the HTTP implementation of API.
type Client struct {
	baseURL string
	secret  []byte
	http    *http.Client
}

func New(baseURL string, secret []byte) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		secret:  secret,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	auth.Sign(c.secret, req, body, time.Now())

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := fmt.Errorf("flixer %s %s returned status %d", method, path, resp.StatusCode)
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return fmt.Errorf("%w: %v", ErrPermanent, err)
		}
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload, out)
}

func (c *Client) Libraries(ctx context.Context) ([]models.Library, error) {
	var body struct {
		Libraries []models.Library `json:"libraries"`
	}
	if err := c.do(ctx, http.MethodGet, librariesPath, nil, &body); err != nil {
		return nil, err
	}
	for i := range body.Libraries {
		normalizeLibrary(&body.Libraries[i])
	}
	return body.Libraries, nil
}

func (c *Client) Settings(ctx context.Context) (models.Settings, error) {
	var settings models.Settings
	err := c.do(ctx, http.MethodGet, settingsPath, nil, &settings)
	return settings, err
}

func (c *Client) UpsertItem(ctx context.Context, item models.Item) (string, error) {
	var body struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, http.MethodPost, itemsPath, item, &body); err != nil {
		return "", err
	}
	return body.ID, nil
}

func (c *Client) IntroSyncCandidates(ctx context.Context, itemType, libraryID, afterID string, limit int) ([]models.IntroSyncCandidate, string, error) {
	query := url.Values{}
	query.Set("type", itemType)
	query.Set("limit", fmt.Sprint(limit))
	if libraryID != "" {
		query.Set("libraryId", libraryID)
	}
	if afterID != "" {
		query.Set("afterId", afterID)
	}
	var body struct {
		Items      []models.IntroSyncCandidate `json:"items"`
		NextCursor string                      `json:"nextCursor"`
	}
	if err := c.do(ctx, http.MethodGet, introSyncPath+"?"+query.Encode(), nil, &body); err != nil {
		return nil, "", err
	}
	return body.Items, body.NextCursor, nil
}

func (c *Client) ExistingIntroSegments(ctx context.Context, itemID string) (models.ExistingIntroSegments, error) {
	path := introSavePath + "?itemId=" + url.QueryEscape(itemID)
	var body struct {
		Segments models.ExistingIntroSegments `json:"segments"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &body); err != nil {
		return nil, err
	}
	return body.Segments, nil
}

func (c *Client) UpsertIntroSegments(ctx context.Context, update models.IntroSegmentUpdate) error {
	return c.do(ctx, http.MethodPost, introSavePath, update, nil)
}

func (c *Client) PruneLibrary(ctx context.Context, libraryID, scanStartedAt string) (int, error) {
	var body struct {
		Removed int `json:"removed"`
	}
	path := librariesPath + "/" + url.PathEscape(libraryID) + "/prune"
	if err := c.do(ctx, http.MethodPost, path, map[string]string{"scanStartedAt": scanStartedAt}, &body); err != nil {
		return 0, err
	}
	return body.Removed, nil
}

func normalizeLibrary(lib *models.Library) {
	if lib.ImageStorage != models.StorageAlongside {
		lib.ImageStorage = models.StorageCache
	}
}
