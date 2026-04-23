// Package jmap hosts the Go BFF's JMAP proxy: speaks JMAP to
// Stalwart on behalf of the React client, translates KChat OIDC
// auth into Stalwart auth, enforces tenant policy, and manages
// capability negotiation.
//
// See `docs/JMAP-CONTRACT.md` for the contract this package
// implements against, and `docs/ARCHITECTURE.md` §7 for the Go
// service topology.
package jmap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ProxyConfig wires the JMAP reverse proxy. `StalwartURL` is the
// internal Stalwart JMAP endpoint (e.g., `http://stalwart:8080` in
// the local compose stack). `Pool` is used to resolve the acting
// user's Stalwart account ID per
// `docs/JMAP-CONTRACT.md` §3.3. `Logger` is optional; if nil, a
// logger writing to the default output is used.
type ProxyConfig struct {
	StalwartURL string
	Pool        *pgxpool.Pool
	Logger      *log.Logger

	// AccountCacheTTL controls how long the `(tenant_id, kchat_user_id)
	// → stalwart_account_id` cache entries live. Defaults to 5
	// minutes per `docs/JMAP-CONTRACT.md` §3.3.
	AccountCacheTTL time.Duration
}

// Proxy forwards authenticated JMAP requests from the React client
// to Stalwart, injecting the acting user's Stalwart account ID
// (resolved and cached from Postgres) into the `X-KMail-Stalwart-Account-Id`
// header for the downstream.
//
// In Phase 1 the proxy does not mint the Stalwart-trusted internal
// OIDC token documented in `docs/JMAP-CONTRACT.md` §3.2 — that
// signing-key dance lands in Phase 2. The header-based account
// identification is a deliberate placeholder that the upstream
// Stalwart config pairs with a trusted-network rule in local dev.
type Proxy struct {
	cfg     ProxyConfig
	rp      *httputil.ReverseProxy
	logger  *log.Logger
	cache   *accountCache
	target  *url.URL
	stripPR string
}

// NewProxy builds a Proxy pointed at the configured Stalwart URL.
func NewProxy(cfg ProxyConfig) (*Proxy, error) {
	if cfg.StalwartURL == "" {
		return nil, errors.New("jmap.NewProxy: StalwartURL is required")
	}
	if cfg.Pool == nil {
		return nil, errors.New("jmap.NewProxy: Pool is required")
	}
	target, err := url.Parse(cfg.StalwartURL)
	if err != nil {
		return nil, fmt.Errorf("jmap.NewProxy: parse StalwartURL: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	ttl := cfg.AccountCacheTTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	p := &Proxy{
		cfg:     cfg,
		logger:  logger,
		cache:   newAccountCache(ttl),
		target:  target,
		stripPR: "/jmap",
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite:      p.rewrite,
		ErrorHandler: p.errorHandler,
	}
	return p, nil
}

// ServeHTTP implements http.Handler. It expects to run behind the
// OIDC middleware: the acting tenant and KChat user are read from
// the request context. Missing context values result in 500 because
// the caller wired the mux incorrectly — 401 would hide the bug.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.Context())
	if tenantID == "" || kchatUserID == "" {
		http.Error(w, "jmap proxy: missing tenant or user context (OIDC middleware not wired)", http.StatusInternalServerError)
		return
	}

	accountID, err := p.resolveAccount(r.Context(), tenantID, kchatUserID)
	if err != nil {
		w.Header().Set("Content-Type", "application/problem+json")
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// An unresolved account is expected while the Tenant Service
			// has not yet provisioned the user; surface it as 404 with a
			// JMAP-compatible error shape.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"type":"urn:ietf:params:jmap:error:accountNotFound","title":"stalwart account not provisioned"}` + "\n"))
		default:
			// Infrastructure failures (Postgres outage, pool exhaustion,
			// GUC errors, context cancellation, etc.) surface as 502 so
			// on-call doesn't chase a spurious "not provisioned" signal.
			p.logger.Printf("jmap proxy resolveAccount err tenant=%s kchat_user=%s err=%v", tenantID, kchatUserID, err)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"type":"urn:ietf:params:jmap:error:serverUnavailable","title":"account lookup failed"}` + "\n"))
		}
		return
	}

	ctx := middleware.WithStalwartAccountID(r.Context(), accountID)
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// rewrite adapts the incoming request to the upstream Stalwart URL.
// It strips the `/jmap` prefix so clients can hit `/jmap/session`
// and Stalwart sees `/session`, and injects the resolved Stalwart
// account ID as a header the upstream can trust in internal-network
// deployments.
func (p *Proxy) rewrite(r *httputil.ProxyRequest) {
	accountID := middleware.StalwartAccountIDFrom(r.In.Context())
	tenantID := middleware.TenantIDFrom(r.In.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.In.Context())

	r.SetURL(p.target)
	r.Out.Host = p.target.Host

	// Strip the `/jmap` prefix from the outgoing path so the
	// upstream sees the JMAP path it actually implements. Leave
	// trailing `/` and deeper paths intact. Clear RawPath so
	// net/url regenerates it from the rewritten Path — otherwise a
	// non-empty RawPath (set whenever the incoming URL contained
	// percent-encoded bytes) would win inside RequestURI() and the
	// upstream would still see `/jmap`.
	outPath := r.Out.URL.Path
	if strings.HasPrefix(outPath, p.stripPR) {
		trimmed := strings.TrimPrefix(outPath, p.stripPR)
		if trimmed == "" {
			trimmed = "/"
		}
		r.Out.URL.Path = trimmed
		r.Out.URL.RawPath = ""
	}

	r.Out.Header.Set("X-KMail-Tenant-Id", tenantID)
	r.Out.Header.Set("X-KMail-Kchat-User-Id", kchatUserID)
	if accountID != "" {
		r.Out.Header.Set("X-KMail-Stalwart-Account-Id", accountID)
	}
	// Stalwart's JMAP is authoritative for its own auth; the BFF's
	// Phase 1 posture is trusted-network only (see package doc).
	r.Out.Header.Del("Authorization")
}

// errorHandler maps upstream failures into BFF-visible errors per
// `docs/JMAP-CONTRACT.md` §7.
func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	p.logger.Printf("jmap proxy upstream error path=%s err=%v", r.URL.Path, err)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusBadGateway)
	_, _ = w.Write([]byte(`{"type":"urn:ietf:params:jmap:error:serverUnavailable","title":"upstream unavailable"}` + "\n"))
}

// resolveAccount returns the Stalwart account ID for the given
// (tenant_id, kchat_user_id) pair, preferring the in-process cache.
// Cache misses go to Postgres and populate the cache.
func (p *Proxy) resolveAccount(ctx context.Context, tenantID, kchatUserID string) (string, error) {
	if accountID, ok := p.cache.get(tenantID, kchatUserID); ok {
		return accountID, nil
	}

	var accountID string
	err := pgx.BeginFunc(ctx, p.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return fmt.Errorf("set tenant GUC: %w", err)
		}
		row := tx.QueryRow(ctx, `
			SELECT stalwart_account_id
			FROM users
			WHERE tenant_id = $1::uuid AND kchat_user_id = $2
		`, tenantID, kchatUserID)
		return row.Scan(&accountID)
	})
	if err != nil {
		return "", err
	}
	p.cache.set(tenantID, kchatUserID, accountID)
	return accountID, nil
}

// accountCache is a TTL'd in-process cache for the
// `(tenant_id, kchat_user_id) → stalwart_account_id` mapping. It is
// deliberately simple; the Valkey-backed shared cache (10 000 entries,
// 5 min TTL) documented in `docs/JMAP-CONTRACT.md` §3.3 lands in
// Phase 2.
type accountCache struct {
	ttl time.Duration
	mu  sync.RWMutex
	m   map[string]accountCacheEntry
}

type accountCacheEntry struct {
	accountID string
	expiresAt time.Time
}

func newAccountCache(ttl time.Duration) *accountCache {
	return &accountCache{ttl: ttl, m: map[string]accountCacheEntry{}}
}

func (c *accountCache) key(tenantID, kchatUserID string) string {
	return tenantID + "|" + kchatUserID
}

func (c *accountCache) get(tenantID, kchatUserID string) (string, bool) {
	k := c.key(tenantID, kchatUserID)
	c.mu.RLock()
	entry, ok := c.m[k]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if time.Now().After(entry.expiresAt) {
		// Drop the expired entry eagerly so callers that only hit
		// stale keys don't accumulate map entries forever.
		c.mu.Lock()
		if cur, still := c.m[k]; still && !time.Now().Before(cur.expiresAt) {
			delete(c.m, k)
		}
		c.mu.Unlock()
		return "", false
	}
	return entry.accountID, true
}

func (c *accountCache) set(tenantID, kchatUserID, accountID string) {
	c.mu.Lock()
	c.m[c.key(tenantID, kchatUserID)] = accountCacheEntry{
		accountID: accountID,
		expiresAt: time.Now().Add(c.ttl),
	}
	c.mu.Unlock()
}
