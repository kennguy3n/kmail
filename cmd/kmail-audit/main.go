// Command kmail-audit is the Audit / Compliance CLI + HTTP entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7): tamper-evident
// audit log consumer, export tooling, eDiscovery preparation, and
// retention policy enforcement. Backed by the audit_log table
// defined in docs/SCHEMA.md §5.8.
//
// Usage:
//
//	kmail-audit serve                      — run the HTTP API
//	kmail-audit verify <tenant_id>         — walk the tenant chain
//	kmail-audit export <tenant_id> [fmt]   — dump entries to stdout
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/middleware"
)

func main() {
	logger := log.New(os.Stderr, "kmail-audit ", log.LstdFlags|log.Lmicroseconds|log.LUTC)
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: kmail-audit [serve|verify|export]")
		os.Exit(2)
	}
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
	svc := audit.NewService(pool)

	switch os.Args[1] {
	case "serve":
		runServe(ctx, logger, cfg, pool, svc)
	case "verify":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: kmail-audit verify <tenant_id>")
			os.Exit(2)
		}
		if err := svc.VerifyChain(ctx, os.Args[2]); err != nil {
			logger.Fatalf("VerifyChain: %v", err)
		}
		fmt.Println("chain OK")
	case "export":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: kmail-audit export <tenant_id> [json|csv]")
			os.Exit(2)
		}
		format := "json"
		if len(os.Args) >= 4 {
			format = os.Args[3]
		}
		data, err := svc.Export(ctx, os.Args[2], format, time.Time{}, time.Time{})
		if err != nil {
			logger.Fatalf("Export: %v", err)
		}
		_, _ = os.Stdout.Write(data)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

func runServe(ctx context.Context, logger *log.Logger, cfg *config.Config, pool *pgxpool.Pool, svc *audit.Service) {
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
	audit.NewHandlers(svc, logger).Register(mux, authMW)

	srv := &http.Server{
		Addr:              cfg.Audit.Addr,
		Handler:           middleware.RequestLogger(logger, "")(mux),
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		logger.Printf("listening on %s", cfg.Audit.Addr)
		_ = srv.ListenAndServe()
	}()
	<-sigCh
	shutdownCtx, shutdownCancel := context.WithTimeout(ctx, cfg.HTTP.ShutdownTimeout)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}
