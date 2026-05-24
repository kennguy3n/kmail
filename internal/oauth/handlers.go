package oauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// csrfCookieName is the cookie that carries the consent-screen
// CSRF token. Path-scoped to the OAuth2 prefix so it is not
// leaked to unrelated routes; HttpOnly so JavaScript on the
// embedding page cannot read it; SameSite=Strict so a malicious
// cross-origin form cannot use a user's session to forge an
// approval. The double-submit pattern (cookie + hidden form
// field, compared with constant-time equality) is defense in
// depth on top of SameSite — a browser that lazily enforces
// SameSite still cannot satisfy the equality check without
// reading the cookie.
const csrfCookieName = "kmail_oauth_csrf"

// serviceAPI is the small slice of *Service that the HTTP layer
// needs. Defining it on the consumer side (handlers) — as
// idiomatic Go — gives two benefits: tests can drive the handlers
// with an in-memory fake (no Postgres pool required), and the
// interface boundary documents exactly which service methods are
// part of the wire-protocol contract vs. internal helpers. The
// real *Service satisfies serviceAPI by virtue of its method set.
type serviceAPI interface {
	GetClient(ctx context.Context, tenantID, clientID string) (*Client, error)
	IssueAuthorizationCode(ctx context.Context, client *Client, userID, redirectURI string, scopes []string, codeChallenge, codeChallengeMethod string) (string, error)
	LookupClientForExchange(ctx context.Context, clientID string) (*Client, error)
	VerifyClientSecret(ctx context.Context, client *Client, secret string) error
	ExchangeAuthorizationCode(ctx context.Context, client *Client, code, redirectURI, codeVerifier string) (*TokenResponse, error)
	RefreshAccessToken(ctx context.Context, client *Client, refreshToken string) (*TokenResponse, error)
	RevokeToken(ctx context.Context, client *Client, token string) error
}

// Handlers wires the OAuth2 endpoints to a Service. The handlers
// are deliberately thin — protocol details (state validation,
// PKCE verification, code consumption atomicity) live in the
// service layer so they're unit-testable without spinning up an
// HTTP server.
//
// The four endpoints are:
//
//	GET  /oauth/authorize          — consent screen, browser-facing
//	POST /oauth/authorize/approve  — user clicks "Approve", browser-facing
//	POST /oauth/token              — code or refresh-token exchange, machine-facing
//	POST /oauth/revoke             — RFC 7009 revocation, machine-facing
//
// The consent screen is rendered with html/template; the machine
// endpoints emit JSON envelopes per RFC 6749 §5.
type Handlers struct {
	svc serviceAPI

	// UserResolver returns the authenticated KChat user ID for an
	// incoming request. It bridges the existing KChat OIDC
	// middleware to the OAuth2 flow without coupling the two:
	// /oauth/authorize is reached by a logged-in human in the
	// KChat web UI, and the user-JWT claim is what tells us whose
	// consent is being recorded. Implementations typically pull
	// the user ID out of the request context populated by
	// internal/middleware's OIDC verifier.
	UserResolver func(r *http.Request) (userID string, tenantID string, ok bool)

	consentTmpl *template.Template

	// routePrefix is the URL prefix the OAuth2 routes are mounted
	// under (as passed to RegisterRoutes). The consent template
	// embeds it in the form action so the BFF can mount this set
	// of routes under any prefix — e.g. `/api/v1/oauth` — without
	// the approve POST 404'ing because the form was hardcoded to
	// `/oauth/authorize/approve`. Set in RegisterRoutes; empty if
	// the handlers were instantiated without going through it
	// (in which case Authorize falls back to the empty prefix,
	// i.e. the root, which is the legacy behaviour).
	routePrefix string

	// secureCookies, when true, marks the CSRF cookie as Secure
	// (cookie-set on plaintext HTTP will be ignored by RFC 6265).
	// Tests and local dev get an insecure default; production
	// callers set this true via SetSecureCookies before wiring
	// the routes. Falling open to false on test/dev is acceptable
	// because the CSRF cookie itself contains no session data —
	// it is just a random nonce.
	secureCookies bool

	// csrfNonce generates the per-request CSRF nonce. Defaults to
	// crypto/rand-backed; tests can substitute a deterministic
	// generator without resorting to runtime monkey-patching.
	csrfNonce func() (string, error)

	// logger records server-side errors that the RFC 6749 wire
	// envelope intentionally hides from the client (database
	// failures, unexpected service-layer errors mapped to
	// `server_error/500` by `mapTokenError`). Without this hook
	// the only observability for those failures would be the
	// HTTP access log, which doesn't surface the underlying
	// pgx / network / template error string. Defaults to
	// `log.Default()` when nil so callers that forget to wire
	// it still get errors on stderr. Settable via SetLogger.
	logger *log.Logger
}

// SetSecureCookies marks all cookies set by this handler set
// (currently just the CSRF cookie) as Secure. Call this in
// production wiring; leave it off for local plaintext HTTP dev.
func (h *Handlers) SetSecureCookies(secure bool) { h.secureCookies = secure }

// SetLogger overrides the default `log.Default()` server-side
// error sink. Production callers should wire this so the OAuth2
// failures land in the same prefixed logger the rest of the BFF
// uses; tests can leave it default or substitute a buffer-backed
// logger to assert error-path observability.
func (h *Handlers) SetLogger(logger *log.Logger) {
	if logger == nil {
		logger = log.Default()
	}
	h.logger = logger
}

// writeTokenError logs the underlying service-layer error
// server-side (so a database / pgx / network failure is visible
// in operator logs even though the wire envelope intentionally
// hides it from the caller) and then writes the RFC 6749 wire
// envelope through writeOAuthError. The endpoint hint is the
// path being handled — useful for grep-narrowing logs to the
// /oauth/token vs /oauth/revoke surface. Only 5xx envelopes log;
// 4xx envelopes are client mistakes (wrong secret, expired
// code, replay) that are signal in the access log, not the
// error log.
func (h *Handlers) writeTokenError(w http.ResponseWriter, endpoint string, err error) {
	wireErr := mapTokenError(err)
	if wireErr.HTTPStatus >= 500 {
		h.serverLogger().Printf("oauth: %s server-side failure: %v", endpoint, err)
	}
	writeOAuthError(w, wireErr)
}

// serverLogger returns the configured logger, falling back to
// the package-default if SetLogger was never called. Keeps the
// nil check in one place rather than at every callsite.
func (h *Handlers) serverLogger() *log.Logger {
	if h.logger != nil {
		return h.logger
	}
	return log.Default()
}

// NewHandlers constructs the HTTP handler set bound to a Service.
// UserResolver MUST be wired before calling Authorize / Approve —
// the consent flow is meaningless without an authenticated user.
func NewHandlers(svc *Service, userResolver func(r *http.Request) (string, string, bool)) *Handlers {
	return newHandlersWithAPI(svc, userResolver)
}

// newHandlersWithAPI is the test seam: it accepts any serviceAPI
// implementation so handler tests can supply an in-memory fake.
// External callers must use NewHandlers with the concrete
// *Service so production code paths can't silently bypass the
// real persistence layer.
func newHandlersWithAPI(svc serviceAPI, userResolver func(r *http.Request) (string, string, bool)) *Handlers {
	return &Handlers{
		svc:          svc,
		UserResolver: userResolver,
		consentTmpl:  template.Must(template.New("consent").Parse(consentHTML)),
		csrfNonce:    randomCSRFNonce,
	}
}

// randomCSRFNonce returns 32 bytes of crypto/rand entropy as a
// raw-URL-base64 string (no padding, URL-safe — fits in a cookie
// value AND a hidden form field without escaping).
func randomCSRFNonce() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// RegisterRoutes registers all four OAuth2 endpoints under the
// given prefix (typically "/oauth"). The prefix is stashed on the
// handler so the consent template renders the correct form
// action even when the routes are mounted under a non-default
// prefix — without this, the approve POST hardcoded to
// `/oauth/authorize/approve` would 404 when the BFF mounted
// these routes under e.g. `/api/v1/oauth`.
func (h *Handlers) RegisterRoutes(mux *http.ServeMux, prefix string) {
	prefix = strings.TrimRight(prefix, "/")
	h.routePrefix = prefix
	mux.HandleFunc(prefix+"/authorize", h.Authorize)
	mux.HandleFunc(prefix+"/authorize/approve", h.Approve)
	mux.HandleFunc(prefix+"/token", h.Token)
	mux.HandleFunc(prefix+"/revoke", h.Revoke)
}

// =================== Authorize (consent screen) ===================

// authorizeRequest is the parsed shape of the inbound
// /oauth/authorize?... query. It exists so the handler can reject
// malformed requests with structured errors before rendering the
// consent screen (we don't want the user to click Approve only to
// hit a 400 from the token endpoint later).
type authorizeRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	Scope               []string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

// Authorize renders the consent screen for a logged-in user.
//
// Per RFC 6749 §4.1.1, /oauth/authorize MUST accept response_type,
// client_id, redirect_uri, scope, state, and (for PKCE) the
// code_challenge / code_challenge_method pair. We validate:
//   - response_type == "code" (we only support the authorization
//     code grant; the implicit grant is deprecated)
//   - client_id resolves to an active oauth_clients row
//   - redirect_uri exactly matches one of the client's allow-list
//     entries
//   - every requested scope is in client.AllowedScopes
//   - for public clients, code_challenge is present
//
// Validation errors that the client app caused (unknown client,
// mismatched redirect_uri) are returned as HTTP 400 to the
// browser — we MUST NOT redirect to the supplied redirect_uri in
// those cases, because the URL is itself untrusted. Validation
// errors that the *user-agent* caused after a valid request are
// delivered via the redirect URL per the spec.
func (h *Handlers) Authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	req := parseAuthorizeRequest(r)

	if req.ResponseType != "code" {
		http.Error(w, "response_type must be 'code'", http.StatusBadRequest)
		return
	}
	if req.ClientID == "" {
		http.Error(w, "client_id required", http.StatusBadRequest)
		return
	}

	userID, tenantID, ok := h.UserResolver(r)
	if !ok || userID == "" || tenantID == "" {
		// Spec requires sending the user to login first. The BFF
		// is expected to wrap this handler with the OIDC user-JWT
		// middleware that handles the redirect; receiving an
		// unauthenticated request here means that wrapping is
		// missing, which is a configuration bug.
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	client, err := h.svc.GetClient(r.Context(), tenantID, req.ClientID)
	if err != nil {
		http.Error(w, "unknown or inactive client", http.StatusBadRequest)
		return
	}
	if !redirectURIInAllowList(client.RedirectURIs, req.RedirectURI) {
		// Per RFC 6749 §3.1.2.4 we MUST NOT redirect to a
		// mismatched URI — the user agent should see the error.
		http.Error(w, "redirect_uri does not match client allow-list", http.StatusBadRequest)
		return
	}
	for _, sc := range req.Scope {
		if !containsString(client.AllowedScopes, sc) {
			h.redirectWithError(w, r, req.RedirectURI, req.State,
				ErrCodeInvalidScope, "scope "+sc+" not allowed for this client")
			return
		}
	}
	if client.ClientType == ClientTypePublic && req.CodeChallenge == "" {
		h.redirectWithError(w, r, req.RedirectURI, req.State,
			ErrCodeInvalidRequest, "public clients must use PKCE")
		return
	}

	// Render the consent screen. The Approve form POSTs back to
	// `<prefix>/authorize/approve` with the same parameters
	// embedded as hidden fields so we can re-validate them
	// server-side — never trust the user-agent to round-trip them
	// honestly. We also mint a CSRF nonce, plant it both in a
	// cookie AND in a hidden form field, and check equality on
	// the POST handler (double-submit cookie pattern).
	csrf, err := h.csrfNonce()
	if err != nil {
		http.Error(w, "csrf generation failed", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    csrf,
		Path:     h.csrfCookiePath(),
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
	tplData := consentTemplateData{
		ClientName:          client.Name,
		ClientHomepage:      client.HomepageURL,
		ClientLogo:          client.LogoURL,
		RequestedScopes:     req.Scope,
		RedirectURI:         req.RedirectURI,
		ClientID:            req.ClientID,
		State:               req.State,
		CodeChallenge:       req.CodeChallenge,
		CodeChallengeMethod: req.CodeChallengeMethod,
		ApproveURL:          h.routePrefix + "/authorize/approve",
		CSRFToken:           csrf,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.consentTmpl.Execute(w, tplData); err != nil {
		http.Error(w, "consent screen render failed", http.StatusInternalServerError)
	}
}

// csrfCookiePath returns the cookie Path. Scope-limit to the
// OAuth2 prefix so the cookie isn't sent on every request to the
// BFF; fall back to "/" when no prefix was set (e.g. test
// harnesses that constructed Handlers directly via NewHandlers
// without going through RegisterRoutes).
func (h *Handlers) csrfCookiePath() string {
	if h.routePrefix == "" {
		return "/"
	}
	return h.routePrefix
}

// =================== Approve (user clicked Approve) ===================

// Approve handles the POST from the consent screen. The form
// carries the same parameters as the original /oauth/authorize
// request (as hidden fields). We re-validate them server-side
// because the user-agent could have tampered with them.
//
// On success we mint a one-time code, persist it, and redirect to
// the client's redirect_uri with ?code=...&state=.... If the user
// instead clicked Deny, the form would submit `decision=deny` and
// we redirect with the access_denied error.
func (h *Handlers) Approve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed form", http.StatusBadRequest)
		return
	}

	userID, tenantID, ok := h.UserResolver(r)
	if !ok || userID == "" || tenantID == "" {
		// Mirror the Authorize guard: an OIDC resolver that returns
		// ok=true with an empty userID or tenantID is a configuration
		// bug, not an authentication failure of the third-party app.
		// Treat it as an unauthenticated request rather than letting
		// the empty-string flow downstream and surface either as a
		// confused "unknown or inactive client" 400 (empty tenantID
		// path hitting GetClient) or as a generic server_error
		// returned via redirectWithOAuthError after
		// IssueAuthorizationCode rejects the empty user_id —
		// neither of which matches the spec's required redirect
		// semantics. The client app sees a clean 401 instead.
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}

	// CSRF verification BEFORE any state-changing work. The
	// cookie was set by Authorize when it rendered the consent
	// screen; the form field is the same value, planted by the
	// browser when the user submitted Approve. Both must match
	// (constant-time compare) AND both must be non-empty — an
	// empty cookie + empty form would otherwise trivially match.
	cookie, err := r.Cookie(csrfCookieName)
	formCSRF := r.Form.Get("csrf_token")
	if err != nil || cookie.Value == "" || formCSRF == "" ||
		subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(formCSRF)) != 1 {
		http.Error(w, "csrf token mismatch", http.StatusForbidden)
		return
	}
	// Invalidate the cookie on the response so it can't be
	// replayed against a second Approve. Set MaxAge < 0 per RFC
	// 6265 §4.1.2.2 to instruct the browser to delete it.
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    "",
		Path:     h.csrfCookiePath(),
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})

	clientID := r.Form.Get("client_id")
	redirectURI := r.Form.Get("redirect_uri")
	state := r.Form.Get("state")
	codeChallenge := r.Form.Get("code_challenge")
	codeChallengeMethod := r.Form.Get("code_challenge_method")
	scopes := splitScopes(r.Form.Get("scope"))
	decision := r.Form.Get("decision")

	client, err := h.svc.GetClient(r.Context(), tenantID, clientID)
	if err != nil {
		http.Error(w, "unknown or inactive client", http.StatusBadRequest)
		return
	}
	if !redirectURIInAllowList(client.RedirectURIs, redirectURI) {
		http.Error(w, "redirect_uri does not match client allow-list", http.StatusBadRequest)
		return
	}

	if decision != "approve" {
		h.redirectWithError(w, r, redirectURI, state,
			ErrCodeAccessDenied, "user denied consent")
		return
	}

	code, err := h.svc.IssueAuthorizationCode(
		r.Context(), client, userID, redirectURI, scopes,
		codeChallenge, codeChallengeMethod,
	)
	if err != nil {
		h.redirectWithOAuthError(w, r, redirectURI, state, err)
		return
	}

	u, _ := url.Parse(redirectURI)
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// =================== Token endpoint ===================

// Token implements /oauth/token, which routes by grant_type to
// either ExchangeAuthorizationCode or RefreshAccessToken.
//
// Confidential clients authenticate via HTTP Basic (preferred per
// RFC 6749 §2.3.1) OR via form parameters client_id +
// client_secret (the §2.3.1 alternative). Public clients
// authenticate by sending only client_id; they MUST present a
// code_verifier matching the original code_challenge.
//
// Per spec, this endpoint MUST emit JSON, set Cache-Control:
// no-store, and use grant-type-specific error codes.
func (h *Handlers) Token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidRequest, Description: "POST required", HTTPStatus: http.StatusMethodNotAllowed})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")

	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidRequest, Description: "malformed form body", HTTPStatus: http.StatusBadRequest})
		return
	}

	grantType := r.Form.Get("grant_type")
	clientID, clientSecret, isBasic := r.BasicAuth()
	if !isBasic {
		clientID = r.Form.Get("client_id")
		clientSecret = r.Form.Get("client_secret")
	}
	if clientID == "" {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidClient, Description: "client_id required", HTTPStatus: http.StatusUnauthorized})
		return
	}

	client, err := h.svc.LookupClientForExchange(r.Context(), clientID)
	if err != nil {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidClient, Description: "unknown client", HTTPStatus: http.StatusUnauthorized})
		return
	}
	if client.ClientType == ClientTypeConfidential {
		if err := h.svc.VerifyClientSecret(r.Context(), client, clientSecret); err != nil {
			writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidClient, Description: "client authentication failed", HTTPStatus: http.StatusUnauthorized})
			return
		}
	}

	switch grantType {
	case GrantTypeAuthorizationCode:
		h.handleAuthCodeGrant(w, r, client)
	case GrantTypeRefreshToken:
		h.handleRefreshGrant(w, r, client)
	default:
		writeOAuthError(w, &OAuthError{Code: ErrCodeUnsupportedGrantType, Description: "grant_type must be authorization_code or refresh_token", HTTPStatus: http.StatusBadRequest})
	}
}

func (h *Handlers) handleAuthCodeGrant(w http.ResponseWriter, r *http.Request, client *Client) {
	code := r.Form.Get("code")
	redirectURI := r.Form.Get("redirect_uri")
	codeVerifier := r.Form.Get("code_verifier")
	if code == "" {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidRequest, Description: "code required", HTTPStatus: http.StatusBadRequest})
		return
	}
	resp, err := h.svc.ExchangeAuthorizationCode(r.Context(), client, code, redirectURI, codeVerifier)
	if err != nil {
		h.writeTokenError(w, "/oauth/token authorization_code", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *Handlers) handleRefreshGrant(w http.ResponseWriter, r *http.Request, client *Client) {
	refreshToken := r.Form.Get("refresh_token")
	if refreshToken == "" {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidRequest, Description: "refresh_token required", HTTPStatus: http.StatusBadRequest})
		return
	}
	resp, err := h.svc.RefreshAccessToken(r.Context(), client, refreshToken)
	if err != nil {
		h.writeTokenError(w, "/oauth/token refresh_token", err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// =================== Revoke (RFC 7009) ===================

// Revoke implements RFC 7009. Spec is permissive: revoking an
// unknown or already-revoked token returns 200 OK. The only error
// shape allowed is unsupported_token_type, which we never emit
// (we accept both access and refresh tokens transparently).
func (h *Handlers) Revoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidRequest, Description: "POST required", HTTPStatus: http.StatusMethodNotAllowed})
		return
	}
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidRequest, Description: "malformed form body", HTTPStatus: http.StatusBadRequest})
		return
	}

	clientID, clientSecret, isBasic := r.BasicAuth()
	if !isBasic {
		clientID = r.Form.Get("client_id")
		clientSecret = r.Form.Get("client_secret")
	}
	if clientID == "" {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidClient, Description: "client_id required", HTTPStatus: http.StatusUnauthorized})
		return
	}
	client, err := h.svc.LookupClientForExchange(r.Context(), clientID)
	if err != nil {
		writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidClient, Description: "unknown client", HTTPStatus: http.StatusUnauthorized})
		return
	}
	if client.ClientType == ClientTypeConfidential {
		if err := h.svc.VerifyClientSecret(r.Context(), client, clientSecret); err != nil {
			writeOAuthError(w, &OAuthError{Code: ErrCodeInvalidClient, Description: "client authentication failed", HTTPStatus: http.StatusUnauthorized})
			return
		}
	}

	token := r.Form.Get("token")
	if token == "" {
		// RFC 7009 §2.1: empty token is treated as an unknown
		// token, which the spec mandates a 200 OK response for.
		w.WriteHeader(http.StatusOK)
		return
	}
	// Service swallows ErrAccessTokenNotFound / ErrRefreshTokenNotFound
	// (the RFC 7009 silent-success contract). What can still come
	// back is:
	//   * an *OAuthError, e.g. unauthorized_client/400 when a
	//     client presents a token issued to a DIFFERENT client —
	//     this MUST be surfaced as its own RFC 6749 §5.2 wire
	//     envelope, NOT collapsed to server_error/500. RFC 7009
	//     §2.2 explicitly says: "If the server is unable to
	//     locate the token using the given hint, it MUST extend
	//     its search across all of its supported token types. An
	//     authorization server MAY ignore this parameter,
	//     particularly if it is able to detect the token type
	//     automatically. This specification defines two such
	//     values [...]. The authorization server first validates
	//     the client credentials [...] and then verifies whether
	//     the token was issued to the client making the
	//     revocation request." — the cross-client case is a
	//     client error, not a server error.
	//   * a plain error (DB / tx / context cancel), which IS a
	//     server error and collapses to the original 500 envelope.
	// errors.As unwraps both *OAuthError and any nested wrapping.
	if err := h.svc.RevokeToken(r.Context(), client, token); err != nil {
		var oerr *OAuthError
		if errors.As(err, &oerr) {
			writeOAuthError(w, oerr)
			return
		}
		// Server-side failure (DB / tx / context cancel). Log
		// the underlying error before collapsing it to the
		// generic 500 envelope so operators can grep the
		// stderr stream to root-cause without exposing the
		// detail to the caller.
		h.serverLogger().Printf("oauth: /oauth/revoke server-side failure: %v", err)
		writeOAuthError(w, &OAuthError{
			Code:        ErrCodeServerError,
			Description: "revocation failed",
			HTTPStatus:  http.StatusInternalServerError,
		})
		return
	}
	w.WriteHeader(http.StatusOK)
}

// =================== Helpers ===================

func parseAuthorizeRequest(r *http.Request) authorizeRequest {
	q := r.URL.Query()
	return authorizeRequest{
		ResponseType:        q.Get("response_type"),
		ClientID:            q.Get("client_id"),
		RedirectURI:         q.Get("redirect_uri"),
		Scope:               splitScopes(q.Get("scope")),
		State:               q.Get("state"),
		CodeChallenge:       q.Get("code_challenge"),
		CodeChallengeMethod: q.Get("code_challenge_method"),
	}
}

func splitScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := strings.Fields(raw)
	return out
}

// redirectWithError sends the user-agent back to the client's
// redirect_uri with ?error=...&state=..., per RFC 6749 §4.1.2.1.
func (h *Handlers) redirectWithError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	u, err := url.Parse(redirectURI)
	if err != nil || u.Scheme == "" {
		http.Error(w, description, http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", description)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// redirectWithOAuthError unwraps an OAuthError (if present) and
// dispatches to redirectWithError with the right code.
func (h *Handlers) redirectWithOAuthError(w http.ResponseWriter, r *http.Request, redirectURI, state string, err error) {
	var oerr *OAuthError
	if errors.As(err, &oerr) {
		h.redirectWithError(w, r, redirectURI, state, oerr.Code, oerr.Description)
		return
	}
	h.redirectWithError(w, r, redirectURI, state, ErrCodeServerError, "internal error issuing authorization code")
}

// mapTokenError converts service-layer errors into the OAuth2-spec
// wire envelope. The /oauth/token handler is the only caller.
//
// The primary error mechanism in this package is *OAuthError —
// every code path in service.go that wants to return a
// spec-compliant client-error reply returns one directly with the
// right Code + HTTPStatus already populated, and `errors.As`
// unwraps that here. The sentinel checks below are the secondary
// path: a handful of service functions (specifically
// RefreshAccessToken and the Exchange code path) return plain
// `errors.New(...)` sentinels instead of *OAuthError so the
// inner Postgres transaction can roll back via a typed return and
// the outer caller still gets a stable wire shape.
//
// Every sentinel that is a *client mistake* is enumerated below.
// Anything that falls through to the default branch is, by
// definition, NOT a recognised client mistake — it is either an
// infrastructure failure (a pgx connection error, a pool
// exhaustion, a panicked driver) or a future sentinel that was
// added without updating this map. Both deserve a 5xx so the
// client retries and operator alerting fires. Misclassifying them
// as invalid_grant/400 (the previous default) would cause clients
// to give up on requests that are actually server-side failures
// AND would hide outages from the 5xx-rate SLOs. See RFC 6749
// §5.2: invalid_grant is reserved for grants the client could
// have legitimately known were invalid — not for "the server
// blew up while looking up the grant".
func mapTokenError(err error) *OAuthError {
	var oerr *OAuthError
	if errors.As(err, &oerr) {
		return oerr
	}
	switch {
	// Client mistakes: invalid_grant per RFC 6749 §5.2. Stable
	// wire shape regardless of which inner check tripped.
	case errors.Is(err, ErrCodeNotFound),
		errors.Is(err, ErrCodeAlreadyConsumed),
		errors.Is(err, ErrInvalidRedirectURI),
		errors.Is(err, ErrInvalidCodeVerifier),
		errors.Is(err, ErrPKCERequiredButMissing),
		errors.Is(err, ErrRefreshTokenNotFound),
		errors.Is(err, ErrRefreshTokenReplay):
		return &OAuthError{Code: ErrCodeInvalidGrant, Description: "authorization grant invalid", HTTPStatus: http.StatusBadRequest}
	case errors.Is(err, ErrScopeNotAllowed):
		return &OAuthError{Code: ErrCodeInvalidScope, Description: "scope not allowed", HTTPStatus: http.StatusBadRequest}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &OAuthError{Code: ErrCodeServerError, Description: "request cancelled", HTTPStatus: http.StatusServiceUnavailable}
	default:
		// Anything else is a server-side failure: DB unreachable,
		// pool exhausted, driver panic, a new sentinel that nobody
		// remembered to add above, etc. Reply 500 so the client
		// retries and the on-call SLO catches the regression. The
		// description deliberately does NOT echo err.Error() —
		// surfacing the raw pgx / driver message to third-party
		// apps would leak schema details; the structured log
		// emitted by the surrounding handler is the channel that
		// keeps the diagnostic info.
		return &OAuthError{Code: ErrCodeServerError, Description: "internal error issuing token", HTTPStatus: http.StatusInternalServerError}
	}
}

func writeOAuthError(w http.ResponseWriter, oerr *OAuthError) {
	w.Header().Set("Content-Type", "application/json")
	status := oerr.HTTPStatus
	if status == 0 {
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	body := map[string]string{"error": oerr.Code}
	if oerr.Description != "" {
		body["error_description"] = oerr.Description
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// =================== Consent screen template ===================

type consentTemplateData struct {
	ClientName          string
	ClientHomepage      string
	ClientLogo          string
	RequestedScopes     []string
	RedirectURI         string
	ClientID            string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	// ApproveURL is the URL the consent form POSTs to. Threaded
	// from Handlers.routePrefix so the form action is correct
	// regardless of where the BFF mounts these routes.
	ApproveURL string
	// CSRFToken is the per-request CSRF nonce. Same value is
	// also set in the kmail_oauth_csrf cookie; Approve compares
	// the two with constant-time equality.
	CSRFToken string
}

// consentHTML is intentionally minimal — the KChat shell injects
// branding around it. We render scope strings verbatim because
// they're constrained to a known set in types.go (KnownScopes);
// any client-controlled value (ClientName, ClientHomepage,
// ClientLogo) is auto-escaped by html/template.
const consentHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Authorize {{ .ClientName }}</title>
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <style>
    body { font-family: -apple-system, system-ui, sans-serif; max-width: 480px; margin: 4rem auto; padding: 0 1rem; }
    h1 { font-size: 1.25rem; }
    .scopes { background: #f6f8fa; padding: 1rem; border-radius: 6px; }
    .scopes li { margin: 0.3rem 0; }
    .actions { margin-top: 1.5rem; display: flex; gap: 0.75rem; }
    button { padding: 0.5rem 1rem; border-radius: 6px; border: 1px solid #d0d7de; cursor: pointer; }
    button.primary { background: #0969da; color: white; border-color: #0969da; }
    img.logo { width: 48px; height: 48px; border-radius: 8px; vertical-align: middle; margin-right: 0.5rem; }
  </style>
</head>
<body>
  <h1>
    {{ if .ClientLogo }}<img class="logo" src="{{ .ClientLogo }}" alt="">{{ end }}
    {{ .ClientName }} wants to access your KMail
  </h1>
  {{ if .ClientHomepage }}<p><a href="{{ .ClientHomepage }}" target="_blank" rel="noopener noreferrer">Learn more about this app</a></p>{{ end }}
  <div class="scopes">
    <strong>It will be able to:</strong>
    <ul>
      {{ range .RequestedScopes }}
      <li><code>{{ . }}</code></li>
      {{ end }}
    </ul>
  </div>
  <form method="POST" action="{{ .ApproveURL }}">
    <input type="hidden" name="client_id" value="{{ .ClientID }}">
    <input type="hidden" name="redirect_uri" value="{{ .RedirectURI }}">
    <input type="hidden" name="scope" value="{{ range $i, $s := .RequestedScopes }}{{ if $i }} {{ end }}{{ $s }}{{ end }}">
    <input type="hidden" name="state" value="{{ .State }}">
    <input type="hidden" name="code_challenge" value="{{ .CodeChallenge }}">
    <input type="hidden" name="code_challenge_method" value="{{ .CodeChallengeMethod }}">
    <input type="hidden" name="csrf_token" value="{{ .CSRFToken }}">
    <div class="actions">
      <button type="submit" name="decision" value="approve" class="primary">Allow</button>
      <button type="submit" name="decision" value="deny">Cancel</button>
    </div>
  </form>
</body>
</html>`
