package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// contextKey is intentionally unexported so callers MUST go
// through the package's FromContext helper. Using a typed key
// avoids accidental collisions with other middleware that uses
// string-keyed context values.
type contextKey struct{}

var accessTokenCtxKey = contextKey{}

// FromContext returns the AccessTokenContext attached to `ctx`
// by AuthMiddleware. The bool is false if no OAuth2 token
// authenticated the request (e.g. the request authenticated via
// the OIDC user-JWT path instead).
func FromContext(ctx context.Context) (*AccessTokenContext, bool) {
	v, ok := ctx.Value(accessTokenCtxKey).(*AccessTokenContext)
	return v, ok
}

// AuthMiddleware verifies the Authorization: Bearer header
// against the oauth_access_tokens table and attaches the
// resolved AccessTokenContext to the request context.
//
// On a missing / malformed / unknown / revoked / expired token,
// it returns HTTP 401 with the WWW-Authenticate header populated
// per RFC 6750 §3.
//
// This is wired in main.go specifically for the /api/v1/integ/*
// routes — the existing /api/v1/* routes continue to flow
// through the OIDC user-JWT middleware. The two paths are kept
// separate so a third-party app's token cannot accidentally
// reach an admin-only handler that assumes a human user.
type AuthMiddleware struct {
	svc *Service
}

// NewAuthMiddleware wires the middleware to a Service.
func NewAuthMiddleware(svc *Service) *AuthMiddleware {
	return &AuthMiddleware{svc: svc}
}

// Wrap returns a handler that authenticates via OAuth2 bearer
// token and rejects unauthenticated requests with 401.
func (m *AuthMiddleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := extractBearerToken(r)
		if err != nil {
			writeWWWAuthenticate(w, ErrCodeInvalidRequest, err.Error(), http.StatusUnauthorized)
			return
		}
		tokenCtx, err := m.svc.ValidateAccessToken(r.Context(), token)
		if err != nil {
			if errors.Is(err, ErrAccessTokenNotFound) {
				writeWWWAuthenticate(w, "invalid_token", "access token unknown, revoked, or expired", http.StatusUnauthorized)
				return
			}
			writeWWWAuthenticate(w, ErrCodeServerError, "token validation failed", http.StatusInternalServerError)
			return
		}
		ctx := context.WithValue(r.Context(), accessTokenCtxKey, tokenCtx)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireScope is a wrapper around Wrap that ALSO enforces the
// token carries `requiredScope`. Use this on per-route mounts
// where the scope is known at registration time (e.g.
// GET /api/v1/integ/mail → ScopeReadMail).
func (m *AuthMiddleware) RequireScope(requiredScope string, next http.Handler) http.Handler {
	wrapped := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCtx, ok := FromContext(r.Context())
		if !ok {
			writeWWWAuthenticate(w, "invalid_token", "missing access token context", http.StatusUnauthorized)
			return
		}
		if !tokenCtx.HasScope(requiredScope) {
			writeWWWAuthenticate(w, "insufficient_scope", "token does not carry required scope "+requiredScope, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
	return wrapped
}

// extractBearerToken parses "Authorization: Bearer <token>" per
// RFC 6750 §2.1. Returns the token string or an error explaining
// what was wrong (used for the 401 response body).
func extractBearerToken(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", errors.New("Authorization header missing")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return "", errors.New("Authorization header must be 'Bearer <token>'")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", errors.New("Bearer token empty")
	}
	return token, nil
}

// writeWWWAuthenticate emits the RFC 6750 §3 response envelope.
// We use the JSON body as the primary signal for SDKs / curl
// users; the header is the protocol-mandated form for browser
// libraries that expect to challenge on 401.
//
// Per RFC 7235 §2.1 the auth-param values are quoted-strings:
// they cannot contain unescaped DQUOTE / backslash / CR / LF.
// We escape callers' inputs defensively even though all current
// callers pass hardcoded strings — a future caller threading a
// user-controlled value into `description` would otherwise be
// able to inject extra headers (CRLF) or terminate the
// quoted-string prematurely (DQUOTE).
func writeWWWAuthenticate(w http.ResponseWriter, errorCode, description string, status int) {
	w.Header().Set("WWW-Authenticate",
		`Bearer realm="kmail", error="`+quotedStringEscape(errorCode)+
			`", error_description="`+quotedStringEscape(description)+`"`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]string{
		"error":             errorCode,
		"error_description": description,
	}
	_ = json.NewEncoder(w).Encode(body)
}

// quotedStringEscape returns `s` made safe for embedding inside
// the value of an HTTP `quoted-string` production (RFC 9110 §5.6.4
// / RFC 7235 §2.1). Backslash and DQUOTE are backslash-escaped;
// control characters (which are forbidden inside quoted-string
// values per the ABNF: VCHAR / SP / HTAB / obs-text) are stripped
// rather than escaped, because no Bearer-error vocabulary needs
// them and emitting them would risk header-injection in clients
// that lazily concatenate.
func quotedStringEscape(s string) string {
	if s == "" {
		return ""
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' || c == '"':
			out = append(out, '\\', c)
		case c == '\t' || c == ' ':
			out = append(out, c)
		case c < 0x20, c == 0x7f:
			// Drop CR/LF/NUL/etc. silently — they have no
			// place in a Bearer error envelope.
		default:
			out = append(out, c)
		}
	}
	return string(out)
}
