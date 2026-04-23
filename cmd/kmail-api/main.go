// Command kmail-api is the API Gateway / BFF entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7 and
// docs/JMAP-CONTRACT.md): translate KChat OIDC auth into Stalwart
// auth, proxy JMAP between the React client and Stalwart, enforce
// tenant policy and rate limits, and fan JMAP push events into
// KChat notifications via the Chat Bridge.
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

	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/dns"
	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/tenant"
)

func main() {
	logger := log.New(os.Stderr, "kmail-api ", log.LstdFlags|log.Lmicroseconds|log.LUTC)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config.Load: %v", err)
	}
	logger.Printf("starting with %s", cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("GET /readyz", readyzHandler(pool))

	authMW := middleware.NewOIDC(middleware.OIDCConfig{
		Issuer:         cfg.KChatOIDCIssuer,
		DevBypassToken: cfg.DevBypassToken,
		Pool:           pool,
		Logger:         logger,
	})

	proxy, err := jmap.NewProxy(jmap.ProxyConfig{
		StalwartURL: cfg.StalwartURL,
		Pool:        pool,
		Logger:      logger,
	})
	if err != nil {
		logger.Fatalf("jmap.NewProxy: %v", err)
	}
	// Everything under /jmap is authenticated and forwarded to
	// Stalwart. The trailing-slash pattern owns every path below
	// /jmap/ so subpaths like /jmap/session and /jmap/upload route
	// here, while the bare /jmap lands on the session endpoint.
	mux.Handle("/jmap", authMW.Wrap(proxy))
	mux.Handle("/jmap/", authMW.Wrap(proxy))

	tenantSvc := tenant.NewService(pool)
	dnsSvc := dns.NewService(dns.Config{
		Pool:                pool,
		MailHost:            cfg.DNS.MailHost,
		SPFInclude:          cfg.DNS.SPFInclude,
		DefaultDKIMSelector: cfg.DNS.DKIMSelector,
		DKIMPublicKey:       cfg.DNS.DKIMPublicKey,
		DMARCPolicy:         cfg.DNS.DMARCPolicy,
		ReportingMailbox:    cfg.DNS.ReportingMailbox,
	})
	tenantHandlers := tenant.NewHandlers(tenantSvc, logger)
	tenantHandlers.Register(mux, authMW)
	dnsHandlers := dns.NewHandlers(dnsSvc, logger)
	dnsHandlers.Register(mux, authMW)

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           middleware.RequestLogger(logger)(mux),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s", cfg.HTTP.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-serverErr:
		if err != nil {
			logger.Fatalf("http server: %v", err)
		}
	case sig := <-sigCh:
		logger.Printf("received %s, starting graceful shutdown", sig)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("graceful shutdown error: %v", err)
	}

	// Drain ListenAndServe's return so deferred cleanups run in a
	// predictable order.
	<-serverErr
	logger.Printf("kmail-api stopped")
}

// healthzHandler is a liveness probe. It returns 200 OK as long as
// the process is running and able to serve HTTP. It does not check
// downstream dependencies.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// readyzHandler is a readiness probe. It returns 200 OK only if the
// BFF can talk to its control-plane Postgres. Kubernetes (or the
// compose healthcheck) uses this to gate traffic.
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
