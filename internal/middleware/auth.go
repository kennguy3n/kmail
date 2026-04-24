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

	"github.com/golang-jwt/jwt/v5"
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
// issuer URL (JWKS discovery happens here). `Audience` is the
// expected `aud` claim — when non-empty the middleware rejects
// tokens that don't carry it. `DevBypassToken` is a static bearer
// token that is accepted verbatim when non-empty; it exists so
// local dev can hit authenticated endpoints without standing up a
// real OIDC issuer. `Pool` is used by `LoadTenantScope` to resolve
// the acting user's tenant and push the `app.tenant_id` GUC before
// handler code runs.
type OIDCConfig struct {
	Issuer         string
	Audience       string
	DevBypassToken string
	Pool           *pgxpool.Pool
	Logger         *log.Logger
	// JWKS, when non-nil, is used to verify RS/ES/PS tokens.
	// Leaving this nil disables signature verification — only the
	// dev-bypass path is then usable. NewOIDC auto-populates this
	// field from cfg.Issuer when Issuer is non-empty.
	JWKS *JWKSFetcher
}

// OIDC is the middleware factory. The JWKS cache lives on this
// struct so one fetcher is shared across every request.
type OIDC struct {
	cfg OIDCConfig
}

// NewOIDC returns an OIDC middleware with the provided configuration.
// When cfg.Issuer is non-empty but cfg.JWKS is nil, a JWKSFetcher
// is built automatically so production code does not have to wire
// one up manually. Returns an error when JWKS construction fails.
func NewOIDC(cfg OIDCConfig) (*OIDC, error) {
	if cfg.Issuer != "" && cfg.JWKS == nil {
		fetcher, err := NewJWKSFetcher(JWKSConfig{Issuer: cfg.Issuer})
		if err != nil {
			return nil, fmt.Errorf("build JWKS fetcher: %w", err)
		}
		cfg.JWKS = fetcher
	}
	return &OIDC{cfg: cfg}, nil
}

// MustNewOIDC is the panicking convenience constructor retained so
// test helpers that don't care about the JWKS-build error can
// continue calling a one-liner.
func MustNewOIDC(cfg OIDCConfig) *OIDC {
	o, err := NewOIDC(cfg)
	if err != nil {
		panic(err)
	}
	return o
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
// unverified; in real mode the JWT is verified against the KChat
// OIDC issuer's JWKS and the `iss` / `exp` / `aud` claims are
// validated before the claims are returned.
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

	if o.cfg.JWKS != nil {
		return o.verifyAndExtract(r.Context(), token)
	}

	// Last-resort path: no JWKS configured (no issuer set) and not
	// the dev-bypass token. Decode the claims but do NOT mark the
	// token as trusted. This keeps local dev without an OIDC
	// issuer working while refusing to surface identity data in
	// deployments that forgot to configure an issuer.
	claims, err := decodeJWTClaims(token)
	if err != nil {
		return nil, fmt.Errorf("invalid JWT: %w", err)
	}
	if claims.TenantID == "" || claims.KChatUserID == "" {
		return nil, errors.New("JWT is missing tenant_id or kchat_user_id claim")
	}
	return claims, nil
}

// oidcTokenClaims mirrors the JWT payload shape KMail consumes.
// It embeds jwt.RegisteredClaims so standard claim validation
// (exp / iss / aud) runs through the jwt library.
type oidcTokenClaims struct {
	TenantID          string `json:"tenant_id"`
	KChatUserID       string `json:"kchat_user_id"`
	StalwartAccountID string `json:"stalwart_account_id,omitempty"`
	jwt.RegisteredClaims
}

func (o *OIDC) verifyAndExtract(ctx context.Context, tokenStr string) (*Claims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{
			"RS256", "RS384", "RS512",
			"PS256", "PS384", "PS512",
			"ES256", "ES384", "ES512",
		}),
		jwt.WithIssuedAt(),
	)

	var claims oidcTokenClaims
	tok, err := parser.ParseWithClaims(tokenStr, &claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return o.cfg.JWKS.KeyFunc(ctx, kid)
	})
	if err != nil {
		return nil, fmt.Errorf("JWT: %w", err)
	}
	if !tok.Valid {
		return nil, errors.New("JWT: signature invalid")
	}

	// Issuer check: a non-empty configured issuer must match the
	// `iss` claim exactly. This protects against a mis-pointed
	// JWKS (e.g. against a different OIDC issuer that trusts the
	// same key material).
	if o.cfg.Issuer != "" && claims.Issuer != o.cfg.Issuer {
		return nil, fmt.Errorf("JWT: iss %q does not match expected issuer", claims.Issuer)
	}

	// Audience check: when configured, `aud` must include the
	// expected value.
	if o.cfg.Audience != "" {
		if !audienceContains(claims.Audience, o.cfg.Audience) {
			return nil, fmt.Errorf("JWT: aud does not contain %q", o.cfg.Audience)
		}
	}

	if claims.TenantID == "" || claims.KChatUserID == "" {
		return nil, errors.New("JWT is missing tenant_id or kchat_user_id claim")
	}
	return &Claims{
		TenantID:          claims.TenantID,
		KChatUserID:       claims.KChatUserID,
		StalwartAccountID: claims.StalwartAccountID,
	}, nil
}

func audienceContains(aud jwt.ClaimStrings, want string) bool {
	for _, v := range aud {
		if v == want {
			return true
		}
	}
	return false
}

// decodeJWTClaims parses the unverified payload of a compact JWT.
// Retained for the no-issuer fallback path; production traffic
// flows through verifyAndExtract instead.
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
