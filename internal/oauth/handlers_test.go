package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// =================== Test doubles ===================

// fakeService is an in-memory implementation of serviceAPI for
// handler-level tests. It records call counts so tests can assert
// that the handler invoked the right service method exactly once
// (e.g. confirming that /oauth/token routes by grant_type without
// silently calling both code-exchange and refresh paths).
type fakeService struct {
	clients map[string]*Client // key = clientID

	// per-call overrides — set these in a test to make the next
	// invocation of the matching method return the supplied error
	// or value. Default zero state means "client lookup succeeds
	// if the ID is in clients, all other methods return nil".
	errGetClient                error
	errLookupClient             error
	errVerifySecret             error
	errIssueCode                error
	errExchangeCode             error
	errRefreshToken             error
	errRevoke                   error
	codeToReturn                string
	tokenResponse               *TokenResponse
	calls                       map[string]int
	lastIssueScopes             []string
	lastIssueCodeChallenge      string
	lastIssueCodeChallengeMeth  string
}

func newFakeService() *fakeService {
	return &fakeService{
		clients: make(map[string]*Client),
		calls:   make(map[string]int),
	}
}

func (f *fakeService) record(name string) { f.calls[name]++ }

func (f *fakeService) GetClient(_ context.Context, _, clientID string) (*Client, error) {
	f.record("GetClient")
	if f.errGetClient != nil {
		return nil, f.errGetClient
	}
	c, ok := f.clients[clientID]
	if !ok {
		return nil, errors.New("oauth: client not found")
	}
	return c, nil
}

func (f *fakeService) IssueAuthorizationCode(
	_ context.Context, _ *Client, _, _ string, scopes []string, ch, chMethod string,
) (string, error) {
	f.record("IssueAuthorizationCode")
	f.lastIssueScopes = scopes
	f.lastIssueCodeChallenge = ch
	f.lastIssueCodeChallengeMeth = chMethod
	if f.errIssueCode != nil {
		return "", f.errIssueCode
	}
	if f.codeToReturn != "" {
		return f.codeToReturn, nil
	}
	return "fake-code-abc123", nil
}

func (f *fakeService) LookupClientForExchange(_ context.Context, clientID string) (*Client, error) {
	f.record("LookupClientForExchange")
	if f.errLookupClient != nil {
		return nil, f.errLookupClient
	}
	c, ok := f.clients[clientID]
	if !ok {
		return nil, errors.New("oauth: client not found")
	}
	return c, nil
}

func (f *fakeService) VerifyClientSecret(_ context.Context, _ *Client, _ string) error {
	f.record("VerifyClientSecret")
	return f.errVerifySecret
}

func (f *fakeService) ExchangeAuthorizationCode(
	_ context.Context, _ *Client, _, _, _ string,
) (*TokenResponse, error) {
	f.record("ExchangeAuthorizationCode")
	if f.errExchangeCode != nil {
		return nil, f.errExchangeCode
	}
	if f.tokenResponse != nil {
		return f.tokenResponse, nil
	}
	return &TokenResponse{
		AccessToken:  "fake-access",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		RefreshToken: "fake-refresh",
		Scope:        "kmail.read",
	}, nil
}

func (f *fakeService) RefreshAccessToken(
	_ context.Context, _ *Client, _ string,
) (*TokenResponse, error) {
	f.record("RefreshAccessToken")
	if f.errRefreshToken != nil {
		return nil, f.errRefreshToken
	}
	if f.tokenResponse != nil {
		return f.tokenResponse, nil
	}
	return &TokenResponse{
		AccessToken:  "fake-access-2",
		TokenType:    "Bearer",
		ExpiresIn:    3600,
		RefreshToken: "fake-refresh-2",
		Scope:        "kmail.read",
	}, nil
}

func (f *fakeService) RevokeToken(_ context.Context, _ *Client, _ string) error {
	f.record("RevokeToken")
	return f.errRevoke
}

// userResolverOK always returns a valid user/tenant. The OIDC
// middleware is responsible for the real auth check; the
// handlers under test only consume the resolver output.
func userResolverOK(_ *http.Request) (string, string, bool) {
	return "user-1", "tenant-1", true
}

func userResolverFail(_ *http.Request) (string, string, bool) {
	return "", "", false
}

// makeConfidentialClient is the shape RegisterClient produces
// in the real Service, abbreviated to the fields handlers read.
func makeConfidentialClient() *Client {
	return &Client{
		TenantID:      "tenant-1",
		ClientID:      "client-conf-1",
		ClientType:    ClientTypeConfidential,
		Name:          "Test Confidential App",
		RedirectURIs:  []string{"https://app.example.com/cb"},
		AllowedScopes: []string{"kmail.read", "kmail.write"},
		HomepageURL:   "https://app.example.com",
	}
}

func makePublicClient() *Client {
	return &Client{
		TenantID:      "tenant-1",
		ClientID:      "client-pub-1",
		ClientType:    ClientTypePublic,
		Name:          "Test Public App",
		RedirectURIs:  []string{"https://spa.example.com/cb"},
		AllowedScopes: []string{"kmail.read"},
	}
}

// =================== Authorize (GET /oauth/authorize) ===================

func TestAuthorize_RejectsNonGet(t *testing.T) {
	h := newHandlersWithAPI(newFakeService(), userResolverOK)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize", nil)
	h.Authorize(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestAuthorize_RejectsResponseTypeOtherThanCode(t *testing.T) {
	h := newHandlersWithAPI(newFakeService(), userResolverOK)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?response_type=token&client_id=foo", nil)
	h.Authorize(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-code response_type, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "response_type") {
		t.Errorf("expected error message mentioning response_type, got %q", rr.Body.String())
	}
}

func TestAuthorize_RejectsEmptyClientID(t *testing.T) {
	h := newHandlersWithAPI(newFakeService(), userResolverOK)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?response_type=code", nil)
	h.Authorize(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty client_id, got %d", rr.Code)
	}
}

func TestAuthorize_RejectsUnauthenticatedUser(t *testing.T) {
	h := newHandlersWithAPI(newFakeService(), userResolverFail)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?response_type=code&client_id=foo", nil)
	h.Authorize(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated user, got %d", rr.Code)
	}
}

func TestAuthorize_RejectsUnknownClient(t *testing.T) {
	svc := newFakeService()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/oauth/authorize?response_type=code&client_id=does-not-exist", nil)
	h.Authorize(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown client, got %d", rr.Code)
	}
	if svc.calls["GetClient"] != 1 {
		t.Errorf("expected exactly one GetClient call, got %d", svc.calls["GetClient"])
	}
}

func TestAuthorize_RejectsRedirectURIMismatch(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	// Different host from the registered https://app.example.com/cb
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", "client-conf-1")
	q.Set("redirect_uri", "https://evil.example.com/cb")
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	h.Authorize(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for redirect_uri mismatch, got %d", rr.Code)
	}
	// RFC 6749 §3.1.2.4: MUST NOT redirect to the supplied URI
	// when it isn't in the allow-list. Verify we surfaced the
	// error directly instead of issuing a 302.
	if loc := rr.Header().Get("Location"); loc != "" {
		t.Errorf("expected no Location header on redirect_uri mismatch, got %q", loc)
	}
}

func TestAuthorize_RejectsScopeOutsideAllowList(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", "client-conf-1")
	q.Set("redirect_uri", "https://app.example.com/cb")
	q.Set("scope", "kmail.read kmail.admin") // admin not in allow-list
	q.Set("state", "xyz")
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	h.Authorize(rr, req)
	// Per spec, an invalid_scope is delivered via redirect with
	// the error code (§4.1.2.1) because the redirect_uri itself
	// has already been validated.
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 with error code in query, got %d", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "error="+ErrCodeInvalidScope) {
		t.Errorf("expected Location to carry invalid_scope, got %q", loc)
	}
	if !strings.Contains(loc, "state=xyz") {
		t.Errorf("expected state to be echoed, got %q", loc)
	}
}

func TestAuthorize_RejectsPublicClientWithoutPKCE(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-pub-1"] = makePublicClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", "client-pub-1")
	q.Set("redirect_uri", "https://spa.example.com/cb")
	q.Set("scope", "kmail.read")
	q.Set("state", "s1")
	// No code_challenge.
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	h.Authorize(rr, req)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302 with invalid_request, got %d", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "error="+ErrCodeInvalidRequest) {
		t.Errorf("expected invalid_request in Location, got %q", loc)
	}
}

func TestAuthorize_RendersConsentHTMLOnHappyPath(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", "client-conf-1")
	q.Set("redirect_uri", "https://app.example.com/cb")
	q.Set("scope", "kmail.read")
	q.Set("state", "xyz")
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	h.Authorize(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	ct := rr.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Errorf("expected text/html content-type, got %q", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Test Confidential App") {
		t.Errorf("expected client name in body, got %q", body)
	}
	if !strings.Contains(body, "kmail.read") {
		t.Errorf("expected requested scope in body, got %q", body)
	}
}

// =================== Approve (POST /oauth/authorize/approve) ===================

func TestApprove_RejectsNonPost(t *testing.T) {
	h := newHandlersWithAPI(newFakeService(), userResolverOK)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize/approve", nil)
	h.Approve(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestApprove_RejectsUnauthenticated(t *testing.T) {
	h := newHandlersWithAPI(newFakeService(), userResolverFail)
	rr := httptest.NewRecorder()
	req := newApproveRequest(url.Values{})
	h.Approve(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestApprove_DenialRedirectsWithAccessDenied(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("redirect_uri", "https://app.example.com/cb")
	form.Set("state", "s1")
	form.Set("decision", "deny")
	form.Set("scope", "kmail.read")
	rr := httptest.NewRecorder()
	h.Approve(rr, newApproveRequest(form))
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rr.Code)
	}
	loc := rr.Header().Get("Location")
	if !strings.Contains(loc, "error="+ErrCodeAccessDenied) {
		t.Errorf("expected access_denied in Location, got %q", loc)
	}
	if !strings.Contains(loc, "state=s1") {
		t.Errorf("expected state echoed, got %q", loc)
	}
	if svc.calls["IssueAuthorizationCode"] != 0 {
		t.Errorf("denial path must NOT mint a code, got %d calls", svc.calls["IssueAuthorizationCode"])
	}
}

func TestApprove_ApprovalRedirectsWithCode(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	svc.codeToReturn = "real-code-xyz"
	h := newHandlersWithAPI(svc, userResolverOK)
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("redirect_uri", "https://app.example.com/cb")
	form.Set("state", "s1")
	form.Set("decision", "approve")
	form.Set("scope", "kmail.read")
	form.Set("code_challenge", "abc")
	form.Set("code_challenge_method", "S256")
	rr := httptest.NewRecorder()
	h.Approve(rr, newApproveRequest(form))
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("Location not parseable: %v", err)
	}
	if u.Query().Get("code") != "real-code-xyz" {
		t.Errorf("expected code=real-code-xyz, got %q", u.Query().Get("code"))
	}
	if u.Query().Get("state") != "s1" {
		t.Errorf("expected state=s1, got %q", u.Query().Get("state"))
	}
	// PKCE params must reach the service untouched — otherwise
	// the eventual code exchange can't verify them.
	if svc.lastIssueCodeChallenge != "abc" {
		t.Errorf("expected code_challenge to thread through, got %q", svc.lastIssueCodeChallenge)
	}
	if svc.lastIssueCodeChallengeMeth != "S256" {
		t.Errorf("expected code_challenge_method to thread through, got %q", svc.lastIssueCodeChallengeMeth)
	}
}

// =================== CSRF (Approve) ===================

func TestApprove_RejectsMissingCSRFCookie(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("redirect_uri", "https://app.example.com/cb")
	form.Set("decision", "approve")
	form.Set("csrf_token", "abcd1234")
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize/approve",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// Deliberately no cookie.
	rr := httptest.NewRecorder()
	h.Approve(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if svc.calls["IssueAuthorizationCode"] != 0 {
		t.Errorf("CSRF-rejected request must NOT mint a code")
	}
}

func TestApprove_RejectsMissingCSRFForm(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("redirect_uri", "https://app.example.com/cb")
	form.Set("decision", "approve")
	// Deliberately no csrf_token in form.
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize/approve",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "abcd1234"})
	rr := httptest.NewRecorder()
	h.Approve(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestApprove_RejectsMismatchedCSRF(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("redirect_uri", "https://app.example.com/cb")
	form.Set("decision", "approve")
	form.Set("csrf_token", "form-value")
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize/approve",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "cookie-value-different"})
	rr := httptest.NewRecorder()
	h.Approve(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rr.Code)
	}
}

func TestApprove_ClearsCSRFCookieAfterSuccess(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	svc.codeToReturn = "ok-code"
	h := newHandlersWithAPI(svc, userResolverOK)
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("redirect_uri", "https://app.example.com/cb")
	form.Set("decision", "approve")
	form.Set("scope", "kmail.read")
	rr := httptest.NewRecorder()
	h.Approve(rr, newApproveRequest(form))
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	// Expect a Set-Cookie that clears the CSRF cookie (MaxAge<0).
	found := false
	for _, sc := range rr.Result().Cookies() {
		if sc.Name == csrfCookieName && sc.MaxAge < 0 {
			found = true
		}
	}
	if !found {
		t.Errorf("expected csrf cookie to be cleared after successful Approve")
	}
}

func TestAuthorize_PlantsCSRFCookieAndFormField(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	// Use a deterministic nonce so we can assert on it.
	h.csrfNonce = func() (string, error) { return "fixed-test-nonce", nil }

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", "client-conf-1")
	q.Set("redirect_uri", "https://app.example.com/cb")
	q.Set("scope", "kmail.read")
	q.Set("state", "s1")
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	rr := httptest.NewRecorder()
	h.Authorize(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	// Cookie planted.
	var cookieVal string
	for _, sc := range rr.Result().Cookies() {
		if sc.Name == csrfCookieName {
			cookieVal = sc.Value
			if !sc.HttpOnly {
				t.Errorf("csrf cookie must be HttpOnly")
			}
			if sc.SameSite != http.SameSiteStrictMode {
				t.Errorf("csrf cookie must be SameSite=Strict")
			}
		}
	}
	if cookieVal != "fixed-test-nonce" {
		t.Errorf("expected csrf cookie value 'fixed-test-nonce', got %q", cookieVal)
	}
	// Hidden form field planted with the SAME value.
	if !strings.Contains(rr.Body.String(),
		`<input type="hidden" name="csrf_token" value="fixed-test-nonce">`) {
		t.Errorf("consent screen missing csrf_token hidden field with expected value")
	}
}

// =================== Prefix routing ===================

func TestAuthorize_FormActionRespectsRoutePrefix(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	mux := http.NewServeMux()
	// Mount under a non-default prefix to verify the consent form
	// action threads the prefix through.
	h.RegisterRoutes(mux, "/api/v1/oauth")

	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", "client-conf-1")
	q.Set("redirect_uri", "https://app.example.com/cb")
	q.Set("scope", "kmail.read")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/oauth/authorize?"+q.Encode(), nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(),
		`<form method="POST" action="/api/v1/oauth/authorize/approve">`) {
		t.Errorf("form action does not reflect /api/v1/oauth prefix; body=%s", rr.Body.String())
	}
}

// =================== Token (POST /oauth/token) ===================

func TestToken_RejectsNonPost(t *testing.T) {
	h := newHandlersWithAPI(newFakeService(), userResolverOK)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth/token", nil)
	h.Token(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeInvalidRequest)
}

func TestToken_SetsNoStoreCacheHeaders(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	form := url.Values{}
	form.Set("grant_type", GrantTypeAuthorizationCode)
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	form.Set("code", "real-code-xyz")
	form.Set("redirect_uri", "https://app.example.com/cb")
	rr := httptest.NewRecorder()
	h.Token(rr, newTokenRequest(form, ""))
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control=no-store, got %q", cc)
	}
	if pg := rr.Header().Get("Pragma"); pg != "no-cache" {
		t.Errorf("expected Pragma=no-cache, got %q", pg)
	}
}

func TestToken_RejectsEmptyClientID(t *testing.T) {
	h := newHandlersWithAPI(newFakeService(), userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", GrantTypeAuthorizationCode)
	h.Token(rr, newTokenRequest(form, ""))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeInvalidClient)
}

func TestToken_RejectsUnknownClient(t *testing.T) {
	svc := newFakeService()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", GrantTypeAuthorizationCode)
	form.Set("client_id", "missing")
	h.Token(rr, newTokenRequest(form, ""))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown client, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeInvalidClient)
}

func TestToken_RejectsBadClientSecret(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	svc.errVerifySecret = errors.New("oauth: bad secret")
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", GrantTypeAuthorizationCode)
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "wrong")
	h.Token(rr, newTokenRequest(form, ""))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for bad secret, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeInvalidClient)
}

func TestToken_PublicClientSkipsSecretVerification(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-pub-1"] = makePublicClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", GrantTypeAuthorizationCode)
	form.Set("client_id", "client-pub-1")
	form.Set("code", "any-code")
	form.Set("code_verifier", "any-verifier")
	form.Set("redirect_uri", "https://spa.example.com/cb")
	h.Token(rr, newTokenRequest(form, ""))
	if svc.calls["VerifyClientSecret"] != 0 {
		t.Errorf("public clients must not call VerifyClientSecret, got %d calls", svc.calls["VerifyClientSecret"])
	}
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200 for happy-path public exchange, got %d", rr.Code)
	}
}

func TestToken_UnsupportedGrantType(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", "password") // banned grant
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	h.Token(rr, newTokenRequest(form, ""))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeUnsupportedGrantType)
}

func TestToken_AuthCodeHappyPath(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", GrantTypeAuthorizationCode)
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	form.Set("code", "real-code")
	form.Set("redirect_uri", "https://app.example.com/cb")
	h.Token(rr, newTokenRequest(form, ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp TokenResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response body not JSON: %v", err)
	}
	if resp.AccessToken == "" || resp.RefreshToken == "" {
		t.Errorf("expected both access and refresh tokens, got %+v", resp)
	}
	if resp.TokenType != "Bearer" {
		t.Errorf("expected token_type=Bearer, got %q", resp.TokenType)
	}
	if svc.calls["RefreshAccessToken"] != 0 {
		t.Errorf("code grant must not call RefreshAccessToken, got %d", svc.calls["RefreshAccessToken"])
	}
}

func TestToken_RefreshHappyPath(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", GrantTypeRefreshToken)
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	form.Set("refresh_token", "old-refresh")
	h.Token(rr, newTokenRequest(form, ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if svc.calls["ExchangeAuthorizationCode"] != 0 {
		t.Errorf("refresh grant must not call ExchangeAuthorizationCode, got %d", svc.calls["ExchangeAuthorizationCode"])
	}
	if svc.calls["RefreshAccessToken"] != 1 {
		t.Errorf("expected exactly one RefreshAccessToken call, got %d", svc.calls["RefreshAccessToken"])
	}
}

func TestToken_CodeMissing(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", GrantTypeAuthorizationCode)
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	// no code
	h.Token(rr, newTokenRequest(form, ""))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when code is missing, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeInvalidRequest)
}

func TestToken_RefreshMissing(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", GrantTypeRefreshToken)
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	// no refresh_token
	h.Token(rr, newTokenRequest(form, ""))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when refresh_token is missing, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeInvalidRequest)
}

func TestToken_HTTPBasicAuthIsAccepted(t *testing.T) {
	// RFC 6749 §2.3.1 mandates that confidential clients MAY use
	// HTTP Basic instead of form params. Verify the handler reads
	// from Basic when present.
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("grant_type", GrantTypeAuthorizationCode)
	form.Set("code", "real-code")
	form.Set("redirect_uri", "https://app.example.com/cb")
	req := newTokenRequest(form, "")
	req.SetBasicAuth("client-conf-1", "any-secret")
	h.Token(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for Basic-auth happy path, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	if svc.calls["VerifyClientSecret"] != 1 {
		t.Errorf("expected VerifyClientSecret to be called once via Basic, got %d", svc.calls["VerifyClientSecret"])
	}
}

// =================== Revoke (POST /oauth/revoke) ===================

func TestRevoke_RejectsNonPost(t *testing.T) {
	h := newHandlersWithAPI(newFakeService(), userResolverOK)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/oauth/revoke", nil)
	h.Revoke(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}

func TestRevoke_EmptyTokenReturnsOK(t *testing.T) {
	// RFC 7009 §2.1: an empty / unknown token must yield 200.
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	// no token
	h.Revoke(rr, newRevokeRequest(form))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on empty token, got %d", rr.Code)
	}
	if svc.calls["RevokeToken"] != 0 {
		t.Errorf("empty token must short-circuit before service call, got %d", svc.calls["RevokeToken"])
	}
}

func TestRevoke_HappyPathReturnsOK(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	form.Set("token", "some-token")
	h.Revoke(rr, newRevokeRequest(form))
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 on revoke, got %d", rr.Code)
	}
	if svc.calls["RevokeToken"] != 1 {
		t.Errorf("expected RevokeToken to be called once, got %d", svc.calls["RevokeToken"])
	}
}

func TestRevoke_ServerErrorMapsTo500(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	svc.errRevoke = errors.New("oauth: db went away")
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	form.Set("token", "some-token")
	h.Revoke(rr, newRevokeRequest(form))
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on db error, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeServerError)
}

// TestRevoke_UnwrapsOAuthError pins the RFC 7009 §2.2 contract:
// when the service rejects a revocation because the token was
// issued to a DIFFERENT client (or any other client error), the
// handler MUST surface the underlying *OAuthError wire envelope
// (e.g. unauthorized_client/400), not collapse to server_error/500.
// The earlier shape mapped any non-nil err to 500 which masked the
// cross-client signal a well-behaved confidential client needs in
// order to distinguish "token doesn't exist" (silent 200) from
// "token isn't yours" (400 unauthorized_client).
func TestRevoke_UnwrapsOAuthError(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	svc.errRevoke = &OAuthError{
		Code:        ErrCodeUnauthorizedClient,
		Description: "token was issued to a different client",
		HTTPStatus:  http.StatusBadRequest,
	}
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	form.Set("token", "some-token")
	h.Revoke(rr, newRevokeRequest(form))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (unauthorized_client) unwrapped from *OAuthError, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeUnauthorizedClient)
}

// TestRevoke_UnwrapsWrappedOAuthError pins the wrapping path
// (errors.As, not errors.Is) so a service-level wrapped error
// like `fmt.Errorf("revoke: %w", &OAuthError{...})` still surfaces
// the inner envelope.
func TestRevoke_UnwrapsWrappedOAuthError(t *testing.T) {
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	svc.errRevoke = fmt.Errorf("revoke failed: %w", &OAuthError{
		Code:        ErrCodeUnauthorizedClient,
		Description: "token was issued to a different client",
		HTTPStatus:  http.StatusBadRequest,
	})
	h := newHandlersWithAPI(svc, userResolverOK)
	rr := httptest.NewRecorder()
	form := url.Values{}
	form.Set("client_id", "client-conf-1")
	form.Set("client_secret", "any")
	form.Set("token", "some-token")
	h.Revoke(rr, newRevokeRequest(form))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 unwrapped via errors.As, got %d", rr.Code)
	}
	assertOAuthErrorJSON(t, rr.Body.Bytes(), ErrCodeUnauthorizedClient)
}

// =================== RegisterRoutes ===================

func TestRegisterRoutes_AllFourEndpointsBound(t *testing.T) {
	mux := http.NewServeMux()
	svc := newFakeService()
	svc.clients["client-conf-1"] = makeConfidentialClient()
	h := newHandlersWithAPI(svc, userResolverOK)
	h.RegisterRoutes(mux, "/oauth")

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Each endpoint should respond (with the appropriate
	// validation 4xx) rather than 404 from the mux.
	cases := []struct {
		method, path string
		wantStatus   int
	}{
		{"GET", "/oauth/authorize", http.StatusBadRequest},
		// 403 (not 400) for the bare /approve probe because the
		// CSRF double-submit gate fires before form validation —
		// any POST without a matched cookie+form pair is now
		// rejected. This is the desired ordering: cheap deny
		// before any service-layer work.
		{"POST", "/oauth/authorize/approve", http.StatusForbidden},
		{"POST", "/oauth/token", http.StatusUnauthorized},
		{"POST", "/oauth/revoke", http.StatusUnauthorized},
	}
	for _, c := range cases {
		req, _ := http.NewRequest(c.method, srv.URL+c.path, nil)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", c.method, c.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.wantStatus {
			t.Errorf("%s %s: expected %d, got %d", c.method, c.path, c.wantStatus, resp.StatusCode)
		}
	}
}

// =================== Helpers ===================

// newApproveRequest builds an Approve POST with a matched
// CSRF cookie + hidden form field so the double-submit check
// passes. Tests that want to exercise CSRF rejection should
// construct the request inline instead of using this helper.
func newApproveRequest(form url.Values) *http.Request {
	const csrf = "test-csrf-nonce-deadbeef"
	if form == nil {
		form = url.Values{}
	}
	form.Set("csrf_token", csrf)
	req := httptest.NewRequest(http.MethodPost, "/oauth/authorize/approve",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
	return req
}

func newTokenRequest(form url.Values, basicAuth string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/oauth/token",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basicAuth != "" {
		req.Header.Set("Authorization", "Basic "+basicAuth)
	}
	return req
}

func newRevokeRequest(form url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/oauth/revoke",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// assertOAuthErrorJSON checks that the response body is a valid
// RFC 6749 error envelope carrying the expected error code.
func assertOAuthErrorJSON(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	var env struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not an OAuth error envelope: %v (body=%q)", err, string(body))
	}
	if env.Error != wantCode {
		t.Errorf("expected error=%q, got %q (body=%q)", wantCode, env.Error, string(body))
	}
}
