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

	"github.com/kennguy3n/kmail/internal/adminproxy"
	"github.com/kennguy3n/kmail/internal/approval"
	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/billing"
	"github.com/kennguy3n/kmail/internal/calendarbridge"
	"github.com/kennguy3n/kmail/internal/contactbridge"
	"github.com/kennguy3n/kmail/internal/chatbridge"
	"github.com/kennguy3n/kmail/internal/cmk"
	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/confidentialsend"
	"github.com/kennguy3n/kmail/internal/deliverability"
	"github.com/kennguy3n/kmail/internal/dns"
	"github.com/kennguy3n/kmail/internal/export"
	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/migration"
	"github.com/kennguy3n/kmail/internal/monitoring"
	"github.com/kennguy3n/kmail/internal/onboarding"
	"github.com/kennguy3n/kmail/internal/push"
	"github.com/kennguy3n/kmail/internal/retention"
	"github.com/kennguy3n/kmail/internal/scim"
	"github.com/kennguy3n/kmail/internal/search"
	"github.com/kennguy3n/kmail/internal/sieve"
	"github.com/kennguy3n/kmail/internal/sharedinbox"
	"github.com/kennguy3n/kmail/internal/tenant"
	"github.com/kennguy3n/kmail/internal/vault"
	"github.com/kennguy3n/kmail/internal/webhooks"
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

	// Multi-tenant Stalwart shard routing — constructed early so
	// the JMAP proxy can resolve per-tenant primary + secondary
	// Stalwart URLs on every request.
	shardSvc := tenant.NewShardService(pool, logger)
	proxy, err := jmap.NewProxy(jmap.ProxyConfig{
		StalwartURL: cfg.StalwartURL,
		Pool:        pool,
		Logger:      logger,
		Shards:      shardSvc,
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
	billingLifecycleEarly := billing.NewLifecycle(billingSvc, logger)
	billing.NewHandlers(billingSvc, logger).WithLifecycle(billingLifecycleEarly).Register(mux, authMW)
	billing.NewWebhookHandler(billing.WebhookConfig{
		Lifecycle:           billingLifecycleEarly,
		StripeWebhookSecret: os.Getenv("KMAIL_STRIPE_WEBHOOK_SECRET"),
		Logger:              logger,
	}).Register(mux)

	// Per-tenant zk-object-fabric provisioning. CreateTenant calls
	// Provision after the DB insert so every new tenant gets its
	// own bucket + API key + placement policy without an operator
	// running a separate one-shot.
	zkProvisioner := tenant.NewZKFabricProvisioner(tenant.ZKFabricProvisioner{
		Pool:           pool,
		S3URL:          cfg.ZKFabric.S3URL,
		ConsoleURL:     cfg.ZKFabric.ConsoleURL,
		AdminAccessKey: cfg.ZKFabric.AccessKey,
		AdminSecretKey: cfg.ZKFabric.SecretKey,
		Logger:         logger,
	})
	tenantSvc := tenant.NewService(pool).
		WithSeatAccounter(billingSvc).
		WithStorageProvisioner(zkProvisioner).
		WithBillingLifecycle(billingLifecycleEarly)
	dnsSvc := dns.NewService(dns.Config{
		Pool:                pool,
		MailHost:            cfg.DNS.MailHost,
		SPFInclude:          cfg.DNS.SPFInclude,
		DefaultDKIMSelector: cfg.DNS.DKIMSelector,
		DKIMPublicKey:       cfg.DNS.DKIMPublicKey,
		DMARCPolicy:         cfg.DNS.DMARCPolicy,
		ReportingMailbox:    cfg.DNS.ReportingMailbox,
		BIMILogoURL:         cfg.DNS.BIMILogoURL,
		BIMIVMCURL:          cfg.DNS.BIMIVMCURL,
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

	// Valkey is consumed by deliverability, push, calendar reminders,
	// and the SLO tracker. Stand it up early so every downstream
	// service can share the same client.
	var valkeyClient *redis.Client
	if cfg.ValkeyURL != "" {
		valkeyClient = redis.NewClient(&redis.Options{Addr: cfg.ValkeyURL})
	}

	chatbridgeSvc := chatbridge.NewService(chatbridge.Config{
		KChatAPIURL:   cfg.KChatAPIURL,
		KChatAPIToken: cfg.KChatAPIToken,
		StalwartURL:   cfg.StalwartURL,
		Pool:          pool,
		Logger:        logger,
	})
	chatbridge.NewHandlers(chatbridgeSvc, logger).Register(mux, authMW)

	calendarSvc := calendarbridge.NewService(calendarbridge.Config{
		StalwartURL: cfg.StalwartURL,
	})
	// Per-tenant scheduling notifications. Phase 4 routes every
	// tenant to a single configured channel
	// (`KMAIL_CALENDAR_NOTIFY_CHANNEL`); Phase 5 will route per
	// resource calendar.
	calendarChannelResolver := calendarbridge.NewDBChannelResolver(pool, os.Getenv("KMAIL_CALENDAR_NOTIFY_CHANNEL"))
	calendarNotifier := calendarbridge.NewNotifier(chatbridgeSvc.KChat(), calendarChannelResolver)
	calendarbridge.NewHandlers(calendarSvc, logger).WithNotifier(calendarNotifier).Register(mux, authMW)
	calendarbridge.NewChannelHandlers(calendarChannelResolver).Register(mux, authMW)
	calendarSharingStore := calendarbridge.NewSharingStore(pool)
	calendarbridge.NewSharingHandlers(calendarSvc, calendarSharingStore).Register(mux, authMW)
	// Background reminder worker: polls upcoming events every 60s
	// and fires KChat reminders 15min / 5min before start.
	reminderWorker := calendarbridge.NewReminderWorker(pool, calendarSvc, calendarNotifier, valkeyClient, logger)
	go reminderWorker.Run(ctx)

	auditSvc := audit.NewService(pool)
	audit.NewHandlers(auditSvc, logger).Register(mux, authMW)

	// Deliverability Control Plane (suppression, bounces, IP
	// pools, send limits, warmup, DMARC).
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
	deliverabilityHandlers := deliverability.NewHandlers(deliverabilitySvc, logger)
	deliverabilityHandlers.Register(mux, authMW)
	deliverabilityHandlers.RegisterPhase3(mux, authMW)

	// Push notifications (web / iOS / Android fan-out).
	pushTransport := buildPushTransport(logger)
	pushSvc := push.NewService(push.Config{
		Pool:        pool,
		StalwartURL: cfg.StalwartURL,
		Logger:      logger,
		Transport:   pushTransport,
	})
	push.NewHandlers(pushSvc, logger).Register(mux, authMW)

	// DKIM rotation surface (Phase 7). Lives next to the DNS
	// wizard so the wizard UI can show "rotation pending" rows
	// when an admin has rolled a new selector but DNS hasn't
	// caught up yet. The kmail-secrets envelope wraps freshly
	// generated private keys before they hit dkim_keys; in dev
	// (no KMAIL_SECRETS_KEY) the service logs a loud warning and
	// stores plaintext PEM.
	dkimSvc := dns.NewDKIMRotationService(pool, logger)
	if env, err := cmk.LoadEnvelope(); err == nil {
		dkimSvc = dkimSvc.WithEnvelope(env)
	} else {
		logger.Printf("dkim: KMAIL_SECRETS_KEY unset (%v) — DKIM private keys will be stored as plaintext PEM", err)
	}
	dns.NewDKIMHandlers(dkimSvc, logger).Register(mux, authMW)

	// Search backend abstraction (Phase 7). Meilisearch is the
	// default; OpenSearch is opt-in per-tenant via the admin
	// surface. The backend registry only contains the backends
	// configured via env so dev compose stays lean.
	var searchBackends []search.SearchBackend
	if url := os.Getenv("KMAIL_MEILISEARCH_URL"); url != "" {
		searchBackends = append(searchBackends, &search.MeilisearchBackend{
			BaseURL: url,
			APIKey:  os.Getenv("KMAIL_MEILISEARCH_API_KEY"),
		})
	}
	if url := os.Getenv("KMAIL_OPENSEARCH_URL"); url != "" {
		searchBackends = append(searchBackends, &search.OpenSearchBackend{
			BaseURL:  url,
			Username: os.Getenv("KMAIL_OPENSEARCH_USER"),
			Password: os.Getenv("KMAIL_OPENSEARCH_PASS"),
		})
	}
	searchSvc := search.NewService(search.Config{
		Pool:     pool,
		Logger:   logger,
		Backends: searchBackends,
	})
	search.NewHandlers(searchSvc, logger).Register(mux, authMW)

	// Sieve rule management (Phase 7).
	sieveSvc := sieve.NewService(sieve.Config{Pool: pool, Logger: logger})
	sieve.NewHandlers(sieveSvc, logger).Register(mux, authMW)

	// Stripe billing portal (Phase 7). The portal endpoint is a
	// no-op in dev (when KMAIL_STRIPE_SECRET_KEY is unset) — the
	// handler returns 503 with `ErrStripeUnconfigured` so the UI
	// can fall through to the existing stub-mode billing surface.
	stripeClient := billing.NewStripeClient(os.Getenv("KMAIL_STRIPE_SECRET_KEY"))
	billing.NewPortalHandlers(pool, stripeClient, logger).Register(mux, authMW)

	// WebAuthn / FIDO2 surface (Phase 7).
	webauthnHandlers := middleware.NewWebAuthnHandlers(middleware.WebAuthnConfig{
		Pool:     pool,
		Logger:   logger,
		RPID:     os.Getenv("KMAIL_WEBAUTHN_RPID"),
		RPName:   "KMail",
		RPOrigin: os.Getenv("KMAIL_WEBAUTHN_ORIGIN"),
	})
	webauthnHandlers.Register(mux, authMW)

	// Shared-inbox workflow state machine.
	sharedInboxWorkflow := sharedinbox.NewService(pool, logger)
	sharedinbox.NewHandlers(sharedInboxWorkflow, logger).Register(mux, authMW)

	tenant.NewShardHandlers(shardSvc).Register(mux, authMW)

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
	// Availability SLO tracker — Phase 4 99.9% target. Requests
	// flowing through `metrics.Middleware` are mirrored into Valkey
	// for the admin dashboard / breach history endpoints.
	sloTracker := monitoring.NewSLOTracker(valkeyClient)
	metrics = metrics.WithSLO(sloTracker)
	// Phase 5: multi-region SLO aggregator targets the 99.95%
	// availability roll-up across BFF instances. `KMAIL_SLO_REGIONS`
	// is a comma-separated list of region tokens (e.g. "us-east-1,
	// eu-west-1"); empty falls back to the single-region rollup.
	sloAggregator := monitoring.NewMultiRegionAggregator(
		valkeyClient,
		middleware.SplitOrigins(os.Getenv("KMAIL_SLO_REGIONS")),
	)
	monitoring.NewHandlers(sloTracker, logger).
		WithMultiRegion(sloAggregator).
		Register(mux, authMW)
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

	// Deliverability alert evaluator: walks every tenant every
	// 15 minutes and raises alerts on threshold breaches.
	alertEvaluator := &deliverability.AlertEvaluator{
		Service:  deliverabilitySvc.Alerts,
		Pool:     pool,
		Interval: getenvDuration("KMAIL_ALERT_EVAL_INTERVAL", 15*time.Minute),
		Logger:   logger,
	}
	go alertEvaluator.Run(ctx)

	// Shard health worker: probes every registered Stalwart
	// shard every 60s and flips offline shards out of rotation.
	shardHealth := &tenant.HealthWorker{
		Service:  shardSvc,
		Interval: getenvDuration("KMAIL_SHARD_HEALTH_INTERVAL", 60*time.Second),
		Logger:   logger,
	}
	go shardHealth.Run(ctx)

	// Phase 5 admin surfaces.
	placementSvc := tenant.NewPlacementService(pool, cfg.ZKFabric.ConsoleURL)
	tenant.NewPlacementHandlers(placementSvc, pool).Register(mux, authMW)

	retentionSvc := retention.NewService(pool)
	// Phase 6: live mode is now the default. Operators opt out
	// per-deployment via `KMAIL_RETENTION_DRY_RUN=true`. Documented
	// in `docs/DEVELOPMENT.md`.
	retentionDryRun := os.Getenv("KMAIL_RETENTION_DRY_RUN") == "true"
	retentionEnforcer := retention.NewJMAPEnforcer(shardSvc, nil, "",
		cfg.ZKFabric.ConsoleURL, "", logger)
	retentionMetrics := retention.NewMetrics(metrics.Registry)
	retentionWorker := retention.NewWorker(retentionSvc, logger).
		WithEnforcer(retentionEnforcer).
		WithDryRun(retentionDryRun).
		WithMetrics(retentionMetrics)
	retention.NewHandlers(retentionSvc, logger).WithWorker(retentionWorker).Register(mux, authMW)
	go retentionWorker.Run(ctx)

	approvalSvc := approval.NewService(pool)
	approval.NewHandlers(approvalSvc).Register(mux, authMW)

	// Phase 5 — Zero-Access Vault folders.
	vaultSvc := vault.NewVaultService(pool)
	vault.NewVaultHandlers(vaultSvc, logger).Register(mux, authMW)

	// Phase 5 — Protected folders + sharing.
	protectedSvc := vault.NewProtectedFolderService(pool)
	vault.NewProtectedFolderHandlers(protectedSvc, logger).Register(mux, authMW)

	// Phase 5 — Customer-managed keys (privacy plan only; the
	// handler enforces the plan gate via a per-request lookup).
	cmkSvc := cmk.NewCMKService(pool)
	cmk.NewHandlers(cmkSvc, pool, logger).Register(mux, authMW)

	// Phase 5 — Confidential Send portal. The public portal route
	// (`GET /api/v1/secure/{token}`) is registered *without* the
	// auth middleware by the handler; tenant-scoped admin routes
	// stay behind authMW.
	confidentialSendSvc := confidentialsend.NewService(pool).
		WithMLS(confidentialsend.NewHTTPKeyDeriver(cfg.KChatMLSEndpoint, cfg.KChatAPIToken))
	confidentialsend.NewHandlers(confidentialSendSvc, valkeyClient, logger).Register(mux, authMW)

	exportSvc := export.NewService(pool)
	// Phase 5 closeout: wire the real JMAP / CalDAV / audit
	// fan-out runner. Each dependency is best-effort so the BFF
	// keeps booting in dev when one of the downstream services
	// is unreachable.
	exportAttachmentSvc := jmap.NewAttachmentService(jmap.AttachmentConfig{
		Pool:      pool,
		S3URL:     cfg.ZKFabric.S3URL,
		AccessKey: cfg.ZKFabric.AccessKey,
		SecretKey: cfg.ZKFabric.SecretKey,
	})
	exportSvc.WithRunner(export.NewRealRunner(export.RealRunnerConfig{
		JMAP:     export.NewHTTPJMAPClient(cfg.StalwartURL, ""),
		Calendar: calendarSvc,
		Audit:    auditSvc,
		Uploader: exportAttachmentSvc,
	}))
	export.NewHandlers(exportSvc).Register(mux, authMW)
	go export.NewWorker(exportSvc, logger).Run(ctx)

	// Phase 5 closeout — SCIM 2.0 provisioning.
	scimSvc := scim.NewService(pool, tenantSvc)
	scim.NewHandlers(scimSvc, logger).Register(mux, authMW)

	// Phase 5 closeout — reverse access proxy for support /
	// SRE access to tenant data behind the existing approval
	// workflow.
	adminProxySvc := adminproxy.NewService(pool, approvalSvc, auditSvc, shardSvc)
	adminproxy.NewHandlers(adminProxySvc, logger, cfg.StalwartURL).Register(mux, authMW)
	// Phase 6: background watcher emits `session_expired` audit
	// rows once `expires_at` passes.
	go adminproxy.NewExpiryWorker(pool, auditSvc, logger).
		WithMetric(metrics.Registry).
		Run(ctx)

	// Phase 5 closeout — CardDAV contact bridge.
	contactSvc := contactbridge.NewService(contactbridge.Config{StalwartURL: cfg.StalwartURL})
	galSvc := contactbridge.NewGALService(pool, contactSvc)
	contactbridge.NewHandlers(contactSvc, logger).WithGAL(galSvc).Register(mux, authMW)

	// Phase 5 closeout — Tenant webhook event system.
	webhookSvc := webhooks.NewService(pool)
	webhooks.NewHandlers(webhookSvc, logger).Register(mux, authMW)
	go webhooks.NewWorker(webhookSvc, logger).Run(ctx)

	// Phase 5 closeout — Onboarding checklist.
	onboardingSvc := onboarding.NewService(pool)
	onboarding.NewHandlers(onboardingSvc, logger).Register(mux, authMW)
	// Phase 6: auto-complete onboarding steps from internal events.
	autoTriggerSvc := onboarding.NewAutoTriggerService(pool)
	webhookSvc.AddListener(autoTriggerSvc)

	// Wire metrics and tracing into the outer handler chain.
	handler := http.Handler(mux)
	if cfg.Observability.TracingEnabled {
		handler = middleware.TracingMiddleware(handler)
	}
	if cfg.Observability.MetricsEnabled {
		handler = metrics.Middleware(handler)
	}
	handler = middleware.RequestLogger(logger, cfg.Observability.LogFormat)(handler)

	// Outermost wrapper: security headers + CORS. The CORS allow
	// list comes from `KMAIL_CORS_ORIGINS` (comma-separated). The
	// CSP `app-src` allows the same origins so the React bundle
	// can load. Wrapped last so every response — including
	// /metrics, /healthz, and the confidential-send public portal
	// — picks up the headers.
	securityMW := middleware.NewSecurity(middleware.SecurityConfig{
		WebOrigins: middleware.SplitOrigins(os.Getenv("KMAIL_CORS_ORIGINS")),
	})
	handler = securityMW.Wrap(handler)

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           handler,
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

// getenvDuration reads a duration env var with a fallback.
func getenvDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

// buildPushTransport assembles the per-platform push transports
// (APNs / FCM / web) from env vars and wires them through a
// TransportRouter. Missing credentials downgrade to the no-op
// logging transport, which is what we want in dev.
func buildPushTransport(logger *log.Logger) push.Transport {
	router := push.NewTransportRouter(logger)
	if keyID := os.Getenv("KMAIL_APNS_KEY_ID"); keyID != "" {
		apns, err := push.NewAPNsTransport(push.APNsConfig{
			KeyID:    keyID,
			TeamID:   os.Getenv("KMAIL_APNS_TEAM_ID"),
			KeyPath:  os.Getenv("KMAIL_APNS_KEY_PATH"),
			Topic:    os.Getenv("KMAIL_APNS_TOPIC"),
			Endpoint: os.Getenv("KMAIL_APNS_ENDPOINT"),
			Logger:   logger,
		})
		if err != nil {
			logger.Printf("apns transport disabled: %v", err)
		} else {
			router.IOS = apns
		}
	}
	if path := os.Getenv("KMAIL_FCM_CREDENTIALS_PATH"); path != "" {
		fcm, err := push.NewFCMTransport(push.FCMConfig{
			CredentialsPath: path,
			Endpoint:        os.Getenv("KMAIL_FCM_ENDPOINT"),
			Logger:          logger,
		})
		if err != nil {
			logger.Printf("fcm transport disabled: %v", err)
		} else {
			router.Android = fcm
		}
	}
	return router
}
