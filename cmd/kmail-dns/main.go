// Command kmail-dns is the DNS Onboarding Service entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7 and
// docs/PROPOSAL.md §9.3): drive the DNS wizard — MX / SPF / DKIM /
// DMARC / MTA-STS / TLS-RPT / autoconfig discovery and
// verification.
//
// The same DNS service is also mounted in-process by `cmd/kmail-api`
// under `/api/v1/tenants/{id}/domains/{domainId}/...`; this binary
// exposes a standalone HTTP surface for deployments that want to
// run the DNS Onboarding Service on its own port and scale it
// independently.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/dns"
	"github.com/kennguy3n/kmail/internal/middleware"
)

func main() {
	logger := log.New(os.Stderr, "kmail-dns ", log.LstdFlags|log.Lmicroseconds|log.LUTC)

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config.Load: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	dnsSvc := dns.NewService(dns.Config{
		Pool:                pool,
		MailHost:            cfg.DNS.MailHost,
		SPFInclude:          cfg.DNS.SPFInclude,
		DefaultDKIMSelector: cfg.DNS.DKIMSelector,
		DKIMPublicKey:       cfg.DNS.DKIMPublicKey,
		DMARCPolicy:         cfg.DNS.DMARCPolicy,
		ReportingMailbox:    cfg.DNS.ReportingMailbox,
	})

	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		Issuer:         cfg.KChatOIDCIssuer,
		Audience:       cfg.KChatOIDCAudience,
		DevBypassToken: cfg.DevBypassToken,
		Pool:           pool,
		Logger:         logger,
	})
	if err != nil {
		logger.Fatalf("middleware.NewOIDC: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	// Register the same Handlers set the in-process path in
	// cmd/kmail-api uses so the two deployments expose an
	// identical URL surface and tenant-isolation posture.
	dns.NewHandlers(dnsSvc, logger).Register(mux, authMW)

	srv := &http.Server{
		Addr:              cfg.DNS.Addr,
		Handler:           middleware.RequestLogger(logger, "")(mux),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s", cfg.DNS.Addr)
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
	<-serverErr
	logger.Printf("kmail-dns stopped")
}
