package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"

	"github.com/flixermedia/flixer-indexer/internal/api"
	"github.com/flixermedia/flixer-indexer/internal/bios"
	"github.com/flixermedia/flixer-indexer/internal/config"
	"github.com/flixermedia/flixer-indexer/internal/flixerclient"
	"github.com/flixermedia/flixer-indexer/internal/images"
	"github.com/flixermedia/flixer-indexer/internal/lyrics"
	"github.com/flixermedia/flixer-indexer/internal/models"
	"github.com/flixermedia/flixer-indexer/internal/monitoring"
	"github.com/flixermedia/flixer-indexer/internal/pipeline"
	"github.com/flixermedia/flixer-indexer/internal/plugins/runner"
	"github.com/flixermedia/flixer-indexer/internal/probe"
	"github.com/flixermedia/flixer-indexer/internal/progress"
	"github.com/flixermedia/flixer-indexer/internal/providers/registry"
	"github.com/flixermedia/flixer-indexer/internal/remoteconfig"
	"github.com/flixermedia/flixer-indexer/internal/settings"
	"github.com/flixermedia/flixer-indexer/internal/tasklog"
	"github.com/flixermedia/flixer-indexer/internal/themes"
	"github.com/flixermedia/flixer-indexer/internal/watcher"
)

func main() {
	if err := run(); err != nil {
		slog.Error("flixer-indexer stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var logOutput io.Writer = os.Stdout
	if cfg.LogDir != "" {
		if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
			return errors.New("create log directory: " + err.Error())
		}
		file, err := os.OpenFile(filepath.Join(cfg.LogDir, "indexer.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return errors.New("open log file: " + err.Error())
		}
		defer file.Close()
		logOutput = io.MultiWriter(os.Stdout, file)
	}
	log := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: cfg.LogLevel})).With("app", "flixer-indexer")
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// flixer decides whether Sentry runs; wait up to a minute for it, then start without.
	sentryEnabled := false
	flushSentry := func() {}
	if !cfg.Stub {
		sentryConfig := monitoring.Fetch(ctx, monitoring.FetchOptions{BaseURL: cfg.FlixerURL, App: "indexer", Logger: log})
		var sentryErr error
		sentryEnabled, flushSentry, sentryErr = monitoring.Start(sentryConfig, "")
		if sentryErr != nil {
			log.Warn("Sentry disabled", "error", sentryErr)
		}
	}
	if sentryEnabled {
		log = monitoring.WrapLogger(log)
		slog.SetDefault(log)
		log.Info("Sentry monitoring enabled")
	}
	defer flushSentry()

	pluginRunner, pluginRuntimeErr := runner.New(ctx)
	if pluginRuntimeErr != nil {
		log.Warn("WASI plugin runtime unavailable; indexer plugins are disabled", "error", pluginRuntimeErr)
	} else {
		defer func() {
			if err := pluginRunner.Close(context.Background()); err != nil {
				log.Warn("close WASI plugin runtime", "error", err)
			}
		}()
	}

	if !cfg.RedisFromEnv && !cfg.Stub {
		remote, err := remoteconfig.Wait(ctx, cfg.FlixerURL, cfg.Secret, log)
		if err != nil {
			return err
		}
		cfg.RedisAddr, cfg.RedisUser, cfg.RedisPass = remote.Redis.Addr, remote.Redis.Username, remote.Redis.Password
		cfg.RedisDB, cfg.RedisTLS = remote.Redis.DB, remote.Redis.TLS
	}
	var tlsConfig *tls.Config
	if cfg.RedisTLS {
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Username: cfg.RedisUser, Password: cfg.RedisPass, DB: cfg.RedisDB, TLSConfig: tlsConfig})
	defer rdb.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = rdb.Ping(pingCtx).Err()
	cancel()
	if err != nil {
		return errors.New("cannot reach redis at " + cfg.RedisAddr + ": " + err.Error())
	}

	var client flixerclient.API
	if cfg.Stub {
		log.Warn("running with the stub flixer client", "libraries", cfg.StubLibs, "output", cfg.StubOutput)
		client = flixerclient.NewStub(cfg.StubLibs, cfg.StubOutput, models.Settings{Concurrency: cfg.Concurrency})
	} else {
		client = flixerclient.New(cfg.FlixerURL, cfg.Secret)
	}

	redisOpt := asynq.RedisClientOpt{Addr: cfg.RedisAddr, Username: cfg.RedisUser, Password: cfg.RedisPass, DB: cfg.RedisDB, TLSConfig: tlsConfig}
	queue := asynq.NewClient(redisOpt)
	defer queue.Close()
	inspector := asynq.NewInspector(redisOpt)
	defer inspector.Close()

	tracker := progress.New(rdb)
	store := images.NewStore(cfg.CachePath, cfg.FFmpegPath, cfg.UserAgent)
	pipe := pipeline.New(client, store, probe.New(cfg.FFprobePath), tracker, queue, log)
	pipe.SubtitleDir = cfg.SubtitleDir
	pipe.TaskLogs = tasklog.New(rdb)
	pipe.Themes = themes.NewFetcher(cfg.CachePath, cfg.FFmpegPath, cfg.UserAgent)
	pipe.SetTrickplay(cfg.TrickplayEnabled, cfg.TrickplayDir, cfg.FFmpegPath, cfg.TrickplayInterval, cfg.TrickplayWidth)

	pipe.LyricsDir = cfg.LyricsDir
	if cfg.LyricsFetch {
		pipe.Lyrics = lyrics.NewFetcher(cfg.UserAgent)
	}
	envSubs := models.SubtitleSettings{
		APIKey: cfg.OpenSubtitlesKey, Username: cfg.OpenSubtitlesUser, Password: cfg.OpenSubtitlesPassword,
		Languages: cfg.SubtitleLanguages, AutoFetch: cfg.SubtitleAutoFetch,
	}
	pipe.SetSubtitles(envSubs, cfg.UserAgent)

	var pool *pipeline.Pool
	if cfg.Role != config.RoleAPI {
		pool = pipeline.NewPool(redisOpt, pipe, log)
	}
	reloader := &settings.Reloader{
		Client: client, Pipeline: pipe, Pool: pool, EnvKeys: cfg.ProviderKeys, EnvSubtitles: envSubs,
		UA: cfg.UserAgent, Language: cfg.Language, Interval: time.Minute, Log: log,
		PluginsPath: cfg.PluginsPath, PluginRunner: pluginRunner,
	}
	// Start with the environment's settings so work can run even when flixer is down.
	if err := reloader.Reload(ctx); err != nil {
		log.Warn("could not load settings from flixer; using defaults", "error", err)
		pipe.SetRegistry(registry.Build(models.Settings{}, cfg.ProviderKeys, cfg.UserAgent, cfg.Language))
	}
	if pool != nil {
		if err := pool.Start(cfg.Concurrency); err != nil {
			return err
		}
		defer pool.Shutdown(context.Background())
	}
	monitoring.Go(func() { reloader.Run(ctx) })
	if cfg.WatchFolders && cfg.Role != config.RoleWorker {
		monitoring.Go(func() {
			w := &watcher.Watcher{Scanner: pipe, Log: log, Debounce: cfg.WatchDebounce, Rescan: cfg.WatchRescan}
			if err := w.Run(ctx); err != nil {
				log.Error("folder watcher stopped", "error", err)
			}
		})
	}

	handler := (&api.Server{
		Secret: cfg.Secret, Pipeline: pipe, Inspector: inspector, Progress: tracker, Redis: rdb,
		Pool: pool, Reloader: reloader, Log: log, CachePath: cfg.CachePath, Bios: bios.New(cfg.UserAgent),
	}).Handler()
	if sentryEnabled {
		handler = monitoring.Middleware(handler)
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddress(),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errs := make(chan error, 1)
	monitoring.Go(func() {
		log.Info("flixer-indexer listening", "address", srv.Addr, "role", cfg.Role, "flixer", cfg.FlixerURL, "stub", cfg.Stub)
		errs <- srv.ListenAndServe()
	})

	select {
	case err := <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
