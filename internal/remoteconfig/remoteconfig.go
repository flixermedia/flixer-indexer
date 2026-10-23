// Package remoteconfig fetches runtime configuration from flixer's signed internal endpoint.
package remoteconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/flixermedia/flixer-indexer/internal/auth"
)

const path = "/api/internal/config/indexer"

// Redis is the Redis connection flixer wants the indexer to use.
type Redis struct {
	Addr     string `json:"addr"`
	Username string `json:"username"`
	Password string `json:"password"`
	DB       int    `json:"db"`
	TLS      bool   `json:"tls"`
}

// Config is the indexer's runtime configuration. Version changes whenever flixer's configuration does.
type Config struct {
	Version int    `json:"version"`
	Redis   *Redis `json:"redis"`
}

// Fetch asks flixer for the configuration once.
func Fetch(ctx context.Context, client *http.Client, baseURL string, secret []byte) (Config, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return Config{}, err
	}
	auth.Sign(secret, req, nil, time.Now())
	res, err := client.Do(req)
	if err != nil {
		return Config{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Config{}, fmt.Errorf("flixer answered %d", res.StatusCode)
	}
	var cfg Config
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	return cfg, nil
}

// Wait fetches until flixer answers with a usable Redis configuration, retrying with backoff until ctx ends.
func Wait(ctx context.Context, baseURL string, secret []byte, log *slog.Logger) (Config, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	delay := time.Second
	for {
		cfg, err := Fetch(ctx, client, baseURL, secret)
		if err == nil && cfg.Redis != nil && cfg.Redis.Addr != "" {
			return cfg, nil
		}
		reason := "flixer has no Redis configured yet"
		if err != nil {
			reason = err.Error()
		}
		log.Warn("waiting for flixer configuration", "reason", reason, "retry_in", delay.String())
		select {
		case <-ctx.Done():
			return Config{}, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 15*time.Second {
			delay *= 2
		}
	}
}
