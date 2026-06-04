// Command kmail-worker is the background-worker entrypoint for the
// KMail BFF (Session 6 — service decomposition).
//
// It runs every background worker that cmd/kmail-api historically
// started in-process (calendar reminders, undo/scheduled/snooze
// dispatch, billing quota scan, search auto-cutover, deliverability
// alert evaluation, shard-health probing, retention enforcement,
// export fan-out, admin-proxy grant expiry, and webhook delivery),
// sharing the same config, Postgres pool, Valkey client, and
// service constructors as the API. The API now defaults to
// KMAIL_DISABLE_WORKERS=true and serves HTTP only, so the workers
// scale and fail independently of request-serving capacity.
//
// The process exposes ONLY operational endpoints — /healthz,
// /readyz, and /metrics — on KMAIL_WORKER_ADDR (default :8090); it
// serves no tenant traffic. On SIGINT/SIGTERM it cancels the worker
// context and drains for up to 30s before shutting down.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/valkeyurl"
)

// workerDrainTimeout bounds the graceful shutdown: on a termination
// signal the worker context is cancelled and the supervisor is given
// this long to let every worker's Run loop return before the process
// tears down its HTTP server and connection pools.
const workerDrainTimeout = 30 * time.Second

func main() {
	logger := log.New(os.Stderr, "kmail-worker ", log.LstdFlags|log.Lmicroseconds|log.LUTC)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config.Load: %v", err)
	}
	logger.Printf("starting with %s", cfg)

	// startupCtx bounds one-shot construction-time probes (e.g. the
	// Valkey ping that selects the circuit-breaker impl, and the
	// pool's initial connection). It is cancelled explicitly as soon
	// as construction finishes (see startupCancel() after
	// buildWorkers) — the long-lived workers run under the separate
	// workerCtx below, and pgxpool only uses this context for the
	// initial connect, not the pool's lifetime. The deferred cancel
	// is an idempotent safety net for the early-return (Fatalf) paths.
	startupCtx, startupCancel := context.WithCancel(context.Background())
	defer startupCancel()

	pool, err := pgxpool.New(startupCtx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	// Valkey client. cfg.ValkeyURL accepts both a redis:// DSN and a
	// bare host:port; normalise via valkeyurl.Parse exactly like
	// cmd/kmail-api. A non-empty default means this is virtually
	// always live; an explicitly-empty value disables undo-send and
	// the shared circuit breaker.
	var valkeyClient *redis.Client
	if cfg.ValkeyURL != "" {
		opts, parseErr := valkeyurl.Parse(cfg.ValkeyURL)
		if parseErr != nil {
			logger.Fatalf("valkey url %q: %v", cfg.ValkeyURL, parseErr)
		}
		valkeyClient = redis.NewClient(opts)
		defer func() {
			if cerr := valkeyClient.Close(); cerr != nil {
				logger.Printf("valkey: close: %v", cerr)
			}
		}()
	}

	// Dedicated Prometheus registry: Go runtime + process collectors
	// (valuable for a long-lived worker) plus the per-worker
	// supervision metrics, and whatever the worker constructors
	// register (retention, admin-proxy expiry).
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	wm := newWorkerMetrics(reg)

	regs, err := buildWorkers(startupCtx, workerDeps{
		cfg:    cfg,
		pool:   pool,
		valkey: valkeyClient,
		reg:    reg,
		logger: logger,
	})
	if err != nil {
		logger.Fatalf("buildWorkers: %v", err)
	}
	wm.registered.Set(float64(len(regs)))
	logger.Printf("kmail-worker: %d background workers registered", len(regs))

	// Construction is done: release the startup context now (rather
	// than waiting for the deferred cancel at process exit) so no
	// construction-time probe lingers while the workers run.
	startupCancel()

	// workerCtx drives the worker lifecycle. It is cancelled first on
	// shutdown so workers drain before the HTTP server and pools go.
	workerCtx, workerCancel := context.WithCancel(context.Background())
	sup := newSupervisor(wm, logger)
	for _, r := range regs {
		sup.start(workerCtx, r)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("GET /readyz", readyzHandler(pool))
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))

	addr := getenvString("KMAIL_WORKER_ADDR", ":8090")
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	serverErr := make(chan error, 1)
	go func() {
		logger.Printf("worker health/metrics listening on %s", addr)
		if serveErr := srv.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			serverErr <- serveErr
			return
		}
		serverErr <- nil
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case serveErr := <-serverErr:
		// ListenAndServe failed before any signal (e.g. addr in use).
		if serveErr != nil {
			logger.Fatalf("worker http server: %v", serveErr)
		}
	case sig := <-sigCh:
		logger.Printf("received %s; draining workers (up to %s)", sig, workerDrainTimeout)
	}

	// 1. Stop the workers, bounded by the drain budget.
	workerCancel()
	if sup.wait(workerDrainTimeout) {
		logger.Printf("all workers drained cleanly")
	} else {
		logger.Printf("worker drain timed out after %s; proceeding with shutdown", workerDrainTimeout)
	}

	// 2. Shut down the HTTP server.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if shErr := srv.Shutdown(shutdownCtx); shErr != nil {
		logger.Printf("worker http server shutdown: %v", shErr)
	}

	logger.Printf("kmail-worker stopped")
}

// healthzHandler is a liveness probe: the process is up and serving.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// readyzHandler is a readiness probe gated on control-plane Postgres
// reachability — the workers cannot make progress without it.
func readyzHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("postgres unreachable\n"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	}
}

// getenvString reads a string env var with a fallback.
func getenvString(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
