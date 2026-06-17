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
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kmail/internal/adminproxy"
	"github.com/kennguy3n/kmail/internal/approval"
	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/billing"
	"github.com/kennguy3n/kmail/internal/calendarbridge"
	"github.com/kennguy3n/kmail/internal/chatbridge"
	"github.com/kennguy3n/kmail/internal/cmk"
	"github.com/kennguy3n/kmail/internal/confidentialsend"
	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/contactbridge"
	"github.com/kennguy3n/kmail/internal/deliverability"
	"github.com/kennguy3n/kmail/internal/dns"
	"github.com/kennguy3n/kmail/internal/export"
	"github.com/kennguy3n/kmail/internal/featureflags"
	"github.com/kennguy3n/kmail/internal/iamcore"
	"github.com/kennguy3n/kmail/internal/integrations"
	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/malware"
	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/migration"
	"github.com/kennguy3n/kmail/internal/monitoring"
	"github.com/kennguy3n/kmail/internal/oauth"
	"github.com/kennguy3n/kmail/internal/onboarding"
	"github.com/kennguy3n/kmail/internal/priority"
	"github.com/kennguy3n/kmail/internal/push"
	"github.com/kennguy3n/kmail/internal/retention"
	"github.com/kennguy3n/kmail/internal/scheduledsend"
	"github.com/kennguy3n/kmail/internal/scim"
	"github.com/kennguy3n/kmail/internal/search"
	"github.com/kennguy3n/kmail/internal/secrets"
	"github.com/kennguy3n/kmail/internal/sharedinbox"
	"github.com/kennguy3n/kmail/internal/sieve"
	"github.com/kennguy3n/kmail/internal/smartfeatures"
	"github.com/kennguy3n/kmail/internal/snooze"
	syncsvc "github.com/kennguy3n/kmail/internal/sync"
	"github.com/kennguy3n/kmail/internal/tenant"
	"github.com/kennguy3n/kmail/internal/undosend"
	"github.com/kennguy3n/kmail/internal/valkeyurl"
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

	// KMAIL_DISABLE_WORKERS decouples the background workers from
	// the API process. The workers now run in their own
	// `cmd/kmail-worker` binary (Session 6 decomposition), so the
	// default is `true` — kmail-api serves HTTP only and does NOT
	// start any background goroutine. Set it to `false` to restore
	// the single-binary dev mode where the API also runs every
	// worker in-process (handy for `go run ./cmd/kmail-api` against
	// a bare docker-compose stack without a separate worker pod).
	disableWorkers := config.GetenvBool("KMAIL_DISABLE_WORKERS", true)
	if disableWorkers {
		logger.Printf("background workers disabled in kmail-api (run cmd/kmail-worker); set KMAIL_DISABLE_WORKERS=false for single-binary dev mode")
	} else {
		logger.Printf("background workers ENABLED in kmail-api (KMAIL_DISABLE_WORKERS=false) — single-binary dev mode")
	}

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", healthzHandler)
	mux.HandleFunc("GET /readyz", readyzHandler(pool))

	// lazyProvision is wired after the tenant Service is constructed
	// (further down in main). The OIDC PostAuthMiddleware closure
	// below captures this pointer and reads it at request time, so
	// every authenticated route — including those registered before
	// the tenant Service exists — picks up lazy provisioning once it
	// is assigned. All writes happen during single-threaded startup,
	// strictly before the HTTP server starts serving, so the
	// closure's read is race-free.
	var lazyProvision *middleware.LazyProvision
	oidcCfg := middleware.OIDCConfig{
		Issuer:         cfg.KChatOIDCIssuer,
		Audience:       cfg.KChatOIDCAudience,
		DevBypassToken: cfg.DevBypassToken,
		Env:            cfg.Env,
		Pool:           pool,
		Logger:         logger,
	}
	if cfg.IAMCore.Enabled() && cfg.IAMCore.LazyProvision {
		oidcCfg.PostAuthMiddleware = func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if lazyProvision != nil {
					lazyProvision.Handle(w, r, next)
					return
				}
				next.ServeHTTP(w, r)
			})
		}
	}
	authMW, err := middleware.NewOIDC(oidcCfg)
	if err != nil {
		logger.Fatalf("middleware.NewOIDC: %v", err)
	}

	// Shared Valkey-backed rate-limit store. ONE underlying
	// `*RedisStore` is built here and reused by every package
	// that needs a rate-limit counter:
	//   * `middleware.RateLimiter` (auth/JMAP request limiter)
	//   * `integrations.Service` (per-OAuth2-client outbound
	//     dispatch quota, plumbed below)
	//
	// Reusing one store is mandatory in production — each
	// `middleware.NewRedisStore` call opens its own
	// `go-redis.Client` with its own pool, so building a second
	// one here would double the open file descriptors / Valkey
	// connection budget without any functional benefit. The
	// store is built unconditionally (even when
	// `cfg.RateLimit.Enabled = false` for the auth limiter)
	// because the integrations dispatcher quota is governed by
	// its own per-client `dispatch_quota_per_hour` setting and
	// is enabled independently of `cfg.RateLimit`.
	limiterStore, err := middleware.NewRedisStore(cfg.ValkeyURL)
	if err != nil {
		logger.Fatalf("middleware.NewRedisStore: %v", err)
	}
	// Release the limiter store's connection pool on shutdown.
	// `middleware.NewRedisStore` constructs its own `*redis.Client`
	// with its own pool (separate from the `valkeyClient` pool
	// closed at the analogous block ~135 lines below — see the
	// "Two separate Redis client pools" doc block at the top of
	// this function for the design rationale on why the two pools
	// stay separate). Without this defer the pool's TCP
	// connections would only be reclaimed by the GC finalizer at
	// process exit; CI leak detectors and graceful-shutdown
	// drains both treat that as a regression. `*redis.Client.Close`
	// is idempotent and safe from defer.
	defer func() {
		if cerr := limiterStore.Client.Close(); cerr != nil {
			logger.Printf("limiter store: close: %v", cerr)
		}
	}()

	// Valkey-backed rate limiter. Enabled via config; when
	// disabled the limiter is a no-op and the middleware passes
	// the request through untouched. Plumbed in between the OIDC
	// gate (needs identity) and the JMAP + tenant handlers.
	var rateLimiter *middleware.RateLimiter
	if cfg.RateLimit.Enabled {
		// Fail-closed posture: production replicas degrade to the
		// in-memory token-bucket fallback (and 503 past it) when
		// Valkey errors, instead of silently bypassing the
		// limiter. Default off only for dev environments — gated
		// through the same IsDevEnv alias table the OIDC
		// middleware uses (KMAIL_ENV=dev resolves to development).
		failClosed := config.GetenvBool("KMAIL_RATELIMIT_FAIL_CLOSED", !middleware.IsDevEnv(cfg.Env))
		rateLimiter = middleware.NewRateLimiter(middleware.RateLimiterConfig{
			Client:     limiterStore,
			TenantRPM:  cfg.RateLimit.TenantRPM,
			UserRPM:    cfg.RateLimit.UserRPM,
			Window:     cfg.RateLimit.Window,
			FailClosed: failClosed,
			Logger:     logger,
		})
	}
	// Server-side session ledger layered on top of the stateless
	// OIDC bearer auth: a concurrent-session cap, session revocation
	// (refuse a token at the KMail boundary before its JWT expires),
	// and "active sessions" visibility. The store reuses the rate
	// limiter's Valkey pool (`limiterStore.Client`) so no second
	// connection pool is opened, and is shared across replicas so
	// the cap and revocation are globally consistent.
	//
	// Enforcement is gated by KMAIL_SESSION_ENABLED (default off) so
	// deployments opt in deliberately — when off, sessionMgr.Wrap is
	// an identity passthrough. The list/revoke API is always served
	// when a store is present. See docs/SESSIONS.md.
	sessionMgr := middleware.NewSessionManager(middleware.SessionConfig{
		Store:         middleware.NewRedisSessionStore(limiterStore.Client),
		Enabled:       config.GetenvBool("KMAIL_SESSION_ENABLED", false),
		IdleTimeout:   getenvDuration("KMAIL_SESSION_IDLE_TIMEOUT", middleware.DefaultSessionIdleTimeout),
		MaxConcurrent: config.GetenvInt("KMAIL_SESSION_MAX_CONCURRENT", middleware.DefaultSessionMaxConcurrent),
		Logger:        logger,
	})
	// Session list/revoke endpoints are auth-gated and rate-limited
	// (per authenticated tenant/user, same Valkey limiter as the rest
	// of the API) so an authenticated caller cannot hammer them for
	// small-scale abuse. They deliberately do NOT run session
	// *enforcement* middleware: a user holding a revoked session must
	// still be able to list and revoke their sessions (they have a
	// valid JWT, only the session record is revoked). See
	// docs/SESSIONS.md.
	wrapSessionAPI := func(h http.Handler) http.Handler {
		inner := h
		if rateLimiter != nil {
			inner = rateLimiter.Wrap(h)
		}
		return authMW.Wrap(inner)
	}
	middleware.NewSessionHandlers(sessionMgr).Register(mux, wrapSessionAPI)

	wrapAuthRL := func(h http.Handler) http.Handler {
		// Auth always sits at the outermost layer so 401s short
		// out before the rate limiter ever consults Valkey. The
		// rate limiter, when enabled, is inserted BETWEEN auth
		// and the handler so the limiter can read tenant/user IDs
		// from the context that auth populates. Session enforcement
		// sits just inside auth (it also needs identity) and is a
		// no-op unless KMAIL_SESSION_ENABLED is set.
		inner := h
		if rateLimiter != nil {
			inner = rateLimiter.Wrap(h)
		}
		inner = sessionMgr.Wrap(inner)
		return authMW.Wrap(inner)
	}

	// Multi-tenant Stalwart shard routing — constructed early so
	// the JMAP proxy can resolve per-tenant primary + secondary
	// Stalwart URLs on every request.
	shardSvc := tenant.NewShardService(pool, logger)

	// Optional malware scanner (Phase 8). When KMAIL_CLAMAV_ADDR is
	// unset we install a no-op scanner so the JMAP submit path
	// stays unchanged.
	var malwareHook func(ctx context.Context, body []byte) error
	if addr := os.Getenv("KMAIL_CLAMAV_ADDR"); addr != "" {
		clamScanner, scanErr := malware.NewClamAVScanner(malware.ClamAVConfig{
			Addr:    addr,
			Timeout: getenvDuration("KMAIL_CLAMAV_TIMEOUT", 10*time.Second),
		})
		if scanErr != nil {
			logger.Printf("malware: ClamAV adapter disabled: %v", scanErr)
		} else {
			handlers := malware.NewHandlers(clamScanner, logger)
			handlers.Register(mux, authMW.Wrap)
			malwareHook = handlers.PreDeliverHook
			logger.Printf("malware: ClamAV adapter enabled at %s", addr)
		}
	} else {
		// Always expose the scan endpoint so the React UI can
		// shape its compose-time call against a real handler;
		// the no-op scanner returns clean for everything.
		malware.NewHandlers(malware.NewNoopScanner(), logger).Register(mux, authMW.Wrap)
	}

	// Surface partial mTLS configuration loudly. `Enabled()` only
	// returns true when BOTH cert+key are set, so an operator that
	// sets only one (or only CAFile / ServerName) would otherwise
	// see the proxy silently fall through to plain HTTP with no
	// hint at boot. `Validate()` reports the specific mismatch.
	//
	// In dev we log a warning so a developer mid-mTLS-setup
	// doesn't get blocked from booting the BFF. In any non-dev
	// environment (staging, production, or anything the auth
	// middleware would treat as production by default) this is
	// fatal — the alternative is a deployment that says it's
	// running with mTLS in its config but actually talks plain
	// HTTP to Stalwart, which is the misconfiguration the
	// production-fail-closed rule exists to catch.
	if err := cfg.StalwartMTLS.Validate(); err != nil {
		if middleware.IsDevEnv(cfg.Env) {
			logger.Printf("jmap proxy: WARNING partial mTLS config (dev): %v", err)
		} else {
			logger.Fatalf("jmap proxy: partial mTLS config in env=%q (fail-closed): %v", cfg.Env, err)
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
		// HTTPS without a client cert is a production-config bug.
		// The proxy itself also logs a warning, but surfacing it
		// here makes the misconfiguration obvious at boot.
		logger.Printf("jmap proxy: WARNING StalwartURL is HTTPS but KMAIL_STALWART_TLS_CERT/KEY are unset \u2014 BFF will not authenticate to Stalwart")
	}
	// Valkey is consumed by deliverability, push, calendar reminders,
	// the SLO tracker, AND the shared JMAP circuit breaker (Phase 5).
	// Stand it up early so the breaker can share trip state across
	// every BFF pod — a 5xx storm against shard X opens the breaker
	// once across the fleet instead of once per pod.
	// Resolve breaker tunables once so both the shared
	// (Valkey-backed) and per-pod fallback paths use the same
	// threshold / cooldown / window. Keeping the two impls in
	// lockstep prevents semantic drift when an operator toggles
	// `KMAIL_VALKEY_URL` on or off.
	breakerThreshold := config.GetenvInt("KMAIL_BREAKER_THRESHOLD", 3)
	breakerCooldown := getenvDuration("KMAIL_BREAKER_COOLDOWN", 30*time.Second)
	breakerWindow := getenvDuration("KMAIL_BREAKER_WINDOW", 60*time.Second)

	// `cfg.ValkeyURL` arrives in one of two wire forms the codebase
	// accepts — a `redis://` / `rediss://` DSN (the Helm Secret
	// default, also the only form that lets an operator point at
	// managed Valkey/Redis with TLS) or a bare `host:port`
	// (the docker-compose default and the in-tree dev convention).
	// `redis.NewClient` only understands the bare form on
	// `Options.Addr`, so route through `valkeyurl.Parse` to
	// normalise both. Without this normalisation a Helm deployment
	// shipping the chart's `redis://valkey:6379` Secret would try
	// to resolve the literal string `redis://valkey` as a DNS name
	// and fail at boot — the exact failure mode Phase A's PR #31
	// was opened to close.
	var valkeyClient *redis.Client
	if cfg.ValkeyURL != "" {
		opts, err := valkeyurl.Parse(cfg.ValkeyURL)
		if err != nil {
			logger.Fatalf("valkey url %q: %v", cfg.ValkeyURL, err)
		}
		valkeyClient = redis.NewClient(opts)
		// Release the connection pool on shutdown so the
		// process exits cleanly (and so leak detectors in CI
		// don't flag a dangling client). The redis client's
		// Close is idempotent and safe to call from defer.
		defer func() {
			if cerr := valkeyClient.Close(); cerr != nil {
				logger.Printf("valkey: close: %v", cerr)
			}
		}()
	}

	// Choose the breaker impl based on whether Valkey is actually
	// reachable. cfg.ValkeyURL has a non-empty default
	// ("redis://localhost:6379" since the Phase D port-layout flip;
	// previously "valkey:6379" — both forms still parse via
	// `valkeyurl.Parse` for backward compatibility with any
	// external caller that still hand-crafts the bare form)
	// so `valkeyClient != nil` is always true in practice — that's
	// fine because the rest of the program's Valkey consumers
	// (deliverability, push, calendar reminder, SLO tracker, ...)
	// already tolerate per-call errors and will recover when Valkey
	// comes back. The JMAP shard breaker, however, is on the request
	// hot path: if every Open() round-trips to an unreachable Valkey
	// and we just log+allow, a 5xx storm against a shard never trips
	// the breaker AND every request eats the round-trip latency.
	//
	// Strategy: ping Valkey with a short bounded timeout. If it
	// answers, wire the shared (Valkey-backed) breaker so trip state
	// is fleet-wide. If it doesn't, leave the *redis.Client live
	// (consumers that tolerate failures may still benefit when
	// Valkey recovers) but fall the proxy back to the in-process
	// breaker so trip decisions happen locally, fast, and with no
	// blocked-network surface area. The fallback is opt-out via
	// `KMAIL_BREAKER_SHARED_FORCE` for the edge case of an operator
	// who doesn't want the fallback (e.g., they prefer "fail loud").
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
				logger.Fatalf("jmap.NewRedisCircuitBreaker: %v", breakerErr)
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
				logger.Fatalf("jmap.NewRedisCircuitBreaker: %v", breakerErr)
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
		logger.Fatalf("jmap.NewProxy: %v", err)
	}
	// Graceful degradation (Phase 5, opt-in via KMAIL_DEGRADATION_ENABLED).
	// When the tenant's Stalwart shards are all tripped, a GET on a
	// read path (default /jmap/session) is served from the last
	// successful Valkey-cached response with X-KMail-Degraded:true
	// instead of a 502/503, so the client can still bootstrap during
	// an outage. Writes on those paths return a clean 503. Health is
	// the proxy's own per-tenant breaker view, so the verdict matches
	// its routing decision. Wrapped inside wrapAuthRL so the cache
	// key can scope by the tenant/user the auth layer populates.
	jmapHandler := http.Handler(proxy)
	if config.GetenvBool("KMAIL_DEGRADATION_ENABLED", false) {
		degradation := middleware.NewDegradation(middleware.DegradationConfig{
			Cache:       valkeyClient,
			HealthCheck: func(ctx context.Context) bool { return proxy.ShardsAvailable(ctx, middleware.TenantIDFrom(ctx)) },
			ReadPaths:   strings.Fields(os.Getenv("KMAIL_DEGRADATION_READ_PATHS")),
			CacheTTL:    getenvDuration("KMAIL_DEGRADATION_TTL", 5*time.Minute),
			Logger:      logger,
		})
		if degradation == nil {
			logger.Printf("jmap: graceful degradation requested (KMAIL_DEGRADATION_ENABLED) but disabled — KMAIL_VALKEY_URL unset, no cache to serve from")
		} else {
			// Install a per-request shard-resolution memo outside the
			// degradation middleware so the health check (ShardsAvailable)
			// and the proxy's own routing share one GetTenantShard query
			// instead of each issuing their own on every eligible read.
			degraded := degradation.Wrap(proxy)
			jmapHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				degraded.ServeHTTP(w, r.WithContext(jmap.WithShardResolveMemo(r.Context())))
			})
			logger.Printf("jmap: graceful degradation enabled — read paths fall back to last-known-good Valkey cache during a Stalwart outage")
		}
	}
	// Everything under /jmap is authenticated and forwarded to
	// Stalwart. The trailing-slash pattern owns every path below
	// /jmap/ so subpaths like /jmap/session and /jmap/upload route
	// here, while the bare /jmap lands on the session endpoint.
	mux.Handle("/jmap", wrapAuthRL(jmapHandler))
	mux.Handle("/jmap/", wrapAuthRL(jmapHandler))

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
	// Phase 8: outbound Stripe wiring. Disabled when
	// KMAIL_STRIPE_SECRET_KEY is empty (the corresponding methods
	// short-circuit on `Configured()`).
	if k := os.Getenv("KMAIL_STRIPE_SECRET_KEY"); k != "" {
		stripeClient := billing.NewStripeClient(k)
		planPrices := map[string]string{
			"core":    os.Getenv("KMAIL_STRIPE_PRICE_CORE"),
			"pro":     os.Getenv("KMAIL_STRIPE_PRICE_PRO"),
			"privacy": os.Getenv("KMAIL_STRIPE_PRICE_PRIVACY"),
		}
		billingLifecycleEarly.WithStripe(stripeClient, planPrices)
	}
	billing.NewHandlers(billingSvc, logger).WithLifecycle(billingLifecycleEarly).Register(mux, authMW)
	// Captured so the self-service signup completer can be injected
	// later (the signup service depends on the tenant service, built
	// further down). See the signup wiring block near onboarding.
	stripeWebhook := billing.NewWebhookHandler(billing.WebhookConfig{
		Lifecycle:           billingLifecycleEarly,
		StripeWebhookSecret: os.Getenv("KMAIL_STRIPE_WEBHOOK_SECRET"),
		Logger:              logger,
	})
	stripeWebhook.Register(mux)

	// kmail-secrets envelope. KMAIL_SECRETS_KEY is the single
	// master key from which every BFF-side at-rest encryption key
	// derives — DKIM private keys, TOTP shared secrets, recovery
	// codes, HSM credentials, per-tenant S3 secret keys. We load
	// it exactly once, here, so the operational signal ("is the
	// envelope wired up?") is logged once instead of four times,
	// and so every consumer shares the same `*cmk.AESGCMEnvelope`
	// value (cheaper, and makes future KMS-backed rotations a
	// single swap).
	// Rotation-aware load: in addition to KMAIL_SECRETS_KEY this
	// also honours KMAIL_SECRETS_KEY_RETIRED (comma-separated old
	// keys) so the master key can be rotated without downtime —
	// new writes seal under the new key while reads still
	// decrypt rows sealed under a retired key. With no retired
	// keys configured this behaves identically to the previous
	// single-key cmk.LoadEnvelope. See docs/SECRETS.md.
	secretsEnvelope, secretsEnvelopeErr := secrets.LoadEnvelope(context.Background(), nil)
	if secretsEnvelopeErr != nil {
		// DKIM, TOTP, and the zk-object-fabric provisioner fall
		// back to plaintext-on-disk when the envelope is unset
		// (the legacy behaviour, kept for dev). HSM credential
		// registration is *refused* in this state by the
		// `ErrEnvelopeNotConfigured` guard in cmk/hsm.go — the
		// API will return 503 for HSM registration. Keep the log
		// message honest about that asymmetry so an operator who
		// reads only this line doesn't believe HSM credentials
		// are silently stored plaintext.
		logger.Printf("secrets: KMAIL_SECRETS_KEY unset (%v) — DKIM/TOTP/storage secrets will be stored unwrapped, HSM registration will be refused (DEV ONLY)", secretsEnvelopeErr)
		secretsEnvelope = nil
	}

	// Per-tenant zk-object-fabric provisioning. CreateTenant calls
	// Provision after the DB insert so every new tenant gets its
	// own bucket + API key + placement policy without an operator
	// running a separate one-shot. The shared `secretsEnvelope`
	// loaded above wraps the per-tenant S3 secret_key minted by
	// the fabric console before it lands in
	// `tenant_storage_credentials.encrypted_secret_key`. Same
	// master key as DKIM/TOTP/HSM creds — Phase 5 swaps the master
	// for a per-tenant CMK envelope without touching the
	// provisioner call site.
	zkProvisioner := tenant.NewZKFabricProvisioner(tenant.ZKFabricProvisioner{
		Pool:           pool,
		S3URL:          cfg.ZKFabric.S3URL,
		ConsoleURL:     cfg.ZKFabric.ConsoleURL,
		AdminAccessKey: cfg.ZKFabric.AccessKey,
		AdminSecretKey: cfg.ZKFabric.SecretKey,
		Envelope:       secretsEnvelope,
		Logger:         logger,
	})
	// Phase 8 — build the shared-inbox workflow service early so
	// the tenant service can wire its `WithSharedInboxMembershipHook`
	// to fire MLS-group rotations when membership changes.
	sharedInboxWorkflowEarly := sharedinbox.NewService(pool, logger).
		WithMLS(sharedinbox.NewHTTPMLSGroupManager(cfg.KChatMLSEndpoint, cfg.KChatAPIToken))
	tenantSvc := tenant.NewService(pool).
		WithSeatAccounter(billingSvc).
		WithStorageProvisioner(zkProvisioner).
		WithBillingLifecycle(billingLifecycleEarly).
		WithSharedInboxMembershipHook(func(ctx context.Context, _ /*tenantID*/, inboxID string, members []string, reason string) {
			sharedInboxWorkflowEarly.HandleMembershipChange(ctx, inboxID, members, reason)
		})
	// Phase 9 — alias CRUD optionally mirrors writes to Stalwart's
	// principal database so inbound SMTP routes the alias address
	// to the user's account. The BFF row is authoritative for the
	// admin console even when Stalwart is unreachable, so a missing
	// admin user is logged but does not fail startup.
	if adminUser := os.Getenv("KMAIL_STALWART_ADMIN_USER"); adminUser != "" {
		aliasSync, err := tenant.NewStalwartAliasHTTPSync(
			shardSvc, adminUser, os.Getenv("KMAIL_STALWART_ADMIN_PASS"),
		)
		if err != nil {
			logger.Printf("stalwart alias sync disabled: %v", err)
		} else {
			// Wire the application pool so the alias read-modify-write
			// is serialised per principal by a Postgres advisory lock
			// (Stalwart's x:Account/set has no ifInState guard).
			aliasSync = aliasSync.WithLockPool(pool)
			tenantSvc = tenantSvc.WithStalwartAliasSync(aliasSync).WithLogger(logger)
			// Drain `alias_stalwart_sync_queue` (see `migrations/001_baseline.sql`)
			// in the background. The Tenant Service enqueues
			// sync intents atomically with each alias write and
			// then attempts Stalwart sync inline; this worker
			// retries the ones that fail inline so a Stalwart
			// outage eventually converges without operator
			// intervention.
			if !disableWorkers {
				go tenant.NewAliasStalwartSyncWorker(pool, aliasSync, logger).Run(ctx)
			}
		}
	} else {
		logger.Printf("stalwart alias sync disabled: KMAIL_STALWART_ADMIN_USER not set")
	}

	// --- iam-core OIDC integration (optional) ---
	// Enabled when KMAIL_IAM_CORE_MGMT_URL is set. Wires three
	// things, each independently gated:
	//   1. an M2M Management API client (token-cached) used to
	//      enrich webhook events;
	//   2. a signature-verified webhook receiver at
	//      POST /api/v1/webhooks/iam-core that provisions /
	//      deprovisions tenants and mailboxes from iam-core events;
	//   3. lazy tenant provisioning, attached via the OIDC
	//      PostAuthMiddleware chokepoint declared above, which
	//      provisions a tenant on first authenticated request when
	//      the webhook has not already (KMAIL_IAM_CORE_LAZY_PROVISION).
	// See docs/IAM_CORE_INTEGRATION.md.
	if cfg.IAMCore.Enabled() {
		var iamClient *iamcore.Client
		if cfg.IAMCore.M2MClientID != "" {
			c, err := iamcore.New(iamcore.Config{
				MgmtURL:      cfg.IAMCore.MgmtURL,
				ClientID:     cfg.IAMCore.M2MClientID,
				ClientSecret: cfg.IAMCore.M2MClientSecret,
				Audience:     cfg.IAMCore.M2MAudience,
				Logger:       logger,
			})
			if err != nil {
				logger.Fatalf("iamcore client: %v", err)
			}
			iamClient = c
		} else {
			logger.Printf("iam-core: M2M client disabled (KMAIL_IAM_CORE_M2M_CLIENT_ID unset); webhook events provisioned from payload only")
		}

		if cfg.IAMCore.WebhookSecret != "" {
			rec := iamcore.NewWebhookReceiver(cfg.IAMCore.WebhookSecret, tenantSvc, logger)
			if iamClient != nil {
				rec = rec.WithClient(iamClient)
			}
			rec.Register(mux)
			logger.Printf("iam-core: webhook receiver mounted at POST /api/v1/webhooks/iam-core")
		} else {
			logger.Printf("iam-core: webhook receiver disabled (KMAIL_IAM_CORE_WEBHOOK_SECRET unset)")
		}

		if cfg.IAMCore.LazyProvision {
			lazyProvision = middleware.NewLazyProvision(middleware.LazyProvisionConfig{
				Provisioner: tenantSvc.LazyProvisioner(),
				Cache:       valkeyClient,
				Logger:      logger,
			})
			logger.Printf("iam-core: lazy tenant provisioning enabled")
		}
		logger.Printf("iam-core: integration active (mgmt=%s)", cfg.IAMCore.MgmtURL)
	}

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

	// Public autoconfig / autodiscover endpoints (Phase 8). Mozilla
	// Thunderbird hits `/mail/config-v1.1.xml`; Outlook hits
	// `/autodiscover/autodiscover.xml`. These are intentionally
	// unauthenticated — IMAP / SMTP / CalDAV bootstrap happens
	// pre-login. Tenant settings come from `KMAIL_AUTOCONFIG_*`
	// env vars and the per-domain row in `domains`.
	autoconfigSvc := dns.NewAutoconfigService(dns.AutoconfigConfig{
		Pool:       pool,
		IMAPHost:   os.Getenv("KMAIL_AUTOCONFIG_IMAP_HOST"),
		IMAPPort:   config.GetenvInt("KMAIL_AUTOCONFIG_IMAP_PORT", 993),
		SMTPHost:   os.Getenv("KMAIL_AUTOCONFIG_SMTP_HOST"),
		SMTPPort:   config.GetenvInt("KMAIL_AUTOCONFIG_SMTP_PORT", 587),
		CalDAVHost: os.Getenv("KMAIL_AUTOCONFIG_CALDAV_HOST"),
		CalDAVPort: config.GetenvInt("KMAIL_AUTOCONFIG_CALDAV_PORT", 443),
		BaseURL:    os.Getenv("KMAIL_PUBLIC_BASE_URL"),
		BrandName:  os.Getenv("KMAIL_AUTOCONFIG_BRAND_NAME"),
	})
	dns.NewAutoconfigHandlers(autoconfigSvc, logger).Register(mux)

	migrationSvc := migration.NewService(migration.Config{
		Pool:             pool,
		StalwartAdminURL: cfg.StalwartURL,
		ImapsyncBin:      os.Getenv("KMAIL_IMAPSYNC_BIN"),
		MaxConcurrent:    config.GetenvInt("KMAIL_MIGRATION_MAX_CONCURRENT", 4),
	})
	migrationHandlers := migration.NewHandlers(migrationSvc, logger)
	migrationHandlers.Register(mux, authMW)

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

	// Free/busy publisher (Phase 8). Exposes
	// `/api/v1/calendars/{accountID}/{calendarID}/freebusy` for the
	// React UI plus the public `/.well-known/caldav` discovery
	// document and the CalDAV REPORT route external clients use.
	calendarbridge.NewFreeBusyHandlers(
		calendarbridge.NewFreeBusyService(calendarSvc),
		os.Getenv("KMAIL_PUBLIC_BASE_URL"),
		logger,
	).Register(mux, authMW)
	calendarSharingStore := calendarbridge.NewSharingStore(pool)
	calendarbridge.NewSharingHandlers(calendarSvc, calendarSharingStore).Register(mux, authMW)
	// Background reminder worker: polls upcoming events every 60s
	// and fires KChat reminders 15min / 5min before start.
	reminderWorker := calendarbridge.NewReminderWorker(pool, calendarSvc, calendarNotifier, valkeyClient, logger)
	if !disableWorkers {
		go reminderWorker.Run(ctx)
	}

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

	// SDK bootstrap (one-shot mailbox + email window + JMAP state
	// tokens for the native clients' first-launch hydration). The
	// internal JMAP client reuses the proxy's mTLS transport +
	// account-resolution cache so the cold-start request rate
	// matches the proxied path without double-charging Postgres on
	// every miss.
	internalJmap, err := jmap.NewInternalClient(proxy)
	if err != nil {
		logger.Fatalf("jmap.NewInternalClient: %v", err)
	}
	syncSvc, err := syncsvc.NewService(syncsvc.Config{
		Client: internalJmap,
		Logger: logger,
	})
	if err != nil {
		logger.Fatalf("sync.NewService: %v", err)
	}
	syncsvc.NewHandlers(syncSvc, logger).Register(mux, authMW)

	// Send-time interceptors (Undo Send / Scheduled Send). Each
	// is independently header-gated and degrades to immediate
	// submission when its backing store is unwired. They are
	// chained behind a single `Proxy.SetSendInterceptor` call so
	// future send-time features can register without touching
	// the proxy.
	var sendInterceptors []jmap.SendInterceptor

	// Undo Send (WS3). Holds outgoing EmailSubmission/set traffic
	// in Valkey for a configurable delay and dispatches to
	// Stalwart only after the window elapses, giving the user a
	// "Cancel" button on the Compose page. When Valkey is
	// unreachable the proxy falls through to immediate
	// submission (interceptor's Forwarder error path), so the
	// feature degrades gracefully rather than blocking sends.
	if valkeyClient != nil {
		undoDelay := time.Duration(config.GetenvInt("KMAIL_UNDO_SEND_DELAY_SECONDS", undosend.DefaultDelaySeconds)) * time.Second
		undoSvc, err := undosend.NewService(undosend.Config{
			Client: valkeyClient,
			Logger: logger,
			Delay:  undoDelay,
		})
		if err != nil {
			logger.Fatalf("undosend.NewService: %v", err)
		}
		undoHook, err := undosend.NewHook(undosend.HookConfig{
			Service:         undoSvc,
			Forwarder:       internalJmap,
			AccountResolver: proxy.ResolveAccountID,
			Logger:          logger,
		})
		if err != nil {
			logger.Fatalf("undosend.NewHook: %v", err)
		}
		sendInterceptors = append(sendInterceptors, undoHook)
		undosend.NewHandlers(undoSvc).Register(mux, authMW)
		undoWorker, err := undosend.NewDispatchWorker(undosend.WorkerConfig{
			Service:  undoSvc,
			Internal: internalJmap,
			Logger:   logger,
		})
		if err != nil {
			logger.Fatalf("undosend.NewDispatchWorker: %v", err)
		}
		if !disableWorkers {
			go undoWorker.Run(ctx)
		}
		logger.Printf("undosend: hold queue wired (delay=%s)", undoDelay)
	} else {
		logger.Printf("undosend: disabled (KMAIL_VALKEY_URL unset)")
	}

	// Scheduled Send (WS4). Persists future EmailSubmission/set
	// traffic in Postgres until `send_at` and dispatches via the
	// JMAP InternalClient. The DB pool is already required for
	// every other Service in this binary, so the feature is
	// always on (no env gate). If the pool is later made
	// optional the wiring degrades the same way undosend does.
	scheduledSvc, err := scheduledsend.NewService(scheduledsend.Config{
		Pool:   pool,
		Logger: logger,
	})
	if err != nil {
		logger.Fatalf("scheduledsend.NewService: %v", err)
	}
	scheduledHook, err := scheduledsend.NewHook(scheduledsend.HookConfig{
		Service:         scheduledSvc,
		Forwarder:       internalJmap,
		AccountResolver: proxy.ResolveAccountID,
		Logger:          logger,
	})
	if err != nil {
		logger.Fatalf("scheduledsend.NewHook: %v", err)
	}
	sendInterceptors = append(sendInterceptors, scheduledHook)
	scheduledsend.NewHandlers(scheduledSvc).Register(mux, authMW)
	scheduledInterval := getenvDuration("KMAIL_SCHEDULED_SEND_INTERVAL", 15*time.Second)
	scheduledWorker, err := scheduledsend.NewDispatchWorker(scheduledsend.WorkerConfig{
		Service:  scheduledSvc,
		Internal: internalJmap,
		Logger:   logger,
		Interval: scheduledInterval,
	})
	if err != nil {
		logger.Fatalf("scheduledsend.NewDispatchWorker: %v", err)
	}
	if !disableWorkers {
		go scheduledWorker.Run(ctx)
	}
	logger.Printf("scheduledsend: worker wired (interval=%s)", scheduledInterval)

	// Email Snooze (WS5). Hides an already-delivered email in a
	// per-user "Snoozed" mailbox until snooze_until, then patches
	// mailboxIds back to the originals via the JMAP InternalClient.
	// Symmetric with scheduledsend: durable Postgres queue, worker
	// with SKIP LOCKED, exponential backoff, dead-letter via
	// status='failed'. Wiring is always on (the DB pool is
	// required) — there is no env gate.
	snoozeSvc, err := snooze.NewService(snooze.Config{
		Pool:   pool,
		Logger: logger,
	})
	if err != nil {
		logger.Fatalf("snooze.NewService: %v", err)
	}
	snooze.NewHandlers(snoozeSvc, internalJmap, logger).Register(mux, authMW)
	snoozeInterval := getenvDuration("KMAIL_SNOOZE_INTERVAL", 30*time.Second)
	snoozeWorker, err := snooze.NewDispatchWorker(snooze.WorkerConfig{
		Service:  snoozeSvc,
		Internal: internalJmap,
		Logger:   logger,
		Interval: snoozeInterval,
	})
	if err != nil {
		logger.Fatalf("snooze.NewDispatchWorker: %v", err)
	}
	if !disableWorkers {
		go snoozeWorker.Run(ctx)
	}
	logger.Printf("snooze: worker wired (interval=%s)", snoozeInterval)

	// Workstream 7 — Smart Features & Intelligence.
	//
	// Rule-based smart replies, Gmail-style categorization, the
	// List-Unsubscribe helper, frequent-contact tracking, and the
	// Priority Inbox. All read-only/derived: the smart-features
	// surface reads mail via the JMAP InternalClient and stores its
	// ephemeral per-user state (contact frequency, unsubscribe
	// records, priority scores) in Valkey. When Valkey is unavailable
	// the contact/priority endpoints degrade to 503 rather than
	// taking down the rest of the API.
	smartFetcher, err := smartfeatures.NewJMAPFetcher(internalJmap)
	if err != nil {
		logger.Fatalf("smartfeatures.NewJMAPFetcher: %v", err)
	}
	var (
		contactTracker *smartfeatures.ContactTracker
		unsubStore     *smartfeatures.UnsubscribeStore
		priorityStore  *priority.Store
		priorityHist   priority.SendHistory
	)
	if valkeyClient != nil {
		if contactTracker, err = smartfeatures.NewContactTracker(valkeyClient, 0); err != nil {
			logger.Fatalf("smartfeatures.NewContactTracker: %v", err)
		}
		if unsubStore, err = smartfeatures.NewUnsubscribeStore(valkeyClient, 0); err != nil {
			logger.Fatalf("smartfeatures.NewUnsubscribeStore: %v", err)
		}
		if priorityStore, err = priority.NewStore(valkeyClient, 0); err != nil {
			logger.Fatalf("priority.NewStore: %v", err)
		}
		priorityHist = contactTracker
	}
	smartfeatures.NewHandlers(smartfeatures.HandlersConfig{
		Fetcher:  smartFetcher,
		Contacts: contactTracker,
		Unsub:    unsubStore,
		OneClick: smartfeatures.NewSafeOneClickUnsubscriber(0),
		Logger:   logger,
	}).Register(mux, authMW)

	prioritySource, err := priority.NewJMAPSource(internalJmap)
	if err != nil {
		logger.Fatalf("priority.NewJMAPSource: %v", err)
	}
	prioritySvc, err := priority.NewService(priority.Config{
		Source:  prioritySource,
		History: priorityHist,
		Store:   priorityStore,
		Logger:  logger,
	})
	if err != nil {
		logger.Fatalf("priority.NewService: %v", err)
	}
	priority.NewHandlers(prioritySvc, priorityStore, logger).Register(mux, authMW)

	analyticsSource, err := smartfeatures.NewJMAPAnalyticsSource(internalJmap)
	if err != nil {
		logger.Fatalf("smartfeatures.NewJMAPAnalyticsSource: %v", err)
	}
	smartfeatures.NewAnalyticsHandlers(analyticsSource, logger).Register(mux, authMW)
	logger.Printf("smartfeatures + priority inbox: handlers wired (valkey=%t)", valkeyClient != nil)

	if chained := jmap.ChainSendInterceptors(sendInterceptors...); chained != nil {
		proxy.SetSendInterceptor(chained)
	}

	// DKIM rotation surface (Phase 7). Lives next to the DNS
	// wizard so the wizard UI can show "rotation pending" rows
	// when an admin has rolled a new selector but DNS hasn't
	// caught up yet. The kmail-secrets envelope wraps freshly
	// generated private keys before they hit dkim_keys.
	dkimSvc := dns.NewDKIMRotationService(pool, logger)
	if secretsEnvelope != nil {
		dkimSvc = dkimSvc.WithEnvelope(secretsEnvelope)
	}
	dns.NewDKIMHandlers(dkimSvc, logger).Register(mux, authMW)

	// Search backend abstraction (Phase 7 + Phase 8). Meilisearch
	// is the default; OpenSearch is opt-in per-tenant via the
	// admin surface. The backend registry only contains the
	// backends configured via env so dev compose stays lean.
	//
	// Phase 8 adds the shared-index variants (one index per
	// Stalwart shard, tenant_id filter at query time). The
	// shared backends are wired off the same env vars as the
	// per-tenant ones and share the Stalwart shard resolver
	// (`shardSvc.GetTenantShardID`) for index-name derivation —
	// that keeps the index a tenant lands on aligned with the
	// shard their JMAP traffic already routes to.
	shardResolver := search.ShardResolverFunc(func(ctx context.Context, tenantID string) (string, error) {
		return shardSvc.GetTenantShardID(ctx, tenantID)
	})
	var searchBackends []search.SearchBackend
	var sharedInitBackends []search.SharedIndexEnsurer
	if url := os.Getenv("KMAIL_MEILISEARCH_URL"); url != "" {
		searchBackends = append(searchBackends, search.NewMeilisearchBackend(
			url, os.Getenv("KMAIL_MEILISEARCH_API_KEY"),
		))
		shared, err := search.NewSharedMeilisearchBackend(
			url, os.Getenv("KMAIL_MEILISEARCH_API_KEY"), shardResolver,
		)
		if err != nil {
			logger.Fatalf("search.NewSharedMeilisearchBackend: %v", err)
		}
		searchBackends = append(searchBackends, shared)
		sharedInitBackends = append(sharedInitBackends, shared)
	}
	if url := os.Getenv("KMAIL_OPENSEARCH_URL"); url != "" {
		searchBackends = append(searchBackends, search.NewOpenSearchBackend(
			url,
			os.Getenv("KMAIL_OPENSEARCH_USER"),
			os.Getenv("KMAIL_OPENSEARCH_PASS"),
		))
		shared, err := search.NewSharedOpenSearchBackend(
			url,
			os.Getenv("KMAIL_OPENSEARCH_USER"),
			os.Getenv("KMAIL_OPENSEARCH_PASS"),
			shardResolver,
		)
		if err != nil {
			logger.Fatalf("search.NewSharedOpenSearchBackend: %v", err)
		}
		searchBackends = append(searchBackends, shared)
		sharedInitBackends = append(sharedInitBackends, shared)
	}
	searchSvc := search.NewService(search.Config{
		Pool:     pool,
		Logger:   logger,
		Backends: searchBackends,
	})
	search.NewHandlers(searchSvc, logger).Register(mux, authMW)

	// Cutover plumbing. The Prometheus collectors are shared by
	// the operator-facing CutoverService (manual REST trigger)
	// and the background auto-cutover worker below, and attached
	// to the serving registry once `middleware.NewMetrics()` runs.
	cutoverMetrics := search.NewCutoverMetrics(nil)
	cutoverStore := search.NewPostgresCutoverStore(pool)
	cutoverSource := search.MessageSourceFunc(func(ctx context.Context, tenantID string) ([]search.Message, error) {
		return searchSvc.Export(ctx, tenantID)
	})
	cutoverSizer := search.MailboxSizerFunc(func(ctx context.Context, tenantID string) (int64, error) {
		q, err := billingSvc.GetQuota(ctx, tenantID)
		if err != nil {
			return 0, err
		}
		return q.StorageUsedBytes, nil
	})
	cutoverSvc, err := search.NewCutoverService(search.CutoverServiceConfig{
		Store:     cutoverStore,
		Flipper:   searchSvc,
		Source:    cutoverSource,
		Sizer:     cutoverSizer,
		Getter:    searchSvc,
		Audit:     auditSvc,
		Metrics:   cutoverMetrics,
		Logger:    logger,
		Threshold: int64(config.GetenvInt64("KMAIL_SEARCH_CUTOVER_THRESHOLD_BYTES", 0)),
	})
	if err != nil {
		logger.Fatalf("search.NewCutoverService: %v", err)
	}
	search.NewCutoverHandlers(cutoverSvc, logger).Register(mux, authMW)

	// Ensure every shared index exists with the correct
	// settings before the first per-tenant write lands. We do
	// this synchronously at startup so the admin / search
	// surface can return early errors rather than discovering a
	// missing index on the first SearchMessages call.
	// Failures inside `EnsureSharedIndexes` are per-(backend,
	// shard) and logged; only a fatal lister error aborts here,
	// in which case the BFF can still start. Both shared
	// backends (`shared_meilisearch` and `shared_opensearch`)
	// have lazy mapping/settings paths in their write methods,
	// so a missed-at-startup shard is created with the correct
	// mapping on its first IndexMessage / MigrateIndex call —
	// the BFF is degraded (latency on the first write) rather
	// than broken (wrong mapping breaking the tenant filter).
	//
	// On partial failure we emit a HIGH-SIGNAL aggregate log
	// line ("shared-indexes init partial-failure: N of M
	// pairs failed") in addition to the per-shard lines the
	// helper already wrote. This is the hook an operator wires
	// to a startup-health metric / alert — they don't have to
	// grep the per-shard noise to know if the fleet booted
	// cleanly.
	if err := search.EnsureSharedIndexes(ctx, logger, shardSvc, sharedInitBackends); err != nil {
		var agg *search.EnsureSharedIndexesError
		if errors.As(err, &agg) {
			logger.Printf("search.EnsureSharedIndexes: shared-indexes init partial-failure: %d of %d (backend, shard) pairs failed (continuing — lazy paths will repair on first write)",
				len(agg.Failures), agg.Attempted)
		} else {
			logger.Printf("search.EnsureSharedIndexes: %v (continuing — shared indexes will be created lazily on first write)", err)
		}
	}

	// Phase 5 / Phase 8: auto-cutover. Disabled when either
	// backend is missing (we'd have nowhere to read from or
	// write to). The worker now walks every configured
	// transition pair (meili->opensearch AND
	// shared_meili->shared_opensearch by default), so the
	// fleet's hot tenants move forward regardless of which
	// model they were provisioned under.
	//
	// We deliberately gate on the LEGACY backend names
	// (`BackendMeilisearch` / `BackendOpenSearch`) rather than
	// the four-name tuple of {meili, shared_meili, opensearch,
	// shared_opensearch}, because the wiring above at
	// `KMAIL_MEILISEARCH_URL` / `KMAIL_OPENSEARCH_URL`
	// CO-REGISTERS both the per-tenant AND the shared backend
	// variants for each URL. So:
	//   - `KMAIL_MEILISEARCH_URL` set  =>  hasMeili = true  AND
	//                                       BackendSharedMeilisearch
	//                                       is also wired
	//   - `KMAIL_OPENSEARCH_URL` set   =>  hasOpen = true  AND
	//                                       BackendSharedOpenSearch
	//                                       is also wired
	// The legacy-name guard is therefore a sufficient proxy
	// for "both transitions in `DefaultCutoverTransitions` have
	// valid Source AND Target backends registered." If a future
	// PR introduces a search backend that ISN'T URL-co-registered
	// (e.g. a hot-tier Meilisearch from its own env var), the
	// right move at that point is to switch this guard to a
	// per-transition predicate (`hasTransitionSource(tr)` /
	// `hasTransitionTarget(tr)`) walking
	// `DefaultCutoverTransitions` rather than counting backend
	// names. Devin Review round 8 (finding 3300377295) flagged
	// this implicit coupling — the explicit comment now makes
	// the URL-env-var invariant visible to the next reader so
	// they don't have to dig through the wiring block above to
	// understand why the two-name guard is correct.
	hasMeili, hasOpen := false, false
	for _, b := range searchBackends {
		switch b.Name() {
		case search.BackendMeilisearch:
			hasMeili = true
		case search.BackendOpenSearch:
			hasOpen = true
		}
	}
	if hasMeili && hasOpen {
		// Reuse the cutover store/source/sizer/metrics built with
		// the CutoverService above so the manual and automatic
		// paths share one store, one metric set, and one
		// mailbox-size source.
		cutover, cutErr := search.NewCutoverWorker(search.CutoverConfig{
			Store:       cutoverStore,
			Pool:        pool,
			Service:     searchSvc,
			Sizer:       cutoverSizer,
			Source:      cutoverSource,
			Logger:      logger,
			Audit:       auditSvc,
			Metrics:     cutoverMetrics,
			Threshold:   int64(config.GetenvInt64("KMAIL_SEARCH_CUTOVER_THRESHOLD_BYTES", 0)),
			Interval:    getenvDuration("KMAIL_SEARCH_CUTOVER_INTERVAL", time.Hour),
			MaxFailures: config.GetenvInt("KMAIL_SEARCH_CUTOVER_MAX_FAILURES", 5),
			MaxRetryGap: getenvDuration("KMAIL_SEARCH_CUTOVER_RETRY_GAP", time.Hour),
		})
		if cutErr != nil {
			logger.Fatalf("search.NewCutoverWorker: %v", cutErr)
		}
		if !disableWorkers {
			go cutover.Run(ctx)
		}
		logger.Printf("search: auto-cutover worker started (poll=%s)", getenvDuration("KMAIL_SEARCH_CUTOVER_INTERVAL", time.Hour))
	} else {
		logger.Printf("search: auto-cutover worker disabled (need both Meilisearch and OpenSearch configured)")
	}

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

	// TOTP fallback (Phase 8). PROPOSAL.md §10.1 specifies TOTP as
	// a fallback to FIDO2 — same identity surface, weaker assurance,
	// usable from any authenticator app. The shared secret is
	// wrapped by the kmail-secrets envelope; recovery codes are
	// SHA-256 hashed.
	// Brute-force lockout thresholds are env-tunable so operators can
	// tighten/loosen them without a rebuild; unset/invalid values fall
	// back to the conservative package defaults (5 attempts / 15m).
	middleware.NewTOTPHandlers(middleware.TOTPConfig{
		Pool:              pool,
		Logger:            logger,
		Issuer:            "KMail",
		Envelope:          secretsEnvelope,
		MaxFailedAttempts: config.GetenvInt("KMAIL_TOTP_MAX_FAILED_ATTEMPTS", 0),
		LockoutDuration:   getenvDuration("KMAIL_TOTP_LOCKOUT_DURATION", 0),
	}).Register(mux, authMW)

	// Shared-inbox workflow state machine. The service was built
	// up above (alongside the tenant Service) so the tenant
	// `WithSharedInboxMembershipHook` could route to it.
	sharedinbox.NewHandlers(sharedInboxWorkflowEarly, logger).Register(mux, authMW)

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
	// Attach the search cutover collectors to the serving registry
	// now that it exists (CutoverService / worker were constructed
	// earlier with a nil registry).
	cutoverMetrics.Register(metrics.Registry)
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
		if !disableWorkers {
			go worker.Run(ctx)
		}
	}

	// Deliverability alert evaluator: walks every tenant every
	// 15 minutes and raises alerts on threshold breaches.
	alertEvaluator := &deliverability.AlertEvaluator{
		Service:  deliverabilitySvc.Alerts,
		Pool:     pool,
		Interval: getenvDuration("KMAIL_ALERT_EVAL_INTERVAL", 15*time.Minute),
		Logger:   logger,
	}
	if !disableWorkers {
		go alertEvaluator.Run(ctx)
	}

	// Shard health worker: probes every registered Stalwart
	// shard every 60s and flips offline shards out of rotation.
	shardHealth := &tenant.HealthWorker{
		Service:  shardSvc,
		Interval: getenvDuration("KMAIL_SHARD_HEALTH_INTERVAL", 60*time.Second),
		Logger:   logger,
	}
	if !disableWorkers {
		go shardHealth.Run(ctx)
	}

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
	if !disableWorkers {
		go retentionWorker.Run(ctx)
	}

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
	// The kmail-secrets envelope wraps HSM connection credentials
	// (KMIP password, PKCS#11 PIN) at rest. We reuse the shared
	// `secretsEnvelope` loaded above so KMAIL_SECRETS_KEY is the
	// single master key for every BFF-side at-rest secret.
	cmkSvc := cmk.NewCMKServiceWithEnvelope(pool, secretsEnvelope)
	if secretsEnvelope == nil {
		logger.Printf("cmk: KMAIL_SECRETS_KEY not set — HSM credential registration will be refused (set the env var to enable BYOC HSM)")
	}
	cmk.NewHandlers(cmkSvc, pool, logger).Register(mux, authMW)

	// Phase 5 — Confidential Send portal. The public portal route
	// (`GET /api/v1/secure/{token}`) is registered *without* the
	// auth middleware by the handler; tenant-scoped admin routes
	// stay behind authMW.
	confidentialSendSvc := confidentialsend.NewService(pool).
		WithMLS(confidentialsend.NewHTTPKeyDeriver(cfg.KChatMLSEndpoint, cfg.KChatAPIToken))
	confidentialsend.NewHandlers(confidentialSendSvc, valkeyClient, logger).Register(mux, authMW)

	exportSvc := export.NewService(pool).WithAuditLogger(auditSvc)
	// Gap-closure Session 2: wire the real eDiscovery export
	// fan-out. The runner pulls full RFC 5322 messages through the
	// Session 0 jmap.EmailExporter, resolves scope via the
	// EmailOperator scope query, packages mbox/eml/pst_stub +
	// audit + calendar into a tar.gz, and streams it to the
	// tenant's zk-object-fabric bucket. Calendar / audit are
	// best-effort so the BFF keeps booting when a downstream
	// service is unreachable.
	exportAttachmentSvc := jmap.NewAttachmentService(jmap.AttachmentConfig{
		Pool:      pool,
		S3URL:     cfg.ZKFabric.S3URL,
		AccessKey: cfg.ZKFabric.AccessKey,
		SecretKey: cfg.ZKFabric.SecretKey,
	})
	exportExporter, err := jmap.NewStalwartEmailExporter(internalJmap, pool, logger)
	if err != nil {
		logger.Fatalf("jmap.NewStalwartEmailExporter: %v", err)
	}
	exportQuerier, err := jmap.NewStalwartEmailOperator(internalJmap, pool, logger)
	if err != nil {
		logger.Fatalf("jmap.NewStalwartEmailOperator: %v", err)
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
		logger.Fatalf("export.NewJMAPExportRunner: %v", err)
	}
	exportSvc.WithRunner(exportRunner)
	export.NewHandlers(exportSvc).Register(mux, authMW)
	// Register the export collectors unconditionally (matching the
	// retention worker above) so the API's /metrics surface stays
	// stable regardless of KMAIL_DISABLE_WORKERS — only the worker's
	// Run loop is gated. This avoids silently dropping kmail_export_*
	// from the API scrape target when workers run in the kmail-worker
	// process instead.
	exportMetrics := export.NewMetrics(metrics.Registry)
	if !disableWorkers {
		go export.NewWorker(exportSvc, logger).WithMetrics(exportMetrics).Run(ctx)
	}

	// Phase 5 closeout — SCIM 2.0 provisioning.
	scimSvc := scim.NewService(pool, tenantSvc)
	scim.NewHandlers(scimSvc, logger).Register(mux, authMW)

	// Phase 5 closeout — reverse access proxy for support /
	// SRE access to tenant data behind the existing approval
	// workflow.
	adminProxySvc := adminproxy.NewService(pool, approvalSvc, auditSvc, shardSvc)
	adminproxy.NewHandlers(adminProxySvc, logger, cfg.StalwartURL).Register(mux, authMW)

	// WS4 Task 1 — feature-flag system. Other workstreams gate their
	// rollouts on featureflags.IsEnabled, so the resolver is installed
	// as the process-wide default and kept fresh by a background
	// refresher. The refresher runs regardless of KMAIL_DISABLE_WORKERS
	// because flag resolution happens *in the API process* on the hot
	// request path — it is not a background job that belongs to
	// kmail-worker. The admin API (GET/PUT /api/v1/admin/feature-flags)
	// sits behind the OIDC middleware like the other admin surfaces.
	flagStore := featureflags.NewStore(pool)
	// Bound the control-plane read so a stalled Postgres fails fast
	// (retryable 503) instead of hanging the admin GET / resolver
	// refresh; the Service keeps serving its cached snapshot meanwhile.
	// KMAIL_FLAGS_READ_TIMEOUT overrides the default; "0" disables it
	// (e.g. when the pool enforces its own statement_timeout).
	if v := os.Getenv("KMAIL_FLAGS_READ_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			flagStore = flagStore.WithReadTimeout(d)
			logger.Printf("featureflags: control-plane read timeout set to %s", d)
		} else {
			logger.Printf("featureflags: ignoring invalid KMAIL_FLAGS_READ_TIMEOUT %q: %v", v, err)
		}
	}
	flagSvc := featureflags.NewStoreService(flagStore, logger)
	featureflags.SetDefault(flagSvc)
	flagHandlers := featureflags.NewHandlers(flagStore, flagSvc, logger)
	// During a Postgres outage the admin GET serves its last-known-good
	// snapshot (marked stale) instead of 503. By default that snapshot
	// is served for as long as the DB is down; KMAIL_FLAGS_MAX_STALE
	// caps its age, after which the read degrades to a retryable 503
	// rather than returning arbitrarily old data.
	if v := os.Getenv("KMAIL_FLAGS_MAX_STALE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			flagHandlers = flagHandlers.WithMaxStaleAge(d)
			logger.Printf("featureflags: max stale-serve age set to %s", d)
		} else {
			logger.Printf("featureflags: ignoring invalid KMAIL_FLAGS_MAX_STALE %q: %v", v, err)
		}
	}
	flagHandlers.Register(mux, authMW)
	go flagSvc.Run(ctx)
	// Phase 6: background watcher emits `session_expired` audit
	// rows once `expires_at` passes. Register the expiry collector
	// unconditionally (matching the retention and export workers
	// above) so the API's /metrics surface keeps exposing
	// kmail_admin_sessions_expired_total regardless of
	// KMAIL_DISABLE_WORKERS — only the worker's Run loop is gated.
	// Otherwise the metric silently vanishes from the API scrape
	// target when the expiry worker runs in the kmail-worker process.
	expiryWorker := adminproxy.NewExpiryWorker(pool, auditSvc, logger).
		WithMetric(metrics.Registry)
	if !disableWorkers {
		go expiryWorker.Run(ctx)
	}

	// Phase 5 closeout — CardDAV contact bridge.
	contactSvc := contactbridge.NewService(contactbridge.Config{StalwartURL: cfg.StalwartURL})
	galSvc := contactbridge.NewGALService(pool, contactSvc)
	contactbridge.NewHandlers(contactSvc, logger).WithGAL(galSvc).Register(mux, authMW)

	// Phase 5 closeout — Tenant webhook event system.
	webhookSvc := webhooks.NewService(pool)
	webhooks.NewHandlers(webhookSvc, logger).Register(mux, authMW)
	if !disableWorkers {
		go webhooks.NewWorker(webhookSvc, logger).Run(ctx)
	}

	// Phase E #14 — OAuth2 authorization server for third-party
	// integrations. Mounted at /api/v1/oauth/* (so all four
	// endpoints: authorize / authorize/approve / token / revoke
	// share a stable prefix). The user-JWT-aware resolver
	// extracts the consenting user from the OIDC token attached
	// by the BFF's existing auth chain; without it the consent
	// screen has nothing to bind the access token to.
	oauthSvc := oauth.NewService(pool)
	oauthHandlers := oauth.NewHandlers(oauthSvc, func(r *http.Request) (userID, tenantID string, ok bool) {
		// The consenting user is the kchat user authenticated by
		// the BFF's OIDC chain (middleware.OIDC). That chain
		// stashes (tenant_id, kchat_user_id) in the request
		// context; both must be present — an OAuth2 consent
		// without an authenticated user is meaningless.
		uid := middleware.KChatUserIDFrom(r.Context())
		tid := middleware.TenantIDFrom(r.Context())
		if uid == "" || tid == "" {
			return "", "", false
		}
		return uid, tid, true
	})
	// In production the consent CSRF cookie MUST be Secure; the
	// local-dev path serves plain HTTP so the Secure flag would
	// suppress the cookie on the redirect, breaking the
	// /authorize/approve POST with a CSRF mismatch.
	//
	// Resolution goes through `middleware.IsDevEnv(cfg.Env)`
	// (NOT a raw `KMAIL_ENV != "development"` string compare),
	// because `docker-compose.yml` ships `KMAIL_ENV: dev` and the
	// canonical alias table in `internal/middleware/auth.go`
	// (`envAliases`) maps `"dev" -> "development"`. A literal
	// comparison would treat the standard compose value as
	// production and set Secure: true on the CSRF cookie, which
	// the dev-mode browser would then refuse to send back over
	// plain HTTP — every consent screen would fail closed with a
	// 403. The OIDC middleware already routes dev / staging /
	// prod through this same helper; reusing it keeps the alias
	// surface in one place rather than spread across goroutines
	// of subtly-different env probes.
	oauthHandlers.SetSecureCookies(!middleware.IsDevEnv(cfg.Env))
	// Wire the prefixed (`kmail-api `) BFF logger into the OAuth2
	// handler set so its 5xx error paths land in the same
	// log-aggregation channel the rest of the BFF uses. Without
	// this, server-side failures in writeTokenError / the revoke
	// fallback would be emitted via `log.Default()` and miss the
	// `kmail-api ` prefix that structured log pipelines key on
	// for service-correlation — making OAuth2 outages harder to
	// triage than equivalent failures in jmap / cmk / webhooks.
	oauthHandlers.SetLogger(logger)
	// `RegisterRoutes` selectively wraps the two browser-facing
	// endpoints (`/authorize`, `/authorize/approve`) with the OIDC
	// middleware so the kchat-user context is populated before the
	// consent handlers read it via UserResolver. The two
	// machine-facing endpoints (`/token`, `/revoke`) are LEFT
	// UNWRAPPED because their callers authenticate as OAuth2
	// clients (client_id + client_secret), not as end users —
	// applying OIDC there would 401 every token exchange. See the
	// RegisterRoutes doc comment in internal/oauth/handlers.go for
	// the full split rationale.
	//
	// When iam-core is the active OIDC provider it owns the
	// authorization-server surface (login, consent, token issuance),
	// so KMail's own /api/v1/oauth authorize/token/revoke endpoints
	// are NOT mounted — running a second authorization server would
	// be a confusing, attackable duplicate. The oauthSvc itself is
	// still constructed: the integrations framework below validates
	// Bearer tokens KMail previously issued to installed third-party
	// apps, which is an API-gateway concern independent of where
	// end users log in. TOTP step-up endpoints (Confidential Send,
	// Vault unlock) are registered elsewhere and likewise remain
	// active regardless.
	if cfg.IAMCore.Enabled() {
		logger.Printf("iam-core: internal OAuth2 authorization server disabled (iam-core is the OIDC provider); /api/v1/oauth authorize/token/revoke not mounted")
	} else {
		oauthHandlers.RegisterRoutes(mux, "/api/v1/oauth", authMW.Wrap)
	}

	// Phase E #15-17 — Integration framework. Wraps webhookSvc
	// with OAuth2-client scope filtering and per-client rate-
	// limited dispatch. The boundary AuthMiddleware lives in
	// the oauth package (verifies Bearer token against
	// oauth_access_tokens, attaches AccessTokenContext).
	oauthAuthMW := oauth.NewAuthMiddleware(oauthSvc)
	// `limiterStore` is the SAME *RedisStore built once at the
	// top of main() for the auth rate limiter. Threading it
	// here (instead of calling NewRedisStore again) keeps the
	// process's Valkey connection budget bounded to a single
	// go-redis pool — see the limiterStore declaration for the
	// full rationale.
	integSvc, err := integrations.NewService(integrations.ServiceConfig{
		Pool:         pool,
		Webhooks:     webhookSvc,
		OAuth:        oauthSvc,
		LimiterStore: limiterStore,
		Logger:       logger,
	})
	if err != nil {
		logger.Fatalf("integrations: %v", err)
	}
	integrations.NewHandlers(integSvc, logger).Register(mux, oauthAuthMW)

	// Phase 5 closeout — Onboarding checklist.
	onboardingSvc := onboarding.NewService(pool)
	onboarding.NewHandlers(onboardingSvc, logger).Register(mux, authMW)
	// Phase 6: auto-complete onboarding steps from internal events.
	autoTriggerSvc := onboarding.NewAutoTriggerService(pool)
	webhookSvc.AddListener(autoTriggerSvc)

	// --- Self-service tenant signup (gap-closure Session 3) ---
	// Public signup funnel: POST /api/v1/signup mints a Stripe
	// Checkout Session; the checkout.session.completed webhook drives
	// CompleteSignup to provision the tenant idempotently. Wired here
	// because it composes the tenant, onboarding, audit, DNS, and
	// JMAP services assembled above.
	signupMetrics := tenant.NewSignupMetrics(metrics.Registry)
	signupPlanPrices := map[string]string{
		"core":    os.Getenv("KMAIL_STRIPE_PRICE_CORE"),
		"pro":     os.Getenv("KMAIL_STRIPE_PRICE_PRO"),
		"privacy": os.Getenv("KMAIL_STRIPE_PRICE_PRIVACY"),
	}
	var signupCheckout tenant.StripeCheckoutClient
	if k := os.Getenv("KMAIL_STRIPE_SECRET_KEY"); k != "" {
		signupCheckout = &tenant.StripeCheckoutHTTP{
			APIKey:  k,
			BaseURL: os.Getenv("KMAIL_STRIPE_API_BASE"),
			// Dedicated client with a hard timeout rather than
			// http.DefaultClient (which has none). The per-request
			// context already cancels on client disconnect / shutdown,
			// but http.Server applies no default handler timeout, so a
			// hung Stripe connection could otherwise pin a goroutine
			// indefinitely. This is the backstop independent of context
			// propagation.
			HTTP: &http.Client{Timeout: 30 * time.Second},
		}
	}
	signupWelcomeMailer := tenant.NewJMAPWelcomeMailer(
		internalJmap,
		os.Getenv("KMAIL_SIGNUP_WELCOME_TENANT"),
		os.Getenv("KMAIL_SIGNUP_WELCOME_USER"),
		os.Getenv("KMAIL_SIGNUP_WELCOME_FROM"),
		os.Getenv("KMAIL_SIGNUP_WELCOME_IDENTITY"),
		logger,
	)
	signupSvc := tenant.NewSignupService(tenant.SignupConfig{
		Repo:        tenant.NewSignupRepository(pool),
		Provisioner: tenant.NewSignupProvisioner(tenantSvc, pool),
		Stripe:      signupCheckout,
		Checklist: tenant.NewChecklistAdapter(func(ctx context.Context, tid string) error {
			_, err := onboardingSvc.GetChecklist(ctx, tid)
			return err
		}),
		Audit:         auditSvc,
		Mailer:        signupWelcomeMailer,
		DNS:           dnsSvc,
		Metrics:       signupMetrics,
		PlanPrices:    signupPlanPrices,
		PublicBaseURL: os.Getenv("KMAIL_PUBLIC_BASE_URL"),
		Logger:        logger,
	})
	// Number of trusted reverse proxies in front of the API so the signup
	// rate limiter reads the real client IP without trusting a spoofable
	// X-Forwarded-For prefix. Defaults to 1 (a single Kubernetes ingress);
	// set to 0 (or negative) to declare the API directly internet-facing
	// and ignore X-Forwarded-For entirely.
	signupTrustedProxyDepth := config.GetenvInt("KMAIL_SIGNUP_TRUSTED_PROXY_DEPTH", 1)
	tenant.NewSignupHandlers(tenant.SignupHandlersConfig{
		Service:           signupSvc,
		Limiter:           limiterStore,
		Metrics:           signupMetrics,
		Logger:            logger,
		TrustedProxyDepth: &signupTrustedProxyDepth,
	}).Register(mux)
	stripeWebhook.SetSignupCompleter(signupSvc)

	// Wire metrics and tracing into the outer handler chain.
	handler := http.Handler(mux)
	if cfg.Observability.TracingEnabled {
		handler = middleware.TracingMiddleware(handler)
	}
	if cfg.Observability.MetricsEnabled {
		handler = metrics.Middleware(handler)
	}
	handler = middleware.RequestLogger(logger, cfg.Observability.LogFormat)(handler)

	// Assign/propagate a correlation id BEFORE the logger (wrapped
	// below so it runs first) so every request log line — and any
	// downstream layer — shares one X-Request-Id.
	handler = middleware.RequestID(handler)

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

	// The feature-flag admin handlers kick post-write cache
	// reconciliations off the response path, so one can still be running
	// after Shutdown drained the request goroutines. Wait for any
	// in-flight reconcile to finish (bounded by the same shutdown
	// deadline) so it isn't orphaned on exit.
	if err := flagHandlers.Drain(shutdownCtx); err != nil {
		logger.Printf("featureflags: reconcile drain did not finish before shutdown deadline: %v", err)
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
// logging transport via the router's Default, which is what we
// want in dev — the router otherwise returns an error for any
// device type that has no transport configured.
func buildPushTransport(logger *log.Logger) push.Transport {
	router := push.NewTransportRouter(logger)
	// Default transport is the logging transport: any device type
	// (web, plus iOS/Android in dev) without a platform-specific
	// transport falls through to it. Web push has no real backend
	// yet — when one ships it will be assigned to router.Web and
	// only unrecognized device types will hit Default.
	router.Default = push.NewLoggingTransport(logger)
	router.Web = router.Default
	if pub, priv := os.Getenv("KMAIL_VAPID_PUBLIC_KEY"), os.Getenv("KMAIL_VAPID_PRIVATE_KEY"); pub != "" && priv != "" {
		webpush, err := push.NewWebPushFromKeys(pub, priv, os.Getenv("KMAIL_VAPID_SUBJECT"), logger)
		if err != nil {
			logger.Printf("web push transport disabled: %v", err)
		} else {
			router.Web = webpush
		}
	}
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
