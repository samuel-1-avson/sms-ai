// Package worker runs the job loop: claim (FOR UPDATE SKIP LOCKED), process,
// repeat. Workers wake on Postgres LISTEN/NOTIFY for immediate jobs and poll
// for debounced ones; maintenance runs once a minute.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"sync"
	"time"

	"smsaighana/internal/config"
	"smsaighana/internal/repository"
	"smsaighana/internal/service"
)

type Worker struct {
	engine *service.Engine
	store  *repository.Store
	cfg    *config.Config
	log    *slog.Logger
	id     string
	wake   chan struct{}
}

func New(engine *service.Engine, store *repository.Store, cfg *config.Config, log *slog.Logger) *Worker {
	host, _ := os.Hostname()
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return &Worker{
		engine: engine, store: store, cfg: cfg, log: log,
		id:   host + "-" + hex.EncodeToString(b),
		wake: make(chan struct{}, cfg.WorkerConcurrency),
	}
}

// Run blocks until ctx is cancelled and in-flight jobs have finished.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("worker started", "id", w.id, "concurrency", w.cfg.WorkerConcurrency)
	var wg sync.WaitGroup
	go w.listen(ctx)
	go w.maintenance(ctx)
	for i := 0; i < w.cfg.WorkerConcurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Wait()
	w.log.Info("worker stopped", "id", w.id)
}

func (w *Worker) loop(ctx context.Context) {
	for ctx.Err() == nil {
		job, err := w.store.Q().ClaimJob(ctx, w.id, w.cfg.JobLease)
		if err != nil {
			if ctx.Err() == nil {
				w.log.Error("claiming job", "err", err)
			}
			w.sleep(ctx)
			continue
		}
		if job == nil {
			w.sleep(ctx)
			continue
		}
		// Jobs finish even during shutdown, bounded by their lease.
		jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.JobLease-5*time.Second)
		if err := w.engine.ProcessJob(jctx, job); err != nil {
			w.log.Error("job failed", "job", job.ID, "kind", job.Kind, "attempt", job.Attempts, "err", err)
			if rerr := w.engine.RescheduleAfterError(jctx, job, err); rerr != nil {
				w.log.Error("rescheduling job", "job", job.ID, "err", rerr)
			}
		}
		cancel()
	}
}

func (w *Worker) sleep(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-w.wake:
	case <-time.After(w.cfg.WorkerPoll):
	}
}

func (w *Worker) broadcast() {
	for i := 0; i < cap(w.wake); i++ {
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
}

// listen holds a dedicated connection on LISTEN smsai_jobs.
func (w *Worker) listen(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := w.store.Pool.Acquire(ctx)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+repository.NotifyChannel()); err != nil {
			conn.Release()
			time.Sleep(time.Second)
			continue
		}
		for ctx.Err() == nil {
			if _, err := conn.Conn().WaitForNotification(ctx); err != nil {
				break
			}
			w.broadcast()
		}
		// The connection may be mid-wait; discard it rather than reuse it.
		_ = conn.Conn().Close(context.Background())
		conn.Release()
	}
}

func (w *Worker) maintenance(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	w.engine.Maintenance(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.engine.Maintenance(ctx)
		}
	}
}
