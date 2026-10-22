package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/hibiken/asynq"

	"github.com/flixermedia/flixer-indexer/internal/config"
)

// Pool runs one Asynq server per queue so each queue's concurrency can be
// tuned (and changed live) independently.
type Pool struct {
	redis    asynq.RedisConnOpt
	pipeline *Pipeline
	log      *slog.Logger

	mu      sync.Mutex
	servers map[string]*asynq.Server
	conc    map[string]int
}

func NewPool(redis asynq.RedisConnOpt, p *Pipeline, log *slog.Logger) *Pool {
	return &Pool{redis: redis, pipeline: p, log: log, servers: map[string]*asynq.Server{}, conc: map[string]int{}}
}

// Start launches a worker server for every queue.
func (p *Pool) Start(concurrency map[string]int) error {
	for _, queue := range config.Queues {
		if err := p.SetConcurrency(queue, concurrency[queue]); err != nil {
			return err
		}
	}
	return nil
}

// SetConcurrency starts or restarts a queue's server with n workers. Tasks
// running during a restart are given time to finish, then requeued.
func (p *Pool) SetConcurrency(queue string, n int) error {
	handlers, ok := p.pipeline.Handlers()[queue]
	if !ok {
		return fmt.Errorf("unknown queue %q", queue)
	}
	if n < 1 || n > config.MaxConcurrency {
		return fmt.Errorf("concurrency for %s must be 1-%d", queue, config.MaxConcurrency)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conc[queue] == n && p.servers[queue] != nil {
		return nil
	}
	if old := p.servers[queue]; old != nil {
		old.Shutdown()
		delete(p.servers, queue)
	}

	srv := asynq.NewServer(p.redis, asynq.Config{
		Concurrency:     n,
		Queues:          map[string]int{queue: 1},
		ShutdownTimeout: 30 * time.Second,
		ErrorHandler:    asynq.ErrorHandlerFunc(p.pipeline.OnError),
		Logger:          slogAdapter{p.log.With("component", "asynq", "queue", queue)},
		LogLevel:        asynq.WarnLevel,
		RetryDelayFunc:  asynq.DefaultRetryDelayFunc,
	})
	mux := asynq.NewServeMux()
	for typ, h := range handlers {
		mux.HandleFunc(typ, h)
	}
	if err := srv.Start(mux); err != nil {
		return fmt.Errorf("start %s workers: %w", queue, err)
	}
	p.servers[queue] = srv
	p.conc[queue] = n
	p.log.Info("workers running", "queue", queue, "concurrency", n)
	return nil
}

// Concurrency reports the current worker count per queue.
func (p *Pool) Concurrency() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.conc))
	for k, v := range p.conc {
		out[k] = v
	}
	return out
}

// Apply sets concurrency for every queue present in settings.
func (p *Pool) Apply(concurrency map[string]int) {
	queues := make([]string, 0, len(concurrency))
	for q := range concurrency {
		queues = append(queues, q)
	}
	sort.Strings(queues)
	for _, q := range queues {
		if err := p.SetConcurrency(q, concurrency[q]); err != nil {
			p.log.Warn("ignoring concurrency setting", "queue", q, "value", concurrency[q], "error", err)
		}
	}
}

// Shutdown stops all servers gracefully.
func (p *Pool) Shutdown(_ context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var wg sync.WaitGroup
	for q, s := range p.servers {
		wg.Add(1)
		go func() { defer wg.Done(); s.Shutdown() }()
		delete(p.servers, q)
	}
	wg.Wait()
}

type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Debug(args ...interface{}) { a.l.Debug(fmt.Sprint(args...)) }
func (a slogAdapter) Info(args ...interface{})  { a.l.Info(fmt.Sprint(args...)) }
func (a slogAdapter) Warn(args ...interface{})  { a.l.Warn(fmt.Sprint(args...)) }
func (a slogAdapter) Error(args ...interface{}) { a.l.Error(fmt.Sprint(args...)) }
func (a slogAdapter) Fatal(args ...interface{}) { a.l.Error(fmt.Sprint(args...)) }
