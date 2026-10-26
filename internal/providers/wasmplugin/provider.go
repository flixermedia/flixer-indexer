package wasmplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/httpx"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/providers"
)

const pluginAPIVersion = 1

var pluginIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[.-][a-z0-9]+)*$`)

type Executor interface {
	Run(context.Context, string, []byte) ([]byte, error)
}

type moduleValidator interface {
	Validate(context.Context, string) error
}

type Provider struct {
	id           string
	path         string
	capabilities map[string]bool
	executor     Executor
}

type pluginRequest struct {
	APIVersion int               `json:"apiVersion"`
	Method     string            `json:"method"`
	Query      *providers.Query  `json:"query,omitempty"`
	IDs        map[string]string `json:"ids,omitempty"`
	Season     int               `json:"season,omitempty"`
	Episode    int               `json:"episode,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	ItemSeason *int              `json:"itemSeason,omitempty"`
	Language   string            `json:"language,omitempty"`
}

type pluginResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func New(root string, module models.PluginModule, executor Executor) (*Provider, error) {
	if executor == nil {
		return nil, errors.New("plugin executor is not configured")
	}
	path, err := ResolveModulePath(root, module.ID, module.Entry)
	if err != nil {
		return nil, err
	}
	if validator, ok := executor.(moduleValidator); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := validator.Validate(ctx, path); err != nil {
			return nil, fmt.Errorf("plugin module failed WASM validation: %w", err)
		}
	}
	capabilities := make(map[string]bool, len(module.Capabilities))
	for _, capability := range module.Capabilities {
		capabilities[capability] = true
	}
	return &Provider{id: module.ID, path: path, capabilities: capabilities, executor: executor}, nil
}

func ResolveModulePath(root, pluginID, entry string) (string, error) {
	if !pluginIDPattern.MatchString(pluginID) || entry == "" || filepath.IsAbs(entry) || strings.Contains(entry, "\\") {
		return "", errors.New("plugin module path is invalid")
	}
	cleanEntry := filepath.Clean(entry)
	if cleanEntry == "." || cleanEntry == ".." || strings.HasPrefix(cleanEntry, ".."+string(filepath.Separator)) || filepath.Ext(cleanEntry) != ".wasm" {
		return "", errors.New("plugin entry must be a relative WASI .wasm module path")
	}
	pluginRoot := filepath.Join(root, pluginID)
	path := filepath.Join(pluginRoot, cleanEntry)
	relative, err := filepath.Rel(pluginRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("plugin entry escapes its installation directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat plugin module: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("plugin module is not a regular file")
	}
	return path, nil
}

func (p *Provider) Name() string { return p.id }

func (p *Provider) Supports(capability string) bool { return p.capabilities[capability] }

func (p *Provider) Movie(ctx context.Context, query providers.Query) (*providers.Result, error) {
	return p.lookup(ctx, pluginRequest{Method: "movie", Query: &query})
}

func (p *Provider) Series(ctx context.Context, query providers.Query) (*providers.Result, error) {
	return p.lookup(ctx, pluginRequest{Method: "series", Query: &query})
}

func (p *Provider) Season(ctx context.Context, ids map[string]string, season int, language string) (*providers.Result, error) {
	return p.lookup(ctx, pluginRequest{Method: "season", IDs: ids, Season: season, Language: language})
}

func (p *Provider) Episode(ctx context.Context, ids map[string]string, season, episode int, language string) (*providers.Result, error) {
	return p.lookup(ctx, pluginRequest{Method: "episode", IDs: ids, Season: season, Episode: episode, Language: language})
}

func (p *Provider) Album(ctx context.Context, query providers.Query) (*providers.Result, error) {
	return p.lookup(ctx, pluginRequest{Method: "album", Query: &query})
}

func (p *Provider) Images(ctx context.Context, kind string, ids map[string]string, season *int) ([]models.Image, error) {
	request := pluginRequest{APIVersion: pluginAPIVersion, Method: "images", Kind: kind, IDs: ids, ItemSeason: season}
	response, err := p.call(ctx, request)
	if err != nil || len(response) == 0 || string(response) == "null" {
		return nil, err
	}
	var images []models.Image
	if err := json.Unmarshal(response, &images); err != nil {
		return nil, fmt.Errorf("plugin %s returned invalid images: %w", p.id, err)
	}
	return images, nil
}

func (p *Provider) lookup(
	ctx context.Context,
	request pluginRequest,
) (*providers.Result, error) {
	request.APIVersion = pluginAPIVersion
	response, err := p.call(ctx, request)
	if err != nil || len(response) == 0 || string(response) == "null" {
		return nil, err
	}
	var result providers.Result
	if err := json.Unmarshal(response, &result); err != nil {
		return nil, fmt.Errorf("plugin %s returned invalid metadata: %w", p.id, err)
	}
	return &result, nil
}

func (p *Provider) call(ctx context.Context, request pluginRequest) (json.RawMessage, error) {
	request.APIVersion = pluginAPIVersion
	input, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	output, err := p.executor.Run(ctx, p.path, input)
	if err != nil {
		return nil, fmt.Errorf("plugin %s failed: %w", p.id, err)
	}
	var response pluginResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("plugin %s returned invalid JSON: %w", p.id, err)
	}
	if response.Error != nil {
		if response.Error.Code == "not_found" {
			return nil, httpx.ErrNotFound
		}
		code := response.Error.Code
		if len(code) > 64 {
			code = code[:64]
		}
		return nil, fmt.Errorf("plugin %s returned error code %q", p.id, code)
	}
	return response.Result, nil
}

var _ providers.MovieProvider = (*Provider)(nil)
var _ providers.SeriesProvider = (*Provider)(nil)
var _ providers.SeasonProvider = (*Provider)(nil)
var _ providers.EpisodeProvider = (*Provider)(nil)
var _ providers.AlbumProvider = (*Provider)(nil)
var _ providers.ImageProvider = (*Provider)(nil)
