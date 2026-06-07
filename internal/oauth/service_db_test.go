package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func oauthService(t *testing.T) (*Service, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	return NewService(pool), tenant
}

// seedUser inserts a real user row (oauth tables FK user_id -> users.id).
func seedUser(t *testing.T, svc *Service, tenant string) string {
	t.Helper()
	ctx := context.Background()
	u := fmt.Sprintf("%d", time.Now().UnixNano())
	var id string
	err := pgx.BeginFunc(ctx, svc.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenant); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO users (tenant_id, kchat_user_id, stalwart_account_id, email, display_name)
			VALUES ($1::uuid, $2, $3, $4, $5) RETURNING id::text
		`, tenant, "kc-"+u, "sw-"+u, "u-"+u+"@example.com", "User "+u).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return id
}

func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestOAuthRegisterValidationDB(t *testing.T) {
	svc, tenant := oauthService(t)
	ctx := context.Background()

	cases := []struct {
		name        string
		tenantID    string
		clientName  string
		clientType  string
		redirects   []string
		scopes      []string
	}{
		{"no tenant", "", "App", ClientTypeConfidential, []string{"https://a/cb"}, nil},
		{"no name", tenant, "", ClientTypeConfidential, []string{"https://a/cb"}, nil},
		{"bad type", tenant, "App", "weird", []string{"https://a/cb"}, nil},
		{"no redirect", tenant, "App", ClientTypeConfidential, nil, nil},
		{"unknown scope", tenant, "App", ClientTypeConfidential, []string{"https://a/cb"}, []string{"read:everything"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := svc.RegisterClient(ctx, c.tenantID, c.clientName, c.clientType, c.redirects, c.scopes, "", ""); err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
		})
	}
}

func TestOAuthAuthCodeFlowDB(t *testing.T) {
	svc, tenant := oauthService(t)
	ctx := context.Background()
	redirect := "https://app.example.com/callback"
	scopes := []string{ScopeReadMail, ScopeReadProfile}

	client, secret, err := svc.RegisterClient(ctx, tenant, "My App", ClientTypeConfidential, []string{redirect}, scopes, "", "")
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	if secret == "" {
		t.Fatal("confidential client must return a secret")
	}

	// GetClient round-trips scopes/redirects.
	got, err := svc.GetClient(ctx, tenant, client.ClientID)
	if err != nil || len(got.AllowedScopes) != 2 {
		t.Fatalf("GetClient: %v %+v", err, got)
	}

	// Secret verification.
	if err := svc.VerifyClientSecret(ctx, client, secret); err != nil {
		t.Errorf("VerifyClientSecret correct: %v", err)
	}
	if err := svc.VerifyClientSecret(ctx, client, "wrong"); err == nil {
		t.Error("VerifyClientSecret wrong: expected error")
	}

	// LookupClientForExchange (no tenant prefix).
	if lc, err := svc.LookupClientForExchange(ctx, client.ClientID); err != nil || lc.ID != client.ID {
		t.Fatalf("LookupClientForExchange: %v %+v", err, lc)
	}

	// Issue an authorization code with PKCE (S256).
	verifier := "this-is-a-sufficiently-long-code-verifier-1234567890"
	userID := seedUser(t, svc, tenant)
	code, err := svc.IssueAuthorizationCode(ctx, client, userID, redirect, []string{ScopeReadMail}, s256Challenge(verifier), CodeChallengeMethodS256)
	if err != nil {
		t.Fatalf("IssueAuthorizationCode: %v", err)
	}

	// Wrong redirect URI is rejected at issue time.
	if _, err := svc.IssueAuthorizationCode(ctx, client, userID, "https://evil/cb", []string{ScopeReadMail}, "", ""); !errors.Is(err, ErrInvalidRedirectURI) {
		t.Errorf("issue bad redirect: want ErrInvalidRedirectURI got %v", err)
	}

	// Exchange with wrong PKCE verifier fails.
	if _, err := svc.ExchangeAuthorizationCode(ctx, client, code, redirect, "wrong-verifier"); err == nil {
		t.Error("exchange wrong verifier: expected error")
	}

	// Exchange with correct verifier succeeds.
	tok, err := svc.ExchangeAuthorizationCode(ctx, client, code, redirect, verifier)
	if err != nil {
		t.Fatalf("ExchangeAuthorizationCode: %v", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("token response incomplete: %+v", tok)
	}

	// The code is single-use.
	if _, err := svc.ExchangeAuthorizationCode(ctx, client, code, redirect, verifier); err == nil {
		t.Error("re-exchange consumed code: expected error")
	}

	// Validate the access token.
	actx, err := svc.ValidateAccessToken(ctx, tok.AccessToken)
	if err != nil {
		t.Fatalf("ValidateAccessToken: %v", err)
	}
	if actx.TenantID != tenant || actx.UserID != userID {
		t.Errorf("token context wrong: %+v", actx)
	}

	// Refresh rotates the token pair.
	refreshed, err := svc.RefreshAccessToken(ctx, client, tok.RefreshToken)
	if err != nil {
		t.Fatalf("RefreshAccessToken: %v", err)
	}
	if refreshed.AccessToken == tok.AccessToken {
		t.Error("refresh did not rotate access token")
	}
	// Old refresh token is now invalid (rotated).
	if _, err := svc.RefreshAccessToken(ctx, client, tok.RefreshToken); err == nil {
		t.Error("reused refresh token: expected error")
	}

	// Revoke the new access token; it then fails validation.
	if err := svc.RevokeToken(ctx, client, refreshed.AccessToken); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := svc.ValidateAccessToken(ctx, refreshed.AccessToken); !errors.Is(err, ErrAccessTokenNotFound) {
		t.Errorf("validate revoked: want ErrAccessTokenNotFound got %v", err)
	}
}
