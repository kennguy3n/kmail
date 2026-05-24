package oauth

import (
	"errors"
	"time"
)

// Client types per RFC 6749 §2.1.
const (
	// ClientTypeConfidential identifies an OAuth2 client that
	// can keep a secret (server-side application). It MUST
	// authenticate to the token endpoint with its client_secret
	// in addition to (optionally) using PKCE.
	ClientTypeConfidential = "confidential"

	// ClientTypePublic identifies an OAuth2 client that cannot
	// keep a secret (SPA, mobile app, desktop app). It MUST use
	// PKCE; the token endpoint refuses non-PKCE exchanges for
	// public clients.
	ClientTypePublic = "public"
)

// PKCE challenge methods per RFC 7636 §4.2. We accept both
// "plain" and "S256" on the protocol surface, but the consent UI
// surfaces a warning when a client requests "plain" because
// S256 is the recommended method on every modern OAuth2 client
// library.
const (
	CodeChallengeMethodPlain = "plain"
	CodeChallengeMethodS256  = "S256"
)

// Grant types we support on /oauth/token per RFC 6749 §1.3.
// `client_credentials` is intentionally NOT here — see doc.go.
const (
	GrantTypeAuthorizationCode = "authorization_code"
	GrantTypeRefreshToken      = "refresh_token"
)

// Canonical scope identifiers. KMail follows the JMAP /
// CalDAV / CardDAV resource boundaries so a third-party app can
// be granted, e.g., read-only inbox access without also gaining
// write access to the user's calendar.
//
// New scopes MUST be added here AND in the `allowed_scopes`
// allow-list on the client row; the consent UI uses this list
// as the source of truth for what to display.
const (
	ScopeReadMail      = "read:mail"
	ScopeWriteMail     = "write:mail"
	ScopeReadCalendar  = "read:calendar"
	ScopeWriteCalendar = "write:calendar"
	ScopeReadContacts  = "read:contacts"
	ScopeWriteContacts = "write:contacts"
	ScopeReadProfile   = "read:profile"
)

// KnownScopes is the canonical set of scopes the authorize
// endpoint will accept. A request for a scope not in this set is
// rejected with `invalid_scope` per RFC 6749 §5.2.
var KnownScopes = map[string]struct{}{
	ScopeReadMail:      {},
	ScopeWriteMail:     {},
	ScopeReadCalendar:  {},
	ScopeWriteCalendar: {},
	ScopeReadContacts:  {},
	ScopeWriteContacts: {},
	ScopeReadProfile:   {},
}

// Default token lifetimes. These can be overridden per-deployment
// via the Service constructor; the defaults are conservative
// (short access tokens, longer refresh tokens) per the OAuth2
// threat model.
const (
	// AccessTokenTTL is the access token lifetime. 1 hour
	// matches the GitHub OAuth, Google OAuth, and Microsoft
	// Identity Platform defaults.
	AccessTokenTTL = 1 * time.Hour

	// RefreshTokenTTL is the refresh token lifetime. 30 days
	// balances "user shouldn't have to re-authorize too often"
	// against "a leaked refresh token should not provide
	// indefinite access". Rotation (every /oauth/token call
	// issues a new refresh token and revokes the old) means a
	// stolen refresh token is detectable.
	RefreshTokenTTL = 30 * 24 * time.Hour

	// AuthorizationCodeTTL is the authorization code lifetime.
	// 60 seconds per RFC 6749 §4.1.2 — codes are short-lived
	// bearer credentials that ride over the user's browser, so
	// the exchange window must be tight.
	AuthorizationCodeTTL = 60 * time.Second
)

// Client is the public view of an `oauth_clients` row. The
// `client_secret_hash` is NEVER returned to a caller; only the
// plaintext secret returned by RegisterClient (and the hash is
// what's stored).
type Client struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	ClientID       string    `json:"client_id"`
	ClientType     string    `json:"client_type"`
	Name           string    `json:"name"`
	HomepageURL    string    `json:"homepage_url,omitempty"`
	LogoURL        string    `json:"logo_url,omitempty"`
	RedirectURIs   []string  `json:"redirect_uris"`
	AllowedScopes  []string  `json:"allowed_scopes"`
	Active         bool      `json:"active"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// AccessTokenContext is the per-request claims set surfaced by
// the bearer-token middleware. Downstream handlers consume it
// via the package-level `FromContext` helper to know which
// tenant / user / scopes a request was authorized for.
type AccessTokenContext struct {
	TokenID   string
	TenantID  string
	UserID    string
	ClientID  string
	Scopes    []string
	ExpiresAt time.Time
}

// HasScope reports whether the access token's granted scopes
// include `want`. Scope checks MUST go through this helper so a
// future scope-hierarchy change (e.g. `write:mail` implies
// `read:mail`) lands in one place.
func (a *AccessTokenContext) HasScope(want string) bool {
	for _, s := range a.Scopes {
		if s == want {
			return true
		}
		// write:* implies read:* on the same resource. This is
		// the implicit hierarchy most operators expect; without
		// it a third-party app would need to request both
		// scopes which is just noise on the consent screen.
		if isWriteScopeImplyingRead(s, want) {
			return true
		}
	}
	return false
}

func isWriteScopeImplyingRead(granted, want string) bool {
	switch granted {
	case ScopeWriteMail:
		return want == ScopeReadMail
	case ScopeWriteCalendar:
		return want == ScopeReadCalendar
	case ScopeWriteContacts:
		return want == ScopeReadContacts
	}
	return false
}

// OAuth2 error codes per RFC 6749 §5.2. Surface these via the
// OAuthError type below so the token endpoint returns the right
// JSON envelope.
const (
	ErrCodeInvalidRequest       = "invalid_request"
	ErrCodeInvalidClient        = "invalid_client"
	ErrCodeInvalidGrant         = "invalid_grant"
	ErrCodeUnauthorizedClient   = "unauthorized_client"
	ErrCodeUnsupportedGrantType = "unsupported_grant_type"
	ErrCodeInvalidScope         = "invalid_scope"
	ErrCodeAccessDenied         = "access_denied"
	ErrCodeServerError          = "server_error"
)

// OAuthError is a typed protocol error that maps to the
// `{"error": "...", "error_description": "..."}` JSON envelope
// the OAuth2 spec requires on /oauth/token failures.
type OAuthError struct {
	Code        string
	Description string
	HTTPStatus  int
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return e.Code + ": " + e.Description
	}
	return e.Code
}

// Sentinel errors for typed callers.
var (
	ErrClientNotFound          = errors.New("oauth: client not found")
	ErrCodeNotFound            = errors.New("oauth: authorization code not found or expired")
	ErrCodeAlreadyConsumed     = errors.New("oauth: authorization code already used")
	ErrAccessTokenNotFound     = errors.New("oauth: access token not found or revoked")
	ErrRefreshTokenNotFound    = errors.New("oauth: refresh token not found or revoked")
	ErrInvalidRedirectURI      = errors.New("oauth: redirect_uri does not match allow-list")
	ErrInvalidCodeVerifier     = errors.New("oauth: code_verifier does not match challenge")
	ErrScopeNotAllowed         = errors.New("oauth: requested scope not in client allow-list")
	ErrClientSecretMismatch    = errors.New("oauth: client_secret mismatch")
	ErrPKCERequiredButMissing  = errors.New("oauth: PKCE required for public clients")
	ErrRefreshTokenReplay      = errors.New("oauth: refresh token replay detected — token family revoked")
)
