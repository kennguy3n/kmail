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
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/billing"
	"github.com/kennguy3n/kmail/internal/calendarbridge"
	"github.com/kennguy3n/kmail/internal/chatbridge"
	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/deliverability"
	"github.com/kennguy3n/kmail/internal/dns"
	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/migration"
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

	// Valkey-backed rate limiter. Enabled via config; when
	// disabled the limiter is a no-op and the middleware passes
	// the request through untouched. Plumbed in between the OIDC
	// gate (needs identity) and the JMAP + tenant handlers.
	var rateLimiter *middleware.RateLimiter
	if cfg.RateLimit.Enabled {
		store, err := middleware.NewRedisStore(cfg.ValkeyURL)
		if err != nil {
			logger.Fatalf("middleware.NewRedisStore: %v", err)
		}
		rateLimiter = middleware.NewRateLimiter(middleware.RateLimiterConfig{
			Client:    store,
			TenantRPM: cfg.RateLimit.TenantRPM,
			UserRPM:   cfg.RateLimit.UserRPM,
			Window:    cfg.RateLimit.Window,
			Logger:    logger,
		})
	}
	wrapAuthRL := func(h http.Handler) http.Handler {
		wrapped := authMW.Wrap(h)
		if rateLimiter != nil {
			// Rate limit AFTER auth so tenant / user IDs are in
			// context when the limiter consults Valkey.
			wrapped = authMW.Wrap(rateLimiter.Wrap(h))
		}
		return wrapped
	}

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
	mux.Handle("/jmap", wrapAuthRL(proxy))
	mux.Handle("/jmap/", wrapAuthRL(proxy))

	// Billing / Quota Service — constructed early so the Tenant
	// Service can consume it as a SeatAccounter for CreateUser /
	// DeleteUser seat counter updates.
	billingSvc := billing.NewService(billing.Config{
		Pool:                pool,
		CoreSeatCents:       cfg.Billing.CoreSeatCents,
		ProSeatCents:        cfg.Billing.ProSeatCents,
		PrivacySeatCents:    cfg.Billing.PrivacySeatCents,
		CorePerSeatBytes:    cfg.Billing.CorePerSeatBytes,
		ProPerSeatBytes:     cfg.Billing.ProPerSeatBytes,
		PrivacyPerSeatBytes: cfg.Billing.PrivacyPerSeatBytes,
	})
	billing.NewHandlers(billingSvc, logger).Register(mux, authMW)

	tenantSvc := tenant.NewService(pool).WithSeatAccounter(billingSvc)
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

	migrationSvc := migration.NewService(migration.Config{
		Pool:             pool,
		StalwartAdminURL: cfg.StalwartURL,
		ImapsyncBin:      os.Getenv("KMAIL_IMAPSYNC_BIN"),
		MaxConcurrent:    config.GetenvInt("KMAIL_MIGRATION_MAX_CONCURRENT", 4),
	})
	migrationHandlers := migration.NewHandlers(migrationSvc, logger)
	migrationHandlers.Register(mux, authMW)

	calendarSvc := calendarbridge.NewService(calendarbridge.Config{
		StalwartURL: cfg.StalwartURL,
	})
	calendarbridge.NewHandlers(calendarSvc, logger).Register(mux, authMW)

	chatbridgeSvc := chatbridge.NewService(chatbridge.Config{
		KChatAPIURL:   cfg.KChatAPIURL,
		KChatAPIToken: cfg.KChatAPIToken,
		StalwartURL:   cfg.StalwartURL,
		Pool:          pool,
		Logger:        logger,
	})
	chatbridge.NewHandlers(chatbridgeSvc, logger).Register(mux, authMW)

	auditSvc := audit.NewService(pool)
	audit.NewHandlers(auditSvc, logger).Register(mux, authMW)

	// Deliverability Control Plane (suppression, bounces, IP
	// pools, send limits, warmup, DMARC).
	var valkeyClient *redis.Client
	if cfg.ValkeyURL != "" {
		valkeyClient = redis.NewClient(&redis.Options{Addr: cfg.ValkeyURL})
	}
	deliverabilitySvc := deliverability.NewService(deliverability.Config{
		Pool:                      pool,
		Valkey:                    valkeyClient,
		Logger:                    logger,
		CoreDailyLimit:            cfg.Deliverability.CoreDailyLimit,
		ProDailyLimit:             cfg.Deliverability.ProDailyLimit,
		PrivacyDailyLimit:         cfg.Deliverability.PrivacyDailyLimit,
		WarmupDays:                cfg.Deliverability.WarmupDays,
		BounceSoftEscalationCount: cfg.Deliverability.BounceSoftEscalationCount,
		BounceSoftWindow:          cfg.Deliverability.BounceSoftWindow,
	})
	deliverability.NewHandlers(deliverabilitySvc, logger).Register(mux, authMW)

	// Attachment-to-link conversion.
	attachmentSvc := jmap.NewAttachmentService(jmap.AttachmentConfig{
		Pool:      pool,
		S3URL:     cfg.ZKFabric.S3URL,
		AccessKey: cfg.ZKFabric.AccessKey,
		SecretKey: cfg.ZKFabric.SecretKey,
		Bucket:    cfg.Attachments.BucketName,
		Threshold: cfg.Attachments.ThresholdBytes,
		Expiry:    cfg.Attachments.DefaultExpiry,
		Logger:    logger,
	})
	jmap.NewAttachmentHandlers(attachmentSvc, logger).Register(mux, authMW)

	// Observability: Prometheus /metrics + OpenTelemetry tracing.
	metrics := middleware.NewMetrics()
	if cfg.Observability.MetricsEnabled {
		mux.Handle("GET /metrics", metrics.Handler())
	}
	tracingShutdown := func(context.Context) error { return nil }
	if cfg.Observability.TracingEnabled {
		sh, err := middleware.InitTracing(ctx, "kmail-api", cfg.Observability.OTLPEndpoint)
		if err != nil {
			logger.Printf("tracing init: %v", err)
		} else {
			tracingShutdown = sh
		}
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracingShutdown(shutdownCtx)
	}()

	// Background quota worker polls zk-object-fabric every
	// `QuotaWorkerInterval` and reconciles actual tenant storage
	// usage with the `quotas.storage_used_bytes` snapshot.
	if cfg.Billing.QuotaWorkerEnabled {
		worker := billing.NewQuotaWorker(billing.QuotaWorkerConfig{
			Pool:     pool,
			Billing:  billingSvc,
			Scanner:  billing.StaticScanner{Bytes: -1},
			Interval: cfg.Billing.QuotaWorkerInterval,
			Logger:   logger,
		})
		go worker.Run(ctx)
	}

	// Wire metrics and tracing into the outer handler chain.
	handler := http.Handler(mux)
	if cfg.Observability.TracingEnabled {
		handler = middleware.TracingMiddleware(handler)
	}
	if cfg.Observability.MetricsEnabled {
		handler = metrics.Middleware(handler)
	}

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           middleware.RequestLogger(logger, cfg.Observability.LogFormat)(handler),
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
