package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// issueAccessToken runs the full register → authorize → exchange flow
// and returns a live access token plus the resolved client.
func issueAccessToken(t *testing.T, svc *Service, tenant string, scopes []string) string {
	t.Helper()
	ctx := context.Background()
	redirect := "https://app.example.com/callback"
	client, _, err := svc.RegisterClient(ctx, tenant, "MW App", ClientTypeConfidential, []string{redirect}, scopes, "", "")
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	verifier := "this-is-a-sufficiently-long-code-verifier-1234567890"
	userID := seedUser(t, svc, tenant)
	code, err := svc.IssueAuthorizationCode(ctx, client, userID, redirect, scopes, s256Challenge(verifier), CodeChallengeMethodS256)
	if err != nil {
		t.Fatalf("IssueAuthorizationCode: %v", err)
	}
	tok, err := svc.ExchangeAuthorizationCode(ctx, client, code, redirect, verifier)
	if err != nil {
		t.Fatalf("ExchangeAuthorizationCode: %v", err)
	}
	return tok.AccessToken
}

// TestAuthMiddlewareWrapDB exercises the OAuth bearer middleware
// end-to-end against a live token: a valid token reaches the next
// handler with an AccessTokenContext attached; missing / malformed /
// unknown tokens are rejected 401 with a populated WWW-Authenticate.
func TestAuthMiddlewareWrapDB(t *testing.T) {
	svc, tenant := oauthService(t)
	token := issueAccessToken(t, svc, tenant, []string{ScopeReadMail})
	mw := NewAuthMiddleware(svc)

	var sawCtx bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc, ok := FromContext(r.Context())
		sawCtx = ok && tc != nil && tc.TenantID == tenant
		w.WriteHeader(http.StatusOK)
	})
	h := mw.Wrap(next)

	// Valid bearer → 200 + context attached.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integ/mail", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !sawCtx {
		t.Fatalf("valid token: code=%d ctx=%v", rec.Code, sawCtx)
	}

	// Missing header → 401 with WWW-Authenticate.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("missing token: code=%d hdr=%q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}

	// Malformed header → 401.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Basic abc")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("malformed: code=%d", rec.Code)
	}

	// Unknown token → 401 invalid_token.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown token: code=%d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_token" {
		t.Errorf("unknown token error=%q want invalid_token", body["error"])
	}
}

// TestAuthMiddlewareRequireScopeDB covers the scope-gated wrapper:
// a token holding the scope passes; one missing it is 403
// insufficient_scope.
func TestAuthMiddlewareRequireScopeDB(t *testing.T) {
	svc, tenant := oauthService(t)
	mw := NewAuthMiddleware(svc)
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	// Token with ScopeReadMail granted → reaching a ScopeReadMail route is OK.
	tokenMail := issueAccessToken(t, svc, tenant, []string{ScopeReadMail})
	h := mw.RequireScope(ScopeReadMail, next)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integ/mail", nil)
	req.Header.Set("Authorization", "Bearer "+tokenMail)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("granted scope: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Same token hitting a route that needs a scope it lacks → 403.
	h = mw.RequireScope(ScopeReadProfile, next)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/v1/integ/profile", nil)
	req.Header.Set("Authorization", "Bearer "+tokenMail)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing scope: code=%d want 403", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "insufficient_scope" {
		t.Errorf("error=%q want insufficient_scope", body["error"])
	}
}

// TestRevokeTokenDB covers RFC 7009 revocation: nil client, unknown
// token (silent success), cross-client rejection, access-token
// revocation, and refresh-token revocation cascading to its access
// tokens.
func TestRevokeTokenDB(t *testing.T) {
	svc, tenant := oauthService(t)
	ctx := context.Background()
	redirect := "https://app.example.com/callback"
	scopes := []string{ScopeReadMail}

	mkToken := func() (*Client, *TokenResponse) {
		client, _, err := svc.RegisterClient(ctx, tenant, "Rev App", ClientTypeConfidential, []string{redirect}, scopes, "", "")
		if err != nil {
			t.Fatalf("RegisterClient: %v", err)
		}
		verifier := "this-is-a-sufficiently-long-code-verifier-1234567890"
		userID := seedUser(t, svc, tenant)
		code, err := svc.IssueAuthorizationCode(ctx, client, userID, redirect, scopes, s256Challenge(verifier), CodeChallengeMethodS256)
		if err != nil {
			t.Fatalf("IssueAuthorizationCode: %v", err)
		}
		tok, err := svc.ExchangeAuthorizationCode(ctx, client, code, redirect, verifier)
		if err != nil {
			t.Fatalf("ExchangeAuthorizationCode: %v", err)
		}
		return client, tok
	}

	// nil client → ErrClientNotFound.
	if err := svc.RevokeToken(ctx, nil, "anything"); !errors.Is(err, ErrClientNotFound) {
		t.Errorf("nil client: want ErrClientNotFound got %v", err)
	}

	clientA, tokA := mkToken()

	// Unknown token → silent success (nil).
	if err := svc.RevokeToken(ctx, clientA, "totally-unknown-token"); err != nil {
		t.Errorf("unknown token should be silent success, got %v", err)
	}

	// A second client cannot revoke client A's access token.
	clientB, _ := mkToken()
	if err := svc.RevokeToken(ctx, clientB, tokA.AccessToken); err == nil {
		t.Error("cross-client revoke should be rejected")
	}

	// Owner revokes its access token → subsequent validation fails.
	if err := svc.RevokeToken(ctx, clientA, tokA.AccessToken); err != nil {
		t.Fatalf("revoke access token: %v", err)
	}
	if _, err := svc.ValidateAccessToken(ctx, tokA.AccessToken); !errors.Is(err, ErrAccessTokenNotFound) {
		t.Errorf("validate revoked access token: want ErrAccessTokenNotFound got %v", err)
	}

	// Revoking a refresh token cascades to its access token.
	clientC, tokC := mkToken()
	if err := svc.RevokeToken(ctx, clientC, tokC.RefreshToken); err != nil {
		t.Fatalf("revoke refresh token: %v", err)
	}
	if _, err := svc.ValidateAccessToken(ctx, tokC.AccessToken); !errors.Is(err, ErrAccessTokenNotFound) {
		t.Errorf("access token should be revoked after refresh revocation, got %v", err)
	}
	// Refresh after revocation must fail.
	if _, err := svc.RefreshAccessToken(ctx, clientC, tokC.RefreshToken); err == nil {
		t.Error("refresh after revoke should fail")
	}
}

func TestWithAccessTokenContextRoundTrip(t *testing.T) {
	tc := &AccessTokenContext{TenantID: "t-1", Scopes: []string{ScopeReadMail}}
	ctx := WithAccessTokenContext(context.Background(), tc)
	got, ok := FromContext(ctx)
	if !ok || got.TenantID != "t-1" || !got.HasScope(ScopeReadMail) {
		t.Fatalf("round-trip failed: ok=%v got=%+v", ok, got)
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Error("empty context should not yield a token context")
	}
}

func TestQuotedStringEscape(t *testing.T) {
	cases := map[string]string{
		"":            "",
		"plain":       "plain",
		`a"b`:         `a\"b`,
		`back\slash`:  `back\\slash`,
		"tab\tspace ": "tab\tspace ",
		"crlf\r\nx":   "crlfx", // control chars stripped
		"nul\x00end":  "nulend",
	}
	for in, want := range cases {
		if got := quotedStringEscape(in); got != want {
			t.Errorf("quotedStringEscape(%q)=%q want %q", in, got, want)
		}
	}
}
