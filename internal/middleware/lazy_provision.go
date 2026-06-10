package middleware

// Lazy tenant provisioning for the iam-core integration.
//
// When KMail trusts iam-core as its OIDC issuer, a user can present
// a valid token for a tenant whose KMail control-plane row does not
// exist yet — e.g. the `tenant.create` webhook was lost, delayed,
// or never configured. Rather than 500-ing inside a handler that
// assumes the tenant exists, this middleware provisions it on the
// first authenticated request and then lets the request proceed.
//
// It is strictly opt-in (KMAIL_IAM_CORE_LAZY_PROVISION=true). Left
// disabled, KMail relies solely on webhook-driven provisioning.

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// TenantProvisioner is the narrow control-plane slice the lazy
// provisioning middleware drives. It is declared here, rather than
// importing internal/tenant, because internal/tenant already
// imports this package — depending on it the other way would create
// an import cycle. cmd/kmail-api adapts *tenant.Service onto this
// interface.
type TenantProvisioner interface {
	// EnsureTenant idempotently provisions the KMail control-plane
	// tenant identified by tenantID. Implementations MUST be safe to
	// call concurrently and repeatedly: lazy provisioning races
	// webhook-driven provisioning and fires on every uncached
	// request.
	EnsureTenant(ctx context.Context, tenantID string) error
}

// provisionCacheKeyPrefix namespaces the Valkey keys recording that
// a tenant was recently confirmed provisioned, so a hit lets the
// middleware skip the EnsureTenant round-trip.
const provisionCacheKeyPrefix = "kmail:iamcore:tenant-provisioned:"

// defaultProvisionCacheTTL bounds how long a tenant is assumed
// provisioned without re-checking. 5 minutes matches the rest of
// KMail's Valkey-backed middleware caches: long enough to keep the
// hot path off the control-plane DB, short enough that a tenant
// deleted out-of-band is re-validated promptly.
const defaultProvisionCacheTTL = 5 * time.Minute

// LazyProvisionConfig wires the middleware.
type LazyProvisionConfig struct {
	// Provisioner performs the idempotent tenant provision.
	// Required — NewLazyProvision panics if nil, since a misconfigured
	// provisioner would silently disable provisioning.
	Provisioner TenantProvisioner

	// Cache is the Valkey client used to remember recently
	// provisioned tenants. Optional: when nil the middleware still
	// works correctly but calls EnsureTenant on every request
	// (EnsureTenant's own SELECT fast-path keeps that cheap).
	Cache *redis.Client

	// CacheTTL overrides defaultProvisionCacheTTL when > 0.
	CacheTTL time.Duration

	// Logger receives provisioning failures. Defaults to log.Default().
	Logger *log.Logger
}

// LazyProvision is the configured middleware. Construct it with
// NewLazyProvision and mount it with Wrap after the OIDC Wrap so the
// authenticated tenant id is present in the request context.
type LazyProvision struct {
	provisioner TenantProvisioner
	cache       *redis.Client
	ttl         time.Duration
	logger      *log.Logger
}

// NewLazyProvision builds the middleware. It panics when no
// Provisioner is supplied — wiring it without one is a programming
// error, not a runtime condition to fail open on.
func NewLazyProvision(cfg LazyProvisionConfig) *LazyProvision {
	if cfg.Provisioner == nil {
		panic("middleware.NewLazyProvision: Provisioner is required")
	}
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = defaultProvisionCacheTTL
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &LazyProvision{
		provisioner: cfg.Provisioner,
		cache:       cfg.Cache,
		ttl:         ttl,
		logger:      logger,
	}
}

// Wrap returns middleware that ensures the authenticated tenant is
// provisioned before the request reaches the handler.
//
// Design choices:
//   - It must run AFTER OIDC Wrap: it reads the tenant id from the
//     context OIDC populated. With no tenant id (unauthenticated /
//     middleware not chained) it is a pass-through.
//   - A Valkey cache hit short-circuits the provision so the common
//     case (already-provisioned tenant) adds at most one Valkey GET.
//   - It fails OPEN: if EnsureTenant errors, the request still
//     proceeds and the failure is logged. Provisioning is a
//     convenience for webhook gaps — a transient control-plane
//     hiccup should not turn every request into a 500. A handler
//     that genuinely needs the tenant row will surface its own
//     error. The cache is only written on success, so a failed
//     provision is retried on the next request rather than masked
//     for the full TTL.
func (l *LazyProvision) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.Handle(w, r, next)
	})
}

// Handle runs the provisioning step for a single request and then
// invokes next. It is the per-request body of Wrap, exposed
// separately so callers that compose middleware via a stable
// closure (cmd/kmail-api wires this through OIDCConfig.PostAuth-
// Middleware, whose target is resolved after the tenant Service is
// constructed) can drive provisioning without allocating a fresh
// http.Handler on every request.
func (l *LazyProvision) Handle(w http.ResponseWriter, r *http.Request, next http.Handler) {
	ctx := r.Context()
	tenantID := TenantIDFrom(ctx)
	if tenantID == "" {
		next.ServeHTTP(w, r)
		return
	}
	if l.isCachedProvisioned(ctx, tenantID) {
		next.ServeHTTP(w, r)
		return
	}
	if err := l.provisioner.EnsureTenant(ctx, tenantID); err != nil {
		l.logger.Printf("iamcore lazy-provision: EnsureTenant(%q) failed, serving request anyway: %v", tenantID, err)
	} else {
		l.markCachedProvisioned(ctx, tenantID)
	}
	next.ServeHTTP(w, r)
}

func (l *LazyProvision) cacheKey(tenantID string) string {
	return provisionCacheKeyPrefix + tenantID
}

// isCachedProvisioned reports whether the tenant was recently
// confirmed provisioned. Any Valkey error (including a nil cache) is
// treated as a miss so provisioning still runs — the cache is a
// pure optimisation, never a correctness dependency.
func (l *LazyProvision) isCachedProvisioned(ctx context.Context, tenantID string) bool {
	if l.cache == nil {
		return false
	}
	n, err := l.cache.Exists(ctx, l.cacheKey(tenantID)).Result()
	if err != nil {
		return false
	}
	return n > 0
}

// markCachedProvisioned records a successful provision. A write
// failure is logged at most implicitly (ignored) because the only
// consequence is an extra EnsureTenant call next request, which is
// idempotent and cheap.
func (l *LazyProvision) markCachedProvisioned(ctx context.Context, tenantID string) {
	if l.cache == nil {
		return
	}
	// Best-effort: ignore the error. A failed SET only costs one
	// extra (idempotent) EnsureTenant on the next request.
	_ = l.cache.Set(ctx, l.cacheKey(tenantID), "1", l.ttl).Err()
}
