package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// =================== AccessTokenContext.HasScope ===================

func TestAccessTokenContext_HasScope_Exact(t *testing.T) {
	ctx := &AccessTokenContext{Scopes: []string{ScopeReadMail, ScopeReadCalendar}}
	if !ctx.HasScope(ScopeReadMail) {
		t.Fatal("expected ScopeReadMail to match")
	}
	if !ctx.HasScope(ScopeReadCalendar) {
		t.Fatal("expected ScopeReadCalendar to match")
	}
	if ctx.HasScope(ScopeWriteMail) {
		t.Fatal("ScopeWriteMail should NOT be implied by read-only scopes")
	}
}

func TestAccessTokenContext_HasScope_WriteImpliesRead(t *testing.T) {
	cases := []struct {
		granted string
		want    string
	}{
		{ScopeWriteMail, ScopeReadMail},
		{ScopeWriteCalendar, ScopeReadCalendar},
		{ScopeWriteContacts, ScopeReadContacts},
	}
	for _, c := range cases {
		ctx := &AccessTokenContext{Scopes: []string{c.granted}}
		if !ctx.HasScope(c.want) {
			t.Errorf("granted=%s should imply %s", c.granted, c.want)
		}
	}
}

func TestAccessTokenContext_HasScope_NoCrossResourceImply(t *testing.T) {
	// write:mail must NOT imply read:calendar (different resource).
	ctx := &AccessTokenContext{Scopes: []string{ScopeWriteMail}}
	if ctx.HasScope(ScopeReadCalendar) {
		t.Fatal("write:mail should not imply read:calendar")
	}
	if ctx.HasScope(ScopeReadContacts) {
		t.Fatal("write:mail should not imply read:contacts")
	}
}

func TestAccessTokenContext_HasScope_ReadNeverImpliesWrite(t *testing.T) {
	// The hierarchy is strictly write→read, never the other way.
	ctx := &AccessTokenContext{Scopes: []string{ScopeReadMail, ScopeReadCalendar, ScopeReadContacts}}
	for _, want := range []string{ScopeWriteMail, ScopeWriteCalendar, ScopeWriteContacts} {
		if ctx.HasScope(want) {
			t.Errorf("read scope should not imply %s", want)
		}
	}
}

func TestAccessTokenContext_HasScope_EmptyScopes(t *testing.T) {
	ctx := &AccessTokenContext{Scopes: nil}
	if ctx.HasScope(ScopeReadMail) {
		t.Fatal("empty scope list should not match anything")
	}
}

// =================== OAuthError ===================

func TestOAuthError_ErrorString(t *testing.T) {
	t.Run("with description", func(t *testing.T) {
		e := &OAuthError{Code: ErrCodeInvalidGrant, Description: "code expired"}
		if got, want := e.Error(), "invalid_grant: code expired"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("without description", func(t *testing.T) {
		e := &OAuthError{Code: ErrCodeInvalidClient}
		if got, want := e.Error(), "invalid_client"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestOAuthError_ErrorsIs(t *testing.T) {
	// OAuthError must NOT collapse to sentinel errors — they're
	// distinct error categories. This guards against an accidental
	// `Unwrap` being added that could break mapTokenError.
	e := &OAuthError{Code: ErrCodeInvalidGrant}
	if errors.Is(e, ErrCodeNotFound) {
		t.Fatal("OAuthError should not match the sentinel by default")
	}
}

// =================== verifyPKCE ===================

func TestVerifyPKCE_S256(t *testing.T) {
	// Verifier per RFC 7636 §4.1: 43-128 URL-safe chars. Use a
	// known-good vector.
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	if !verifyPKCE(challenge, verifier, CodeChallengeMethodS256) {
		t.Fatal("S256 with matching verifier should verify")
	}
	if verifyPKCE(challenge, "wrong-verifier", CodeChallengeMethodS256) {
		t.Fatal("S256 with wrong verifier should NOT verify")
	}
}

func TestVerifyPKCE_Plain(t *testing.T) {
	if !verifyPKCE("the-challenge", "the-challenge", CodeChallengeMethodPlain) {
		t.Fatal("plain with matching verifier should verify")
	}
	if verifyPKCE("the-challenge", "the-challenge ", CodeChallengeMethodPlain) {
		t.Fatal("plain compare is exact — trailing space must not match")
	}
}

func TestVerifyPKCE_UnknownMethod(t *testing.T) {
	if verifyPKCE("foo", "foo", "MD5") {
		t.Fatal("unknown method must never succeed")
	}
	if verifyPKCE("foo", "foo", "") {
		t.Fatal("empty method must never succeed")
	}
}

// =================== URL validators ===================

// TestValidateRedirectURI pins the RFC 6749 §3.1.2 + RFC 8252 §7.1
// requirements that this package enforces at client-registration
// time. The validator is defence-in-depth for the consent screen's
// auto-escaping URL filter and the /authorize redirect-target
// guard: catching a bad scheme at registration means we never
// serve a "broken link to #ZgotmplZ" page later.
func TestValidateRedirectURI(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"https accepted", "https://example.com/cb", false},
		{"http accepted (localhost dev per RFC 8252 §8.3)", "http://localhost:8080/cb", false},
		{"reverse-dns native scheme accepted", "com.example.app://callback", false},
		{"empty string rejected", "", true},
		{"no scheme rejected", "example.com/cb", true},
		{"javascript scheme rejected", "javascript:alert(1)", true},
		{"data URI rejected", "data:text/html,<script>alert(1)</script>", true},
		{"vbscript scheme rejected", "vbscript:msgbox(1)", true},
		{"file scheme rejected", "file:///etc/passwd", true},
		{"single-token custom scheme rejected (not reverse-DNS)", "mycustom://cb", true},
		{"JAVASCRIPT (case-insensitive) rejected", "JAVASCRIPT:alert(1)", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRedirectURI(tc.raw)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %q, got nil", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error for %q, got %v", tc.raw, err)
			}
		})
	}
}

// TestValidateExternalDisplayURL pins the homepage_url / logo_url
// rules: http or https only, host required. Defence-in-depth atop
// html/template's URL filter for href/src contexts.
func TestValidateExternalDisplayURL(t *testing.T) {
	cases := []struct {
		name    string
		field   string
		raw     string
		wantErr bool
	}{
		{"https homepage accepted", "homepage_url", "https://example.com/", false},
		{"http homepage accepted (intranet)", "homepage_url", "http://intranet.local/", false},
		{"https logo accepted", "logo_url", "https://cdn.example.com/logo.png", false},
		{"javascript: rejected", "homepage_url", "javascript:alert(1)", true},
		{"data: rejected", "logo_url", "data:image/png;base64,iVBORw0KGgo=", true},
		{"missing host rejected", "homepage_url", "https:///nohost", true},
		{"unparseable rejected", "logo_url", "://not-a-url", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateExternalDisplayURL(tc.field, tc.raw)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for field=%s raw=%q, got nil", tc.field, tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error for field=%s raw=%q, got %v", tc.field, tc.raw, err)
			}
		})
	}
}

// =================== generateOpaqueToken / hashToken ===================

func TestGenerateOpaqueToken_Length(t *testing.T) {
	for _, n := range []int{16, 24, 32, 48} {
		tok, err := generateOpaqueToken(n)
		if err != nil {
			t.Fatalf("generateOpaqueToken(%d): %v", n, err)
		}
		// base64 raw URL encoding: ceil(n / 3) * 4 with no padding,
		// so 32 bytes → 43 chars, 16 → 22, 24 → 32.
		decoded, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("token %q not valid base64url: %v", tok, err)
		}
		if len(decoded) != n {
			t.Fatalf("expected %d decoded bytes, got %d", n, len(decoded))
		}
	}
}

func TestGenerateOpaqueToken_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 1024)
	for i := 0; i < 1024; i++ {
		tok, err := generateOpaqueToken(32)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token at iter %d: %q", i, tok)
		}
		seen[tok] = struct{}{}
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	a := hashToken("hello")
	b := hashToken("hello")
	if a != b {
		t.Fatalf("hashToken not deterministic: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("expected 64 hex chars (SHA-256), got %d", len(a))
	}
}

func TestHashToken_DifferentInputs(t *testing.T) {
	a := hashToken("hello")
	b := hashToken("Hello") // case-sensitive
	if a == b {
		t.Fatal("hashToken should be case-sensitive")
	}
}

// =================== redirectURIInAllowList ===================

func TestRedirectURIInAllowList_ExactMatch(t *testing.T) {
	allow := []string{
		"https://app.example.com/callback",
		"https://app.example.com/oauth/return",
	}
	if !redirectURIInAllowList(allow, "https://app.example.com/callback") {
		t.Fatal("exact match should succeed")
	}
	if !redirectURIInAllowList(allow, "https://app.example.com/oauth/return") {
		t.Fatal("exact match for second URI should succeed")
	}
}

func TestRedirectURIInAllowList_NoPrefixMatching(t *testing.T) {
	// RFC 6749 §3.1.2.4: redirect URI matching is exact — no
	// prefix matching, no query merging. Anything else is an
	// open-redirect attack vector.
	allow := []string{"https://app.example.com/callback"}
	bad := []string{
		"https://app.example.com/callback/evil",
		"https://app.example.com/callback?evil=1",
		"https://app.example.com.attacker.com/callback",
		"http://app.example.com/callback", // scheme mismatch
		"https://APP.EXAMPLE.COM/callback", // case-sensitive comparison
	}
	for _, b := range bad {
		if redirectURIInAllowList(allow, b) {
			t.Errorf("redirect %q must NOT match %v", b, allow)
		}
	}
}

func TestRedirectURIInAllowList_EmptyAllow(t *testing.T) {
	if redirectURIInAllowList(nil, "https://app.example.com/callback") {
		t.Fatal("empty allow list must reject all URIs")
	}
}

// =================== joinScopes / containsString ===================

func TestJoinScopes(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{ScopeReadMail}, "read:mail"},
		{[]string{ScopeReadMail, ScopeWriteMail}, "read:mail write:mail"},
	}
	for _, c := range cases {
		if got := joinScopes(c.in); got != c.want {
			t.Errorf("joinScopes(%v): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestContainsString(t *testing.T) {
	hay := []string{ScopeReadMail, ScopeWriteCalendar}
	if !containsString(hay, ScopeReadMail) {
		t.Fatal("should find ScopeReadMail")
	}
	if containsString(hay, ScopeReadContacts) {
		t.Fatal("should not find ScopeReadContacts")
	}
	if containsString(nil, ScopeReadMail) {
		t.Fatal("nil haystack must never match")
	}
}

// =================== Service constructor ===================

func TestNewService_DefaultTTLs(t *testing.T) {
	s := NewService(nil)
	if s.accessTokenTTL != AccessTokenTTL {
		t.Errorf("accessTokenTTL: got %v, want %v", s.accessTokenTTL, AccessTokenTTL)
	}
	if s.refreshTokenTTL != RefreshTokenTTL {
		t.Errorf("refreshTokenTTL: got %v, want %v", s.refreshTokenTTL, RefreshTokenTTL)
	}
	if s.codeTTL != AuthorizationCodeTTL {
		t.Errorf("codeTTL: got %v, want %v", s.codeTTL, AuthorizationCodeTTL)
	}
	if s.now == nil {
		t.Fatal("now func should default to time.Now, not nil")
	}
	// Calling now() should produce a non-zero time.
	if s.now().IsZero() {
		t.Fatal("default now() returned zero time")
	}
}

func TestService_WithClock(t *testing.T) {
	fixed := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)
	s := NewService(nil).WithClock(func() time.Time { return fixed })
	if got := s.now(); !got.Equal(fixed) {
		t.Errorf("WithClock not honoured: got %v, want %v", got, fixed)
	}
}

// =================== Sentinel error stability ===================

func TestSentinelErrors_HaveStableMessages(t *testing.T) {
	// These messages are part of the package's external contract
	// (the BFF logs them; ops tooling may grep for them). Pin them
	// so a careless edit shows up in CI.
	cases := map[error]string{
		ErrClientNotFound:         "oauth: client not found",
		ErrCodeNotFound:           "oauth: authorization code not found or expired",
		ErrCodeAlreadyConsumed:    "oauth: authorization code already used",
		ErrAccessTokenNotFound:    "oauth: access token not found or revoked",
		ErrRefreshTokenNotFound:   "oauth: refresh token not found or revoked",
		ErrInvalidRedirectURI:     "oauth: redirect_uri does not match allow-list",
		ErrInvalidCodeVerifier:    "oauth: code_verifier does not match challenge",
		ErrScopeNotAllowed:        "oauth: requested scope not in client allow-list",
		ErrClientSecretMismatch:   "oauth: client_secret mismatch",
		ErrPKCERequiredButMissing: "oauth: PKCE required for public clients",
		ErrRefreshTokenReplay:     "oauth: refresh token replay detected — token family revoked",
	}
	for err, want := range cases {
		if got := err.Error(); got != want {
			t.Errorf("%v: got %q, want %q", err, got, want)
		}
	}
}

// =================== KnownScopes coverage ===================

func TestKnownScopes_IncludesAll(t *testing.T) {
	want := []string{
		ScopeReadMail, ScopeWriteMail,
		ScopeReadCalendar, ScopeWriteCalendar,
		ScopeReadContacts, ScopeWriteContacts,
		ScopeReadProfile,
	}
	for _, sc := range want {
		if _, ok := KnownScopes[sc]; !ok {
			t.Errorf("scope %q missing from KnownScopes", sc)
		}
	}
	// A bare colon-separated string that *looks* like a scope but
	// isn't registered should NOT be in KnownScopes — guards
	// against accidental shadowing.
	if _, ok := KnownScopes["read:everything"]; ok {
		t.Error("unexpected scope read:everything in KnownScopes")
	}
}

// =================== Constants pinned ===================

func TestTokenLifetimes_AreSane(t *testing.T) {
	// Pin the spec-aligned defaults. If someone bumps these
	// without updating docs the test prompts a discussion.
	if AccessTokenTTL != time.Hour {
		t.Errorf("AccessTokenTTL: got %v, want 1h", AccessTokenTTL)
	}
	if RefreshTokenTTL != 30*24*time.Hour {
		t.Errorf("RefreshTokenTTL: got %v, want 30d", RefreshTokenTTL)
	}
	if AuthorizationCodeTTL != 60*time.Second {
		t.Errorf("AuthorizationCodeTTL: got %v, want 60s", AuthorizationCodeTTL)
	}
}

func TestClientType_Constants(t *testing.T) {
	if ClientTypeConfidential != "confidential" {
		t.Errorf("ClientTypeConfidential changed value")
	}
	if ClientTypePublic != "public" {
		t.Errorf("ClientTypePublic changed value")
	}
}

func TestGrantType_Constants(t *testing.T) {
	if GrantTypeAuthorizationCode != "authorization_code" {
		t.Errorf("GrantTypeAuthorizationCode changed value")
	}
	if GrantTypeRefreshToken != "refresh_token" {
		t.Errorf("GrantTypeRefreshToken changed value")
	}
}

// =================== splitScopes ===================

func TestSplitScopes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"   ", nil},
		{"read:mail", []string{"read:mail"}},
		{"read:mail write:mail", []string{"read:mail", "write:mail"}},
		{"  read:mail   write:mail  ", []string{"read:mail", "write:mail"}},
		// Tab and other whitespace per strings.Fields rules.
		{"read:mail\twrite:mail", []string{"read:mail", "write:mail"}},
	}
	for _, c := range cases {
		got := splitScopes(c.in)
		if !equalStringSlices(got, c.want) {
			t.Errorf("splitScopes(%q): got %v, want %v", c.in, got, c.want)
		}
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// =================== mapTokenError ===================

func TestMapTokenError_Sentinels(t *testing.T) {
	cases := map[error]string{
		ErrCodeNotFound:           ErrCodeInvalidGrant,
		ErrCodeAlreadyConsumed:    ErrCodeInvalidGrant,
		ErrInvalidRedirectURI:     ErrCodeInvalidGrant,
		ErrInvalidCodeVerifier:    ErrCodeInvalidGrant,
		ErrRefreshTokenNotFound:   ErrCodeInvalidGrant,
		ErrRefreshTokenReplay:     ErrCodeInvalidGrant,
		ErrPKCERequiredButMissing: ErrCodeInvalidGrant,
		ErrScopeNotAllowed:        ErrCodeInvalidScope,
	}
	for in, wantCode := range cases {
		got := mapTokenError(in)
		if got.Code != wantCode {
			t.Errorf("mapTokenError(%v): code=%s, want %s", in, got.Code, wantCode)
		}
	}
}

func TestMapTokenError_PassesThroughOAuthError(t *testing.T) {
	original := &OAuthError{Code: ErrCodeInvalidClient, Description: "x"}
	got := mapTokenError(original)
	if got != original {
		t.Fatal("mapTokenError should pass through *OAuthError unchanged")
	}
}

func TestMapTokenError_UnknownErrorCollapsesToInvalidGrant(t *testing.T) {
	// Unknown errors must collapse to invalid_grant (not
	// server_error) so we don't leak internal error text to a
	// third-party client.
	got := mapTokenError(errors.New("some internal db error"))
	if got.Code != ErrCodeInvalidGrant {
		t.Errorf("unknown error: got code %s, want %s", got.Code, ErrCodeInvalidGrant)
	}
	if strings.Contains(got.Description, "db") {
		t.Errorf("description leaked internal text: %q", got.Description)
	}
}
