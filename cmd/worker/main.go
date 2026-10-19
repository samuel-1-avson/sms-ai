// Command worker claims and processes jobs: debounced answer generation,
// outbound SMS dispatch, reconciliation and retention housekeeping.
//
// With WORKER_METRICS_ADDR set (e.g. ":9091") it also serves GET /metrics.
// Job, dispatch, AI and spend metrics are counted in this process, so the
// API's /metrics cannot show them when the worker runs separately.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"smsaighana/internal/app"
	"smsaighana/internal/config"
	"smsaighana/internal/security"
	"smsaighana/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.Build(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "worker:", err)
		os.Exit(1)
	}
	defer a.Close()
	if addr := strings.TrimSpace(os.Getenv("WORKER_METRICS_ADDR")); addr != "" {
		go serveMetrics(ctx, a, addr)
	}
	worker.New(a.Engine, a.Store, cfg, a.Log).Run(ctx)
}

// serveMetrics exposes this process's registry, guarded by METRICS_TOKEN
// like the API's /metrics.
func serveMetrics(ctx context.Context, a *app.App, addr string) {
	h := promhttp.HandlerFor(a.Metrics.Registry, promhttp.HandlerOpts{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		token := a.Cfg.MetricsToken
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token != "" && !security.ConstantTimeEqual(strings.TrimSpace(got), token) {
			http.Error(w, "metrics token required", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	a.Log.Info("worker metrics listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.Log.Error("worker metrics server stopped", "err", err)
	}
}
