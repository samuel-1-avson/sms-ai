// Command api serves the provider webhooks, admin API and metrics. With
// EMBEDDED_WORKER=true it also runs the job worker in-process (handy for a
// single-container pilot or local development).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"smsaighana/internal/app"
	"smsaighana/internal/config"
	"smsaighana/internal/httpapi"
	"smsaighana/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "api:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.Build(ctx, cfg)
	if err != nil {
		return err
	}
	defer a.Close()

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewHandler(a),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		// Must exceed the USSD fast-path deadline plus database work.
		WriteTimeout: cfg.USSDFastPathTimeout + 10*time.Second,
		IdleTimeout:  60 * time.Second,
	}

	workerDone := make(chan struct{})
	if cfg.EmbeddedWorker {
		go func() {
			worker.New(a.Engine, a.Store, cfg, a.Log).Run(ctx)
			close(workerDone)
		}()
	} else {
		close(workerDone)
	}

	errc := make(chan error, 1)
	go func() {
		a.Log.Info("api listening", "addr", cfg.HTTPAddr, "env", cfg.Env, "embedded_worker", cfg.EmbeddedWorker)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	select {
	case err := <-errc:
		if err != nil {
			return err
		}
	case <-ctx.Done():
	}
	a.Log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	<-workerDone
	return err
}
