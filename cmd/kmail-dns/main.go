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
	"encoding/json"
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
	"github.com/kennguy3n/kmail/internal/tenant"
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
	// tenantSvc is used for the one RLS-scoped domain lookup the
	// records handler needs; it reuses the same RLS-wrapped query
	// the in-process kmail-api path uses, so the standalone binary
	// has identical tenant-isolation guarantees.
	tenantSvc := tenant.NewService(pool)

	authMW := middleware.NewOIDC(middleware.OIDCConfig{
		Issuer:         cfg.KChatOIDCIssuer,
		DevBypassToken: cfg.DevBypassToken,
		Pool:           pool,
		Logger:         logger,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("POST /api/v1/tenants/{id}/domains/{domainId}/verify",
		authMW.Wrap(http.HandlerFunc(verifyDomainHandler(dnsSvc, logger))))
	mux.Handle("GET /api/v1/tenants/{id}/domains/{domainId}/records",
		authMW.Wrap(http.HandlerFunc(getDomainRecordsHandler(dnsSvc, tenantSvc, logger))))

	srv := &http.Server{
		Addr:              cfg.DNS.Addr,
		Handler:           middleware.RequestLogger(logger)(mux),
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

func verifyDomainHandler(svc *dns.Service, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.PathValue("id")
		domainID := r.PathValue("domainId")
		if err := checkTenantScope(r, tenantID); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		result, err := svc.VerifyDomain(r.Context(), tenantID, domainID)
		if err != nil {
			logger.Printf("verifyDomain: %v", err)
			writeJSON(w, statusForDNSError(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func getDomainRecordsHandler(svc *dns.Service, tenantSvc *tenant.Service, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.PathValue("id")
		domainID := r.PathValue("domainId")
		if err := checkTenantScope(r, tenantID); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
			return
		}
		d, err := tenantSvc.GetDomain(r.Context(), tenantID, domainID)
		if err != nil {
			logger.Printf("getDomainRecords: %v", err)
			writeJSON(w, statusForTenantError(err), map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, svc.GenerateRecords(d.Domain))
	}
}

// checkTenantScope enforces that the authenticated tenant (read
// from the request context by the OIDC middleware) matches the
// tenant ID in the URL path. Mirrors the application-level check
// the in-process handlers in `internal/tenant/handlers.go` do
// before every tenant-scoped mutation — the RLS policy would
// already block a cross-tenant read, but failing at the handler
// gives a friendlier 403 than a Postgres error and prevents an
// authenticated caller in tenant A from even touching tenant B's
// verification state.
func checkTenantScope(r *http.Request, pathTenantID string) error {
	ctxTenantID := middleware.TenantIDFrom(r.Context())
	if ctxTenantID == "" {
		return errors.New("missing tenant context")
	}
	if ctxTenantID != pathTenantID {
		return errors.New("cross-tenant access forbidden")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func statusForDNSError(err error) int {
	switch {
	case errors.Is(err, dns.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, dns.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func statusForTenantError(err error) int {
	switch {
	case errors.Is(err, tenant.ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, tenant.ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
