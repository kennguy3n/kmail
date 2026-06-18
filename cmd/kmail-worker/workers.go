package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kmail/internal/adminproxy"
	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/billing"
	"github.com/kennguy3n/kmail/internal/calendarbridge"
	"github.com/kennguy3n/kmail/internal/chatbridge"
	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/deliverability"
	"github.com/kennguy3n/kmail/internal/export"
	"github.com/kennguy3n/kmail/internal/featureflags"
	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/malware"
	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/retention"
	"github.com/kennguy3n/kmail/internal/scheduledsend"
	"github.com/kennguy3n/kmail/internal/search"
	"github.com/kennguy3n/kmail/internal/snooze"
	"github.com/kennguy3n/kmail/internal/tenant"
	"github.com/kennguy3n/kmail/internal/undosend"
	"github.com/kennguy3n/kmail/internal/webhooks"
)

// workerRegistration pairs a stable, metric-safe name with a
// long-running Run loop. A registration's Run MUST block until its
// context is cancelled and return promptly thereafter — the
// supervisor (see supervise.go) uses the context to drive the 30s
// graceful drain and treats any earlier return as an unexpected
// exit worth restarting.
type workerRegistration struct {
	name string
	run  func(ctx context.Context)
}

// workerDeps holds the shared infrastructure handles every worker
// constructor draws on. They mirror the handles cmd/kmail-api
// builds for its single-binary mode so the two stay in lockstep.
type workerDeps struct {
	cfg    *config.Config
	pool   *pgxpool.Pool
	valkey *redis.Client // nil when KMAIL_VALKEY_URL is unset
	reg    prometheus.Registerer
	logger *log.Logger
}

// buildWorkers constructs every background worker that
// cmd/kmail-api used to start in-process and returns them as a
// registry. The order mirrors cmd/kmail-api/main.go so the two
// remain easy to diff. Adding a worker introduced by a sibling
// session is a one-line `regs = append(...)` at the matching point
// below (see the StorageEventWorker note near the billing block).
//
// Construction errors are returned (not logged-and-fataled) so the
// caller owns process exit and tests can assert on them.
//
// INVARIANT — construction must stay Postgres-query-free: worker
// constructors here may only stash their dependencies (pool handle,
// valkey client, config); they must NOT touch the *database* (no
// Ping, no SELECT, no migration check). The actual DB I/O happens
// later in each worker's Run loop under workerCtx. This keeps
// buildWorkers a pure config-driven assembly step, which is what lets
// worker_test.go (TestBuildWorkersBaselineRegistry /
// ...OptionalGatesEnabled) run without a live Postgres — pgxpool.New
// is lazy and nothing dials it at build time.
//
// The one Valkey exception is the shared buildInternalJMAP helper,
// which Pings Valkey at construction *only when a non-nil client is
// supplied* (to pick the shared vs. local circuit breaker). The tests
// stay backend-free precisely because they pass valkey == nil, so
// that Ping is never reached. If you add a constructor that probes
// Postgres — or unconditionally probes Valkey — at build time, those
// tests will start requiring a live backend; gate the probe behind
// the Run loop (or inject a stub in the tests) instead.
func buildWorkers(ctx context.Context, d workerDeps) ([]workerRegistration, error) {
	cfg := d.cfg
	pool := d.pool
	logger := d.logger
	valkeyClient := d.valkey

	var regs []workerRegistration

	// Shard service — shared by the JMAP proxy, the alias-sync
	// worker, the shard-health worker, and the retention enforcer.
	shardSvc := tenant.NewShardService(pool, logger)

	// One audit logger shared by every worker that writes to the
	// tamper-evident trail (search-cutover, export, adminproxy-expiry),
	// mirroring cmd/kmail-api's single auditSvc. The worker is the
	// process that actually runs these jobs when KMAIL_DISABLE_WORKERS
	// is the default true, so each must own the audit logger or its
	// system-actor entries silently drop.
	auditSvc := audit.NewService(pool)

	// JMAP proxy + internal client. The internal client is the wire
	// path the undo / scheduled / snooze dispatch workers submit
	// through, so it must carry the same mTLS + circuit-breaker
	// posture cmd/kmail-api wires.
	internalJmap, err := buildInternalJMAP(ctx, cfg, pool, valkeyClient, shardSvc, logger)
	if err != nil {
		return nil, err
	}

	// --- alias → Stalwart sync (tenant) ---
	// Gated on the Stalwart admin credentials, exactly as
	// cmd/kmail-api gates it: without an admin user the HTTP sync
	// adapter cannot authenticate to Stalwart's admin API.
	if adminUser := os.Getenv("KMAIL_STALWART_ADMIN_USER"); adminUser != "" {
		aliasSync, syncErr := tenant.NewStalwartAliasHTTPSync(shardSvc, adminUser, os.Getenv("KMAIL_STALWART_ADMIN_PASS"))
		if syncErr != nil {
			logger.Printf("alias-stalwart-sync worker disabled: %v", syncErr)
		} else {
			// Share the application pool so the alias
			// read-modify-write is serialised per principal by a
			// Postgres advisory lock across the worker and kmail-api
			// (Stalwart's x:Account/set has no ifInState guard), and a
			// logger so a failed advisory-unlock is observable.
			aliasSync = aliasSync.WithLockPool(pool).WithLogger(logger)
			w := tenant.NewAliasStalwartSyncWorker(pool, aliasSync, logger)
			regs = append(regs, workerRegistration{name: "alias-stalwart-sync", run: w.Run})
		}
	} else {
		logger.Printf("alias-stalwart-sync worker disabled: KMAIL_STALWART_ADMIN_USER not set")
	}

	// --- calendar reminder (calendarbridge) ---
	chatbridgeSvc := chatbridge.NewService(chatbridge.Config{
		KChatAPIURL:   cfg.KChatAPIURL,
		KChatAPIToken: cfg.KChatAPIToken,
		StalwartURL:   cfg.StalwartURL,
		Pool:          pool,
		Logger:        logger,
	})
	// Dev/CI only: the reminder worker resolves CalDAV account
	// names and reads collections as the Stalwart superuser over
	// plain HTTP Basic, mirroring the kmail-api wiring. Production
	// authenticates via mTLS and never sets these — gated through
	// the same IsDevEnv alias table the proxy and OIDC middleware
	// use. Without the credential davAccount is a no-op, so every
	// CalDAV call targets /dav/cal/{id}/ while Stalwart serves
	// /dav/cal/{name}/ — i.e. the reminder sweep would 404 for
	// every tenant in dev/CI. DevAdminConfig is the single source of
	// this wiring, shared with kmail-api so the two can't drift.
	calendarSvc := calendarbridge.NewService(
		calendarbridge.DevAdminConfig(cfg.StalwartURL, middleware.IsDevEnv(cfg.Env), logger),
	)
	calendarChannelResolver := calendarbridge.NewDBChannelResolver(pool, os.Getenv("KMAIL_CALENDAR_NOTIFY_CHANNEL"))
	calendarNotifier := calendarbridge.NewNotifier(chatbridgeSvc.KChat(), calendarChannelResolver)
	reminderWorker := calendarbridge.NewReminderWorker(pool, calendarSvc, calendarNotifier, valkeyClient, logger)
	regs = append(regs, workerRegistration{name: "calendar-reminder", run: reminderWorker.Run})

	// --- undo send dispatch (requires Valkey) ---
	if valkeyClient != nil {
		undoDelay := time.Duration(config.GetenvInt("KMAIL_UNDO_SEND_DELAY_SECONDS", undosend.DefaultDelaySeconds)) * time.Second
		undoSvc, undoErr := undosend.NewService(undosend.Config{
			Client: valkeyClient,
			Logger: logger,
			Delay:  undoDelay,
		})
		if undoErr != nil {
			return nil, fmt.Errorf("undosend.NewService: %w", undoErr)
		}
		undoWorker, workerErr := undosend.NewDispatchWorker(undosend.WorkerConfig{
			Service:  undoSvc,
			Internal: internalJmap,
			Logger:   logger,
		})
		if workerErr != nil {
			return nil, fmt.Errorf("undosend.NewDispatchWorker: %w", workerErr)
		}
		regs = append(regs, workerRegistration{name: "undosend-dispatch", run: undoWorker.Run})
	} else {
		logger.Printf("undosend-dispatch worker disabled: KMAIL_VALKEY_URL not set")
	}

	// --- scheduled send dispatch ---
	scheduledSvc, schedErr := scheduledsend.NewService(scheduledsend.Config{Pool: pool, Logger: logger})
	if schedErr != nil {
		return nil, fmt.Errorf("scheduledsend.NewService: %w", schedErr)
	}
	scheduledWorker, schedWorkerErr := scheduledsend.NewDispatchWorker(scheduledsend.WorkerConfig{
		Service:  scheduledSvc,
		Internal: internalJmap,
		Logger:   logger,
		Interval: getenvDuration("KMAIL_SCHEDULED_SEND_INTERVAL", 15*time.Second),
	})
	if schedWorkerErr != nil {
		return nil, fmt.Errorf("scheduledsend.NewDispatchWorker: %w", schedWorkerErr)
	}
	regs = append(regs, workerRegistration{name: "scheduledsend-dispatch", run: scheduledWorker.Run})

	// --- snooze dispatch ---
	snoozeSvc, snoozeErr := snooze.NewService(snooze.Config{Pool: pool, Logger: logger})
	if snoozeErr != nil {
		return nil, fmt.Errorf("snooze.NewService: %w", snoozeErr)
	}
	snoozeWorker, snoozeWorkerErr := snooze.NewDispatchWorker(snooze.WorkerConfig{
		Service:  snoozeSvc,
		Internal: internalJmap,
		Logger:   logger,
		Interval: getenvDuration("KMAIL_SNOOZE_INTERVAL", 30*time.Second),
	})
	if snoozeWorkerErr != nil {
		return nil, fmt.Errorf("snooze.NewDispatchWorker: %w", snoozeWorkerErr)
	}
	regs = append(regs, workerRegistration{name: "snooze-dispatch", run: snoozeWorker.Run})

	// --- billing services (quota worker + cutover sizer) ---
	billingSvc := billing.NewService(billing.Config{
		Pool:                pool,
		CoreSeatCents:       cfg.Billing.CoreSeatCents,
		ProSeatCents:        cfg.Billing.ProSeatCents,
		PrivacySeatCents:    cfg.Billing.PrivacySeatCents,
		CorePerSeatBytes:    cfg.Billing.CorePerSeatBytes,
		ProPerSeatBytes:     cfg.Billing.ProPerSeatBytes,
		PrivacyPerSeatBytes: cfg.Billing.PrivacyPerSeatBytes,
	})
	if cfg.Billing.QuotaWorkerEnabled {
		quotaWorker := billing.NewQuotaWorker(billing.QuotaWorkerConfig{
			Pool:     pool,
			Billing:  billingSvc,
			Scanner:  billing.StaticScanner{Bytes: -1},
			Interval: cfg.Billing.QuotaWorkerInterval,
			Logger:   logger,
		})
		regs = append(regs, workerRegistration{name: "billing-quota", run: quotaWorker.Run})
	} else {
		logger.Printf("billing-quota worker disabled: KMAIL_QUOTA_WORKER_ENABLED not set")
	}
	// NOTE: billing.StorageEventWorker (Session 5) registers here
	// with a single line once it lands on main, e.g.:
	//   regs = append(regs, workerRegistration{name: "billing-storage-event", run: storageEventWorker.Run})

	// --- search auto-cutover (requires both Meilisearch + OpenSearch) ---
	if cutoverWorker, ok, cutErr := buildCutoverWorker(cfg, pool, shardSvc, billingSvc, auditSvc, d.reg, logger); cutErr != nil {
		return nil, cutErr
	} else if ok {
		regs = append(regs, workerRegistration{name: "search-cutover", run: cutoverWorker.Run})
	} else {
		logger.Printf("search-cutover worker disabled: needs both Meilisearch and OpenSearch configured")
	}

	// --- deliverability alert evaluator ---
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
	alertEvaluator := &deliverability.AlertEvaluator{
		Service:  deliverabilitySvc.Alerts,
		Pool:     pool,
		Interval: getenvDuration("KMAIL_ALERT_EVAL_INTERVAL", 15*time.Minute),
		Logger:   logger,
	}
	regs = append(regs, workerRegistration{name: "deliverability-alert-evaluator", run: alertEvaluator.Run})

	// --- shard health probe (tenant) ---
	shardHealth := &tenant.HealthWorker{
		Service:  shardSvc,
		Interval: getenvDuration("KMAIL_SHARD_HEALTH_INTERVAL", 60*time.Second),
		Logger:   logger,
	}
	regs = append(regs, workerRegistration{name: "shard-health", run: shardHealth.Run})

	// --- automated shard lifecycle (WS4 Task 3) ---
	// Provisions a new shard when active capacity crosses the
	// utilisation threshold. Gated on KMAIL_SHARD_AUTOPROVISION_CMD:
	// without a provisioning command there is nothing to run (manual
	// RegisterShard remains the path), so the worker is only
	// registered when an operator opts in by pointing the var at
	// e.g. scripts/provision-shard.sh.
	if cmd := os.Getenv("KMAIL_SHARD_AUTOPROVISION_CMD"); cmd != "" {
		shardSvc.SetProvisioner(&tenant.ExecShardProvisioner{
			Command: cmd,
			Timeout: getenvDuration("KMAIL_SHARD_AUTOPROVISION_TIMEOUT", 15*time.Minute),
		})
		autoProvision := &tenant.AutoProvisionWorker{
			Service:   shardSvc,
			Interval:  getenvDuration("KMAIL_SHARD_AUTOPROVISION_INTERVAL", 5*time.Minute),
			Threshold: getenvFloat("KMAIL_SHARD_AUTOPROVISION_THRESHOLD", tenant.DefaultProvisionThreshold),
			Logger:    logger,
		}
		regs = append(regs, workerRegistration{name: "shard-autoprovision", run: autoProvision.Run})
	}

	// --- retention enforcement ---
	retentionSvc := retention.NewService(pool)
	retentionEnforcer := retention.NewJMAPEnforcer(shardSvc, nil, "", cfg.ZKFabric.ConsoleURL, "", logger)
	retentionMetrics := retention.NewMetrics(d.reg)
	retentionWorker := retention.NewWorker(retentionSvc, logger).
		WithEnforcer(retentionEnforcer).
		WithDryRun(os.Getenv("KMAIL_RETENTION_DRY_RUN") == "true").
		WithMetrics(retentionMetrics)
	regs = append(regs, workerRegistration{name: "retention", run: retentionWorker.Run})

	// --- export fan-out (calendar + audit reuse) ---
	// Mirrors cmd/kmail-api: the runner exports/queries Stalwart over
	// the same internal JMAP client the dispatch workers use, so the
	// worker process applies identical mTLS + breaker posture.
	// WithAuditLogger mirrors cmd/kmail-api: the worker is now the
	// dedicated process that runs export jobs, so the Service must
	// own the audit logger to emit export.completed / export.failed
	// entries (eDiscovery compliance). Without it Service.logAudit
	// silently no-ops on the nil audit field.
	exportSvc := export.NewService(pool).WithAuditLogger(auditSvc)
	exportAttachmentSvc := jmap.NewAttachmentService(jmap.AttachmentConfig{
		Pool:      pool,
		S3URL:     cfg.ZKFabric.S3URL,
		AccessKey: cfg.ZKFabric.AccessKey,
		SecretKey: cfg.ZKFabric.SecretKey,
	})
	exportExporter, err := jmap.NewStalwartEmailExporter(internalJmap, pool, logger)
	if err != nil {
		return nil, fmt.Errorf("jmap.NewStalwartEmailExporter: %w", err)
	}
	exportQuerier, err := jmap.NewStalwartEmailOperator(internalJmap, pool, logger)
	if err != nil {
		return nil, fmt.Errorf("jmap.NewStalwartEmailOperator: %w", err)
	}
	exportRunner, err := export.NewJMAPExportRunner(export.JMAPExportRunnerConfig{
		Exporter: exportExporter,
		Querier:  exportQuerier,
		Uploader: exportAttachmentSvc,
		Calendar: calendarSvc,
		Audit:    auditSvc,
		Logger:   logger,
	})
	if err != nil {
		return nil, fmt.Errorf("export.NewJMAPExportRunner: %w", err)
	}
	exportSvc.WithRunner(exportRunner)
	exportWorker := export.NewWorker(exportSvc, logger).WithMetrics(export.NewMetrics(d.reg))
	regs = append(regs, workerRegistration{name: "export", run: exportWorker.Run})

	// --- admin-proxy grant expiry ---
	expiryWorker := adminproxy.NewExpiryWorker(pool, auditSvc, logger).WithMetric(d.reg)
	regs = append(regs, workerRegistration{name: "adminproxy-expiry", run: expiryWorker.Run})

	// --- webhook delivery ---
	webhookSvc := webhooks.NewService(pool)
	webhookWorker := webhooks.NewWorker(webhookSvc, logger)
	regs = append(regs, workerRegistration{name: "webhooks", run: webhookWorker.Run})

	// --- feature-flag resolver refresher (WS4 Task 1) ---
	// The worker process also evaluates feature flags (e.g. workers
	// that gate behaviour on a rollout), so it installs the same
	// process-wide resolver the API does and keeps its snapshot warm.
	// Construction is Postgres-free (the store loads lazily), so this
	// honours the buildWorkers no-DB-at-construction invariant.
	flagStore := featureflags.NewStore(pool)
	flagSvc := featureflags.NewStoreService(flagStore, logger)
	featureflags.SetDefault(flagSvc)
	regs = append(regs, workerRegistration{name: "feature-flags-refresh", run: flagSvc.Run})

	return regs, nil
}

// buildInternalJMAP assembles the BFF→Stalwart JMAP proxy and the
// internal client the dispatch workers submit through. It mirrors
// the mTLS validation, ClamAV pre-deliver hook, and shared/fallback
// circuit-breaker selection in cmd/kmail-api/main.go so the worker
// process talks to Stalwart with the same posture as the API. The
// proxy's HTTP handlers are intentionally NOT mounted — the worker
// only needs the programmatic submit path.
func buildInternalJMAP(
	ctx context.Context,
	cfg *config.Config,
	pool *pgxpool.Pool,
	valkeyClient *redis.Client,
	shardSvc *tenant.ShardService,
	logger *log.Logger,
) (*jmap.InternalClient, error) {
	// Pre-deliver malware hook (parity with the API submit path).
	var malwareHook func(ctx context.Context, body []byte) error
	if addr := os.Getenv("KMAIL_CLAMAV_ADDR"); addr != "" {
		clamScanner, scanErr := malware.NewClamAVScanner(malware.ClamAVConfig{
			Addr:    addr,
			Timeout: getenvDuration("KMAIL_CLAMAV_TIMEOUT", 10*time.Second),
		})
		if scanErr != nil {
			logger.Printf("malware: ClamAV adapter disabled: %v", scanErr)
		} else {
			malwareHook = malware.NewHandlers(clamScanner, logger).PreDeliverHook
			logger.Printf("malware: ClamAV adapter enabled at %s", addr)
		}
	}

	// Surface partial mTLS configuration loudly: fatal in non-dev
	// (fail-closed), warn in dev. Mirrors cmd/kmail-api.
	if err := cfg.StalwartMTLS.Validate(); err != nil {
		if middleware.IsDevEnv(cfg.Env) {
			logger.Printf("jmap proxy: WARNING partial mTLS config (dev): %v", err)
		} else {
			return nil, fmt.Errorf("jmap proxy: partial mTLS config in env=%q (fail-closed): %w", cfg.Env, err)
		}
	}
	var stalwartTLS *jmap.ClientTLSConfig
	if cfg.StalwartMTLS.Enabled() {
		stalwartTLS = &jmap.ClientTLSConfig{
			CertFile:   cfg.StalwartMTLS.CertFile,
			KeyFile:    cfg.StalwartMTLS.KeyFile,
			CAFile:     cfg.StalwartMTLS.CAFile,
			ServerName: cfg.StalwartMTLS.ServerName,
		}
		logger.Printf("jmap proxy: mTLS to Stalwart enabled (cert=%s ca=%s server=%s)",
			cfg.StalwartMTLS.CertFile, cfg.StalwartMTLS.CAFile, cfg.StalwartMTLS.ServerName)
	} else if strings.HasPrefix(cfg.StalwartURL, "https://") {
		logger.Printf("jmap proxy: WARNING StalwartURL is HTTPS but KMAIL_STALWART_TLS_CERT/KEY are unset \u2014 worker will not authenticate to Stalwart")
	}

	// Breaker tunables resolved once so the shared (Valkey-backed)
	// and per-pod fallback paths stay in lockstep.
	breakerThreshold := config.GetenvInt("KMAIL_BREAKER_THRESHOLD", 3)
	breakerCooldown := getenvDuration("KMAIL_BREAKER_COOLDOWN", 30*time.Second)
	breakerWindow := getenvDuration("KMAIL_BREAKER_WINDOW", 60*time.Second)

	var jmapBreaker jmap.CircuitBreaker
	if valkeyClient != nil {
		pingCtx, pingCancel := context.WithTimeout(ctx, 2*time.Second)
		pingErr := valkeyClient.Ping(pingCtx).Err()
		pingCancel()
		forceShared := config.GetenvBool("KMAIL_BREAKER_SHARED_FORCE", false)
		switch {
		case pingErr == nil:
			shared, breakerErr := jmap.NewRedisCircuitBreaker(jmap.RedisCircuitBreakerConfig{
				Client:    valkeyClient,
				Logger:    logger,
				Threshold: breakerThreshold,
				Cooldown:  breakerCooldown,
				Window:    breakerWindow,
			})
			if breakerErr != nil {
				return nil, fmt.Errorf("jmap.NewRedisCircuitBreaker: %w", breakerErr)
			}
			jmapBreaker = shared
			logger.Printf("jmap: shared circuit breaker enabled against %s", cfg.ValkeyURL)
		case forceShared:
			shared, breakerErr := jmap.NewRedisCircuitBreaker(jmap.RedisCircuitBreakerConfig{
				Client:    valkeyClient,
				Logger:    logger,
				Threshold: breakerThreshold,
				Cooldown:  breakerCooldown,
				Window:    breakerWindow,
			})
			if breakerErr != nil {
				return nil, fmt.Errorf("jmap.NewRedisCircuitBreaker: %w", breakerErr)
			}
			jmapBreaker = shared
			logger.Printf("jmap: shared circuit breaker forced (KMAIL_BREAKER_SHARED_FORCE=1) against unreachable %s: ping=%v", cfg.ValkeyURL, pingErr)
		default:
			logger.Printf("jmap: shared circuit breaker disabled — Valkey %s unreachable (%v); falling back to per-pod in-process breaker", cfg.ValkeyURL, pingErr)
		}
	} else {
		logger.Printf("jmap: shared circuit breaker disabled (KMAIL_VALKEY_URL unset); falling back to per-pod breaker")
	}

	proxy, err := jmap.NewProxy(jmap.ProxyConfig{
		StalwartURL:           cfg.StalwartURL,
		Pool:                  pool,
		Logger:                logger,
		Shards:                shardSvc,
		PreDeliverHook:        malwareHook,
		TLS:                   stalwartTLS,
		Breaker:               jmapBreaker,
		CircuitBreakThreshold: breakerThreshold,
		CircuitBreakCooldown:  breakerCooldown,
		CircuitBreakWindow:    breakerWindow,
	})
	if err != nil {
		return nil, fmt.Errorf("jmap.NewProxy: %w", err)
	}
	internalJmap, err := jmap.NewInternalClient(proxy)
	if err != nil {
		return nil, fmt.Errorf("jmap.NewInternalClient: %w", err)
	}
	return internalJmap, nil
}

// buildCutoverWorker wires the Meilisearch→OpenSearch auto-cutover
// worker. It returns ok=false (and a nil worker) when either search
// backend is not configured, matching cmd/kmail-api's gate: a
// cutover is only meaningful when both the source (Meilisearch) and
// the target (OpenSearch) exist.
func buildCutoverWorker(
	cfg *config.Config,
	pool *pgxpool.Pool,
	shardSvc *tenant.ShardService,
	billingSvc *billing.Service,
	auditLogger search.AuditLogger,
	reg prometheus.Registerer,
	logger *log.Logger,
) (*search.CutoverWorker, bool, error) {
	shardResolver := search.ShardResolverFunc(func(ctx context.Context, tenantID string) (string, error) {
		return shardSvc.GetTenantShardID(ctx, tenantID)
	})

	var backends []search.SearchBackend
	meiliURL := os.Getenv("KMAIL_MEILISEARCH_URL")
	if meiliURL != "" {
		meiliKey := os.Getenv("KMAIL_MEILISEARCH_API_KEY")
		backends = append(backends, search.NewMeilisearchBackend(meiliURL, meiliKey))
		sharedMeili, err := search.NewSharedMeilisearchBackend(meiliURL, meiliKey, shardResolver)
		if err != nil {
			return nil, false, fmt.Errorf("search.NewSharedMeilisearchBackend: %w", err)
		}
		backends = append(backends, sharedMeili)
	}
	openURL := os.Getenv("KMAIL_OPENSEARCH_URL")
	if openURL != "" {
		openUser := os.Getenv("KMAIL_OPENSEARCH_USER")
		openPass := os.Getenv("KMAIL_OPENSEARCH_PASS")
		backends = append(backends, search.NewOpenSearchBackend(openURL, openUser, openPass))
		sharedOpen, err := search.NewSharedOpenSearchBackend(openURL, openUser, openPass, shardResolver)
		if err != nil {
			return nil, false, fmt.Errorf("search.NewSharedOpenSearchBackend: %w", err)
		}
		backends = append(backends, sharedOpen)
	}

	if meiliURL == "" || openURL == "" {
		return nil, false, nil
	}

	searchSvc := search.NewService(search.Config{Pool: pool, Logger: logger, Backends: backends})
	sizer := search.MailboxSizerFunc(func(ctx context.Context, tenantID string) (int64, error) {
		q, err := billingSvc.GetQuota(ctx, tenantID)
		if err != nil {
			return 0, err
		}
		return q.StorageUsedBytes, nil
	})
	source := search.MessageSourceFunc(func(ctx context.Context, tenantID string) ([]search.Message, error) {
		return searchSvc.Export(ctx, tenantID)
	})
	// Audit + Metrics mirror cmd/kmail-api's cutover wiring. The worker
	// is the process that actually performs the migration when workers
	// run out-of-process (the default), so it must (a) emit the
	// system-actor completed/failed audit entries the API would have,
	// and (b) export kmail_search_cutover_* on the worker's own
	// registry — otherwise both the audit trail and the metrics for
	// real cutovers silently vanish in the default deployment. Unlike
	// the API (whose registry is built after the worker), the worker's
	// registry already exists here, so NewCutoverMetrics(reg) registers
	// directly.
	cutover, err := search.NewCutoverWorker(search.CutoverConfig{
		Pool:        pool,
		Service:     searchSvc,
		Sizer:       sizer,
		Source:      source,
		Logger:      logger,
		Audit:       auditLogger,
		Metrics:     search.NewCutoverMetrics(reg),
		Threshold:   config.GetenvInt64("KMAIL_SEARCH_CUTOVER_THRESHOLD_BYTES", 0),
		Interval:    getenvDuration("KMAIL_SEARCH_CUTOVER_INTERVAL", time.Hour),
		MaxFailures: config.GetenvInt("KMAIL_SEARCH_CUTOVER_MAX_FAILURES", 5),
		MaxRetryGap: getenvDuration("KMAIL_SEARCH_CUTOVER_RETRY_GAP", time.Hour),
	})
	if err != nil {
		return nil, false, fmt.Errorf("search.NewCutoverWorker: %w", err)
	}
	return cutover, true, nil
}

// getenvDuration reads a duration env var with a fallback. Mirrors
// the helper in cmd/kmail-api/main.go so worker poll cadences honour
// the same KMAIL_* overrides.
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

// getenvFloat reads a float64 env var with a fallback. Used for the
// shard auto-provision utilisation threshold (e.g.
// KMAIL_SHARD_AUTOPROVISION_THRESHOLD=0.85).
func getenvFloat(key string, fallback float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fallback
	}
	return f
}
