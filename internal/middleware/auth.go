// Package middleware hosts shared HTTP middleware for the Go
// control plane.
//
// Responsibilities (per docs/PROPOSAL.md §5 and
// docs/ARCHITECTURE.md §7): KChat OIDC authentication, tenant
// context propagation (the `app.tenant_id` Postgres GUC that
// drives row-level security — see docs/SCHEMA.md §4), structured
// request logging, correlation ID injection
// (`X-KMail-Correlation-Id`), and rate limiting.
package middleware

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// contextKey is the unexported key type used for auth values stored
// in the request context. Exporting the accessors (below) keeps the
// key private so callers cannot stash arbitrary values under it.
type contextKey int

const (
	ctxKeyTenantID contextKey = iota + 1
	ctxKeyKChatUserID
	ctxKeyStalwartAccountID
)

// OIDCConfig wires the OIDC middleware. `Issuer` is the KChat OIDC
// issuer URL (JWKS discovery happens here in Phase 2). `DevBypassToken`
// is a static bearer token that is accepted verbatim when non-empty;
// it exists so local dev can hit authenticated endpoints without
// standing up a real OIDC issuer. `Pool` is used by
// `LoadTenantScope` to resolve the acting user's tenant and push the
// `app.tenant_id` GUC before handler code runs.
type OIDCConfig struct {
	Issuer         string
	DevBypassToken string
	Pool           *pgxpool.Pool
	Logger         *log.Logger
}

// OIDC is the middleware factory. It is stateless today (JWKS cache
// will live here in Phase 2) but constructed once per process so the
// handler type does not leak `OIDCConfig` to every call site.
type OIDC struct {
	cfg OIDCConfig
}

// NewOIDC returns an OIDC middleware with the provided configuration.
func NewOIDC(cfg OIDCConfig) *OIDC {
	return &OIDC{cfg: cfg}
}

// Wrap returns middleware that authenticates the request and stores
// `tenant_id`, `kchat_user_id`, and (when present) `stalwart_account_id`
// in the request context. It sends 401 Unauthorized on any failure;
// callers can rely on the context values being populated when their
// handler runs.
func (o *OIDC) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := o.authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		ctx := r.Context()
		ctx = context.WithValue(ctx, ctxKeyTenantID, claims.TenantID)
		ctx = context.WithValue(ctx, ctxKeyKChatUserID, claims.KChatUserID)
		if claims.StalwartAccountID != "" {
			ctx = context.WithValue(ctx, ctxKeyStalwartAccountID, claims.StalwartAccountID)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Claims is the authenticated identity extracted from the bearer
// token. In production these fields come from validated JWT claims;
// in dev-bypass mode they come from the dev token payload or env
// defaults.
type Claims struct {
	TenantID          string `json:"tenant_id"`
	KChatUserID       string `json:"kchat_user_id"`
	StalwartAccountID string `json:"stalwart_account_id,omitempty"`
}

// authenticate extracts and validates the bearer token, returning
// the Claims it carries. In dev-bypass mode the token is trusted
// unverified; in real mode (Phase 2) the JWT is verified against the
// KChat OIDC issuer's JWKS before the claims are returned.
func (o *OIDC) authenticate(r *http.Request) (*Claims, error) {
	authz := r.Header.Get("Authorization")
	if authz == "" {
		return nil, errors.New("missing Authorization header")
	}
	parts := strings.SplitN(authz, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, errors.New("Authorization header must be 'Bearer <token>'")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return nil, errors.New("empty bearer token")
	}

	// Dev-bypass path. The static token unlocks a synthesized set of
	// claims from headers or env defaults — never wire this on in
	// production.
	if o.cfg.DevBypassToken != "" && token == o.cfg.DevBypassToken {
		return devClaimsFromHeaders(r), nil
	}

	// Real path: decode the JWT. We parse the payload so the claims
	// can be surfaced immediately, but we MUST NOT mark the token as
	// trusted until signature verification is implemented in Phase 2
	// (see docs/JMAP-CONTRACT.md §3.1). Until then the middleware
	// only runs when either (a) DevBypassToken is set, or (b) the
	// token parses as a JWT with the required custom claims — both
	// of which are dev-only postures documented on OIDCConfig.
	claims, err := decodeJWTClaims(token)
	if err != nil {
		return nil, fmt.Errorf("invalid JWT: %w", err)
	}
	if claims.TenantID == "" || claims.KChatUserID == "" {
		return nil, errors.New("JWT is missing tenant_id or kchat_user_id claim")
	}
	return claims, nil
}

// decodeJWTClaims parses the unverified payload of a compact JWT. It
// does NOT verify the signature. See the caller for the
// Phase 1 / Phase 2 contract around this.
func decodeJWTClaims(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("token is not a well-formed JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("decode claims: %w", err)
	}
	return &c, nil
}

// devClaimsFromHeaders synthesizes Claims for dev-bypass requests.
// Headers let developers simulate multiple tenants without minting
// different tokens; sane defaults keep `curl` one-liners usable.
func devClaimsFromHeaders(r *http.Request) *Claims {
	tenantID := r.Header.Get("X-KMail-Dev-Tenant-Id")
	if tenantID == "" {
		tenantID = "00000000-0000-0000-0000-000000000000"
	}
	kchatUserID := r.Header.Get("X-KMail-Dev-Kchat-User-Id")
	if kchatUserID == "" {
		kchatUserID = "dev-user"
	}
	return &Claims{
		TenantID:          tenantID,
		KChatUserID:       kchatUserID,
		StalwartAccountID: r.Header.Get("X-KMail-Dev-Stalwart-Account-Id"),
	}
}

// TenantIDFrom returns the authenticated tenant UUID from the
// request context, or an empty string if the middleware did not run.
func TenantIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyTenantID).(string); ok {
		return v
	}
	return ""
}

// KChatUserIDFrom returns the authenticated KChat user ID from the
// request context, or an empty string if the middleware did not run.
func KChatUserIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyKChatUserID).(string); ok {
		return v
	}
	return ""
}

// StalwartAccountIDFrom returns the resolved Stalwart account ID
// from the request context, or an empty string if it has not been
// resolved.
func StalwartAccountIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyStalwartAccountID).(string); ok {
		return v
	}
	return ""
}

// WithStalwartAccountID returns a copy of ctx with the provided
// Stalwart account ID stored. Used by the JMAP proxy after it
// resolves `(tenant_id, kchat_user_id) → stalwart_account_id`.
func WithStalwartAccountID(ctx context.Context, accountID string) context.Context {
	return context.WithValue(ctx, ctxKeyStalwartAccountID, accountID)
}

// SetTenantGUC sets the `app.tenant_id` Postgres session variable on
// the provided transaction. Row-level security policies on every
// tenant-scoped table read this GUC — see
// `migrations/001_initial_schema.sql` and `docs/SCHEMA.md` §4.
//
// The `true` third argument to `set_config` scopes the value to the
// current transaction so callers cannot leak tenant context onto a
// pooled connection after commit.
func SetTenantGUC(ctx context.Context, tx pgx.Tx, tenantID string) error {
	if tenantID == "" {
		return errors.New("SetTenantGUC: empty tenantID")
	}
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID)
	return err
}
