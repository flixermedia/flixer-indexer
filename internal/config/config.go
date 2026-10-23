package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Queue names, shared by the pipeline, the pool and the API.
const (
	QueueScan     = "scan"
	QueueIdentify = "identify"
	QueueMetadata = "metadata"
	QueueImages   = "images"
	QueueProbe    = "probe"
	QueueRefresh  = "refresh"
)

// Queues lists every queue in pipeline order.
var Queues = []string{QueueScan, QueueIdentify, QueueMetadata, QueueImages, QueueProbe, QueueRefresh}

// MaxConcurrency caps what an admin can set for a single queue.
const MaxConcurrency = 64

// Roles select which halves of the service run in this process.
const (
	RoleAll    = "all"
	RoleAPI    = "api"
	RoleWorker = "worker"
)

type Config struct {
	Port       string
	Role       string
	FlixerURL  string
	Secret     []byte
	Stub       bool
	StubLibs   string
	StubOutput string
	RedisAddr  string
	RedisUser  string
	RedisPass  string
	RedisDB    int
	RedisTLS   bool
	// RedisFromEnv is true when REDIS_ADDR is set; otherwise flixer supplies the Redis settings.
	RedisFromEnv bool
	CachePath    string
	FFprobePath  string
	FFmpegPath   string
	Concurrency  map[string]int
	LogLevel     slog.Level
	// ProviderKeys seed provider keys from the environment; flixer's values win.
	ProviderKeys map[string]string
	UserAgent    string
	Language     string
	// LyricsFetch enables downloading missing .lrc files from lrclib.net.
	LyricsFetch bool
	// OpenSubtitles credentials enable subtitle search and download; SubtitleLanguages are fetched automatically.
	OpenSubtitlesKey      string
	OpenSubtitlesUser     string
	OpenSubtitlesPassword string
	SubtitleLanguages     []string
	SubtitleAutoFetch     bool
	// WatchFolders scans a library automatically when files appear in its folders.
	WatchFolders bool
	// WatchDebounce is how long a library must be quiet after a change before it is scanned.
	WatchDebounce time.Duration
	// WatchRescan scans every library on this interval, for network shares that report no changes. Zero disables it.
	WatchRescan time.Duration
}

// secretFileWait is how long to wait for flixer to publish the shared secret on first start.
const secretFileWait = 2 * time.Minute

// ReadSecretFile returns the trimmed contents of path, waiting up to wait for it to appear. An empty path gives "".
func ReadSecretFile(path string, wait time.Duration) (string, error) {
	if path == "" {
		return "", nil
	}
	deadline := time.Now().Add(wait)
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data)), nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("read STREAM_TOKEN_SECRET_FILE: %w", err)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("STREAM_TOKEN_SECRET_FILE %s was not published by flixer in time", path)
		}
		time.Sleep(time.Second)
	}
}

func Load() (Config, error) {
	if err := loadDotEnv(".env"); err != nil {
		return Config{}, fmt.Errorf("load .env: %w", err)
	}

	port := strings.TrimPrefix(strings.TrimSpace(envOrDefault("PORT", "8092")), ":")
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return Config{}, errors.New("PORT must be a valid port number from 1 to 65535")
	}

	role := strings.ToLower(envOrDefault("ROLE", RoleAll))
	if role != RoleAll && role != RoleAPI && role != RoleWorker {
		return Config{}, errors.New("ROLE must be all, api or worker")
	}

	secret := strings.TrimSpace(os.Getenv("STREAM_TOKEN_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv("JWT_SECRET"))
	}
	if secret == "" {
		fromFile, err := ReadSecretFile(os.Getenv("STREAM_TOKEN_SECRET_FILE"), secretFileWait)
		if err != nil {
			return Config{}, err
		}
		secret = fromFile
	}
	if len([]byte(secret)) < 32 {
		return Config{}, errors.New("STREAM_TOKEN_SECRET, STREAM_TOKEN_SECRET_FILE or JWT_SECRET must contain at least 32 bytes")
	}

	level, err := parseLogLevel(envOrDefault("LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}

	redisDB, err := strconv.Atoi(envOrDefault("REDIS_DB", "0"))
	if err != nil || redisDB < 0 {
		return Config{}, errors.New("REDIS_DB must be a non-negative integer")
	}

	defaults := map[string]int{
		QueueScan: 2, QueueIdentify: 12, QueueMetadata: 12,
		QueueImages: 8, QueueProbe: 6, QueueRefresh: 4,
	}
	concurrency := make(map[string]int, len(defaults))
	for queue, fallback := range defaults {
		key := "CONCURRENCY_" + strings.ToUpper(queue)
		n, err := strconv.Atoi(envOrDefault(key, strconv.Itoa(fallback)))
		if err != nil || n < 1 || n > MaxConcurrency {
			return Config{}, fmt.Errorf("%s must be an integer from 1 to %d", key, MaxConcurrency)
		}
		concurrency[queue] = n
	}

	ffprobe := envOrDefault("FFPROBE_PATH", "ffprobe")
	keys := map[string]string{}
	for provider, env := range map[string]string{
		"tmdb": "TMDB_API_KEY", "tvdb": "TVDB_API_KEY", "omdb": "OMDB_API_KEY",
		"fanart": "FANART_API_KEY", "audiodb": "AUDIODB_API_KEY",
		"discogs": "DISCOGS_TOKEN", "anidb": "ANIDB_CLIENT",
	} {
		if value := strings.TrimSpace(os.Getenv(env)); value != "" {
			keys[provider] = value
		}
	}

	cachePath := envOrDefault("CACHE_PATH", "./cache/metadata")
	if abs, err := filepath.Abs(cachePath); err == nil {
		cachePath = abs
	}
	flixerURL := envOrDefault("FLIXER_URL", envOrDefault("FLIXER_INTERNAL_URL", "http://localhost:8083"))
	debounce, err := parseDuration("WATCH_DEBOUNCE", "30s")
	if err != nil {
		return Config{}, err
	}
	rescan, err := parseDuration("WATCH_RESCAN_INTERVAL", "0")
	if err != nil {
		return Config{}, err
	}
	return Config{
		Port:         port,
		Role:         role,
		FlixerURL:    strings.TrimRight(flixerURL, "/"),
		Secret:       []byte(secret),
		Stub:         strings.EqualFold(os.Getenv("FLIXER_MODE"), "stub"),
		StubLibs:     os.Getenv("STUB_LIBRARIES_FILE"),
		StubOutput:   envOrDefault("STUB_OUTPUT_FILE", "./cache/stub-items.jsonl"),
		RedisAddr:    envOrDefault("REDIS_ADDR", "localhost:6379"),
		RedisUser:    os.Getenv("REDIS_USERNAME"),
		RedisPass:    os.Getenv("REDIS_PASSWORD"),
		RedisDB:      redisDB,
		RedisTLS:     strings.EqualFold(os.Getenv("REDIS_TLS"), "true"),
		RedisFromEnv: strings.TrimSpace(os.Getenv("REDIS_ADDR")) != "",
		CachePath:    cachePath,
		FFprobePath:  ffprobe,
		FFmpegPath:   envOrDefault("FFMPEG_PATH", "ffmpeg"),
		Concurrency:  concurrency,
		LogLevel:     level,
		ProviderKeys: keys,
		UserAgent:    envOrDefault("USER_AGENT", "flixer-indexer/1.0 (+https://github.com/flixermedia/flixer-indexer)"),
		Language:     envOrDefault("METADATA_LANGUAGE", "en"),
		LyricsFetch:  !strings.EqualFold(strings.TrimSpace(os.Getenv("LYRICS_FETCH")), "false"),

		OpenSubtitlesKey:      strings.TrimSpace(os.Getenv("OPENSUBTITLES_API_KEY")),
		OpenSubtitlesUser:     strings.TrimSpace(os.Getenv("OPENSUBTITLES_USERNAME")),
		OpenSubtitlesPassword: os.Getenv("OPENSUBTITLES_PASSWORD"),
		SubtitleLanguages:     splitCSV(envOrDefault("SUBTITLE_LANGUAGES", envOrDefault("METADATA_LANGUAGE", "en"))),
		SubtitleAutoFetch:     !strings.EqualFold(strings.TrimSpace(os.Getenv("SUBTITLE_AUTO_FETCH")), "false"),

		WatchFolders:  !strings.EqualFold(strings.TrimSpace(os.Getenv("WATCH_FOLDERS")), "false"),
		WatchDebounce: debounce,
		WatchRescan:   rescan,
	}, nil
}

func parseDuration(key, fallback string) (time.Duration, error) {
	value := envOrDefault(key, fallback)
	if value == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a duration such as 30s or 15m", key)
	}
	return d, nil
}

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (c Config) ListenAddress() string { return ":" + c.Port }

func parseLogLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, errors.New("LOG_LEVEL must be debug, info, warn, or error")
	}
}

func loadDotEnv(file string) error {
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf(".env line %d must be KEY=VALUE", lineNumber)
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			unquoted, err := strconv.Unquote(value)
			if err != nil {
				return fmt.Errorf(".env line %d: %w", lineNumber, err)
			}
			value = unquoted
		} else if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
			value = value[1 : len(value)-1]
		} else if comment := strings.Index(value, " #"); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf(".env line %d: set %s: %w", lineNumber, key, err)
		}
	}
	return scanner.Err()
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
