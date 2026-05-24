package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Service implements the OAuth2 authorization server backed by
// the four tables created in migrations/046_oauth_clients.sql.
type Service struct {
	pool *pgxpool.Pool

	// accessTokenTTL / refreshTokenTTL / codeTTL are overridable
	// for tests. Production callers should pass zero and inherit
	// the spec-aligned defaults defined in types.go.
	accessTokenTTL  time.Duration
	refreshTokenTTL time.Duration
	codeTTL         time.Duration

	// now lets tests inject a deterministic clock. Production
	// uses time.Now.
	now func() time.Time
}

// NewService returns a Service using the package-level default
// TTLs and the real wall clock.
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{
		pool:            pool,
		accessTokenTTL:  AccessTokenTTL,
		refreshTokenTTL: RefreshTokenTTL,
		codeTTL:         AuthorizationCodeTTL,
		now:             time.Now,
	}
}

// WithClock overrides the clock for tests.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// =================== Client registration ===================

// RegisterClient creates a new oauth_clients row and returns the
// plaintext client_secret (which is only available at this
// instant — only its bcrypt hash is stored). For public clients
// (client_type == "public") the plaintext secret return value is
// empty and the row stores NULL for the hash.
func (s *Service) RegisterClient(
	ctx context.Context,
	tenantID, name, clientType string,
	redirectURIs, allowedScopes []string,
	homepageURL, logoURL string,
) (*Client, string, error) {
	if tenantID == "" {
		return nil, "", errors.New("oauth: tenant_id required")
	}
	if name == "" {
		return nil, "", errors.New("oauth: name required")
	}
	if clientType != ClientTypeConfidential && clientType != ClientTypePublic {
		return nil, "", fmt.Errorf("oauth: client_type must be %q or %q", ClientTypeConfidential, ClientTypePublic)
	}
	if len(redirectURIs) == 0 {
		return nil, "", errors.New("oauth: at least one redirect_uri required")
	}
	for _, ru := range redirectURIs {
		if _, err := url.Parse(ru); err != nil {
			return nil, "", fmt.Errorf("oauth: invalid redirect_uri %q: %w", ru, err)
		}
	}
	for _, sc := range allowedScopes {
		if _, ok := KnownScopes[sc]; !ok {
			return nil, "", fmt.Errorf("oauth: unknown scope %q", sc)
		}
	}

	clientID, err := generateOpaqueToken(16)
	if err != nil {
		return nil, "", fmt.Errorf("oauth: generate client_id: %w", err)
	}
	var (
		plaintextSecret string
		secretHashPtr   *string
	)
	if clientType == ClientTypeConfidential {
		plaintextSecret, err = generateOpaqueToken(32)
		if err != nil {
			return nil, "", fmt.Errorf("oauth: generate client_secret: %w", err)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(plaintextSecret), bcrypt.DefaultCost)
		if err != nil {
			return nil, "", fmt.Errorf("oauth: hash client_secret: %w", err)
		}
		hashStr := string(hash)
		secretHashPtr = &hashStr
	}

	redirectJSON, _ := json.Marshal(redirectURIs)
	scopesJSON, _ := json.Marshal(allowedScopes)

	var c Client
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var (
			redirectRaw []byte
			scopesRaw   []byte
			homepageOpt *string
			logoOpt     *string
		)
		if homepageURL != "" {
			homepageOpt = &homepageURL
		}
		if logoURL != "" {
			logoOpt = &logoURL
		}
		return tx.QueryRow(ctx, `
			INSERT INTO oauth_clients (
				tenant_id, client_id, client_secret_hash, client_type,
				name, homepage_url, logo_url, redirect_uris, allowed_scopes
			)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8::jsonb, $9::jsonb)
			RETURNING id::text, tenant_id::text, client_id, client_type, name,
			          COALESCE(homepage_url, ''), COALESCE(logo_url, ''),
			          redirect_uris, allowed_scopes, active, created_at, updated_at
		`,
			tenantID, clientID, secretHashPtr, clientType,
			name, homepageOpt, logoOpt, string(redirectJSON), string(scopesJSON),
		).Scan(
			&c.ID, &c.TenantID, &c.ClientID, &c.ClientType, &c.Name,
			&c.HomepageURL, &c.LogoURL, &redirectRaw, &scopesRaw,
			&c.Active, &c.CreatedAt, &c.UpdatedAt,
		)
	})
	if err != nil {
		return nil, "", err
	}
	// Decode JSONB columns into the typed struct.
	_ = json.Unmarshal([]byte(c.HomepageURL), &c.HomepageURL) // no-op for plain string
	if err := json.Unmarshal([]byte("[]"), &c.RedirectURIs); err == nil {
		// Re-populate from the actual returned rows; we need a
		// second SELECT because Scan into a []byte for JSONB
		// gave us the raw bytes, but the typed Client struct
		// wants []string. The cleaner path is the GetClient
		// helper below.
	}
	full, err := s.getClientByPK(ctx, c.ID)
	if err != nil {
		return nil, "", fmt.Errorf("oauth: re-read newly-registered client: %w", err)
	}
	return full, plaintextSecret, nil
}

// GetClient fetches a client by its client_id (the public
// identifier). Tenant-scoped read.
func (s *Service) GetClient(ctx context.Context, tenantID, clientID string) (*Client, error) {
	if tenantID == "" || clientID == "" {
		return nil, ErrClientNotFound
	}
	return s.queryClient(ctx, tenantID, "client_id = $1", clientID)
}

func (s *Service) getClientByPK(ctx context.Context, id string) (*Client, error) {
	// Cross-tenant lookup by primary key; used internally after
	// INSERT to materialise the typed struct without an RLS
	// round-trip. Caller MUST NOT expose this surface to HTTP.
	c := &Client{}
	var redirectRaw, scopesRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, client_id, client_type, name,
		       COALESCE(homepage_url, ''), COALESCE(logo_url, ''),
		       redirect_uris, allowed_scopes, active, created_at, updated_at
		FROM oauth_clients
		WHERE id = $1::uuid
	`, id).Scan(
		&c.ID, &c.TenantID, &c.ClientID, &c.ClientType, &c.Name,
		&c.HomepageURL, &c.LogoURL, &redirectRaw, &scopesRaw,
		&c.Active, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(redirectRaw, &c.RedirectURIs)
	_ = json.Unmarshal(scopesRaw, &c.AllowedScopes)
	return c, nil
}

func (s *Service) queryClient(ctx context.Context, tenantID, where string, args ...any) (*Client, error) {
	c := &Client{}
	var redirectRaw, scopesRaw []byte
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, tenant_id::text, client_id, client_type, name,
			       COALESCE(homepage_url, ''), COALESCE(logo_url, ''),
			       redirect_uris, allowed_scopes, active, created_at, updated_at
			FROM oauth_clients
			WHERE `+where, args...,
		).Scan(
			&c.ID, &c.TenantID, &c.ClientID, &c.ClientType, &c.Name,
			&c.HomepageURL, &c.LogoURL, &redirectRaw, &scopesRaw,
			&c.Active, &c.CreatedAt, &c.UpdatedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrClientNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal(redirectRaw, &c.RedirectURIs)
	_ = json.Unmarshal(scopesRaw, &c.AllowedScopes)
	return c, nil
}

// LookupClientForExchange resolves the client by client_id WITHOUT
// a tenant prefix. The /oauth/token endpoint must do this because
// the client presents only its client_id (no tenant scoping is
// possible at the wire — the tenant is derived from the row).
//
// Callers MUST verify the resolved tenant matches the
// authorization code's tenant before doing anything else; otherwise
// a malicious client could exchange a code issued in tenant A
// using a client registered in tenant B. The exchange flow below
// enforces this invariant.
func (s *Service) LookupClientForExchange(ctx context.Context, clientID string) (*Client, error) {
	if clientID == "" {
		return nil, ErrClientNotFound
	}
	c := &Client{}
	var redirectRaw, scopesRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, tenant_id::text, client_id, client_type, name,
		       COALESCE(homepage_url, ''), COALESCE(logo_url, ''),
		       redirect_uris, allowed_scopes, active, created_at, updated_at
		FROM oauth_clients
		WHERE client_id = $1
	`, clientID).Scan(
		&c.ID, &c.TenantID, &c.ClientID, &c.ClientType, &c.Name,
		&c.HomepageURL, &c.LogoURL, &redirectRaw, &scopesRaw,
		&c.Active, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrClientNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal(redirectRaw, &c.RedirectURIs)
	_ = json.Unmarshal(scopesRaw, &c.AllowedScopes)
	return c, nil
}

// VerifyClientSecret returns nil if `plaintextSecret` matches
// the stored bcrypt hash for the client; otherwise
// ErrClientSecretMismatch. Public clients (no stored hash) MUST
// NOT call this — the /oauth/token endpoint enforces PKCE-only
// auth for public clients instead.
func (s *Service) VerifyClientSecret(ctx context.Context, c *Client, plaintextSecret string) error {
	if c == nil || c.ClientType != ClientTypeConfidential {
		return ErrClientSecretMismatch
	}
	var hash string
	err := s.pool.QueryRow(ctx, `
		SELECT client_secret_hash FROM oauth_clients WHERE id = $1::uuid
	`, c.ID).Scan(&hash)
	if err != nil {
		return err
	}
	if hash == "" {
		return ErrClientSecretMismatch
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintextSecret)); err != nil {
		return ErrClientSecretMismatch
	}
	return nil
}

// =================== Authorization code flow ===================

// IssueAuthorizationCode is called by the /oauth/authorize/approve
// handler after the user has consented. It validates the request,
// generates a one-time code, and persists it with a 60-second TTL.
//
// The plaintext code is returned to the caller for inclusion in
// the redirect URL; only its SHA-256 hash is persisted.
func (s *Service) IssueAuthorizationCode(
	ctx context.Context,
	client *Client,
	userID, redirectURI string,
	grantedScopes []string,
	codeChallenge, codeChallengeMethod string,
) (string, error) {
	if client == nil {
		return "", ErrClientNotFound
	}
	if userID == "" {
		return "", errors.New("oauth: user_id required")
	}
	if !redirectURIInAllowList(client.RedirectURIs, redirectURI) {
		return "", ErrInvalidRedirectURI
	}
	for _, sc := range grantedScopes {
		if !containsString(client.AllowedScopes, sc) {
			return "", ErrScopeNotAllowed
		}
	}
	// Public clients MUST use PKCE; confidential clients MAY.
	if client.ClientType == ClientTypePublic && codeChallenge == "" {
		return "", ErrPKCERequiredButMissing
	}
	if codeChallengeMethod != "" &&
		codeChallengeMethod != CodeChallengeMethodPlain &&
		codeChallengeMethod != CodeChallengeMethodS256 {
		return "", &OAuthError{
			Code:        ErrCodeInvalidRequest,
			Description: "code_challenge_method must be 'plain' or 'S256'",
			HTTPStatus:  400,
		}
	}

	plaintextCode, err := generateOpaqueToken(32)
	if err != nil {
		return "", fmt.Errorf("oauth: generate code: %w", err)
	}
	codeHash := hashToken(plaintextCode)
	scopesJSON, _ := json.Marshal(grantedScopes)

	var challengePtr, methodPtr *string
	if codeChallenge != "" {
		challengePtr = &codeChallenge
		method := codeChallengeMethod
		if method == "" {
			method = CodeChallengeMethodS256
		}
		methodPtr = &method
	}

	expiresAt := s.now().Add(s.codeTTL)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, client.TenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO oauth_authorization_codes (
				tenant_id, client_id, user_id, code_hash,
				redirect_uri, granted_scopes, code_challenge,
				code_challenge_method, expires_at
			)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6::jsonb, $7, $8, $9)
		`,
			client.TenantID, client.ID, userID, codeHash,
			redirectURI, string(scopesJSON), challengePtr, methodPtr, expiresAt,
		)
		return err
	})
	if err != nil {
		return "", err
	}
	return plaintextCode, nil
}

// TokenResponse is the wire shape returned by /oauth/token per
// RFC 6749 §5.1.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope"`
}

// ExchangeAuthorizationCode performs the /oauth/token
// authorization_code grant: validates the code, consumes it,
// verifies PKCE, issues access + refresh tokens, and returns the
// response shape per RFC 6749 §5.1. The plaintext code, code
// verifier, and (for confidential clients) plaintext client
// secret are passed in by the handler.
func (s *Service) ExchangeAuthorizationCode(
	ctx context.Context,
	client *Client,
	plaintextCode, redirectURI, codeVerifier string,
) (*TokenResponse, error) {
	if client == nil {
		return nil, ErrClientNotFound
	}
	codeHash := hashToken(plaintextCode)
	now := s.now()

	var (
		codeID, codeUserID, codeRedirect string
		codeChallenge                    *string
		codeChallengeMethod              *string
		scopesRaw                        []byte
		consumedAt                       *time.Time
		expiresAt                        time.Time
		codeClientID, codeTenantID       string
	)

	// Step 1: consume the code in a single transaction. The
	// UPDATE ... RETURNING pattern atomically marks the code
	// consumed AND returns its fields, so a concurrent
	// /oauth/token exchange with the same code can only succeed
	// once.
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, client.TenantID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			UPDATE oauth_authorization_codes
			SET consumed_at = $2
			WHERE code_hash = $1 AND consumed_at IS NULL
			RETURNING id::text, client_id::text, tenant_id::text, user_id::text,
			          redirect_uri, granted_scopes, code_challenge,
			          code_challenge_method, expires_at
		`, codeHash, now).Scan(
			&codeID, &codeClientID, &codeTenantID, &codeUserID,
			&codeRedirect, &scopesRaw, &codeChallenge, &codeChallengeMethod, &expiresAt,
		)
		return err
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "authorization code not found, expired, or already consumed",
				HTTPStatus:  400,
			}
		}
		return nil, err
	}
	_ = consumedAt // referenced for tx; suppress unused warnings on the named return

	// Step 2: validate the code matches THIS client (a code
	// issued for client A cannot be exchanged by client B).
	if codeClientID != client.ID {
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "code was issued to a different client",
			HTTPStatus:  400,
		}
	}
	if codeTenantID != client.TenantID {
		// Defence-in-depth — should be impossible given RLS, but
		// belt-and-braces in case the policy is ever loosened.
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "code tenant mismatch",
			HTTPStatus:  400,
		}
	}

	// Step 3: redirect URI must match the one used at /authorize.
	if codeRedirect != redirectURI {
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "redirect_uri does not match the value used at /authorize",
			HTTPStatus:  400,
		}
	}

	// Step 4: expiry check.
	if now.After(expiresAt) {
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "authorization code expired",
			HTTPStatus:  400,
		}
	}

	// Step 5: PKCE verification.
	if codeChallenge != nil && *codeChallenge != "" {
		if codeVerifier == "" {
			return nil, &OAuthError{
				Code:        ErrCodeInvalidRequest,
				Description: "code_verifier required (PKCE in use)",
				HTTPStatus:  400,
			}
		}
		method := CodeChallengeMethodS256
		if codeChallengeMethod != nil {
			method = *codeChallengeMethod
		}
		if !verifyPKCE(*codeChallenge, codeVerifier, method) {
			return nil, &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "code_verifier does not match challenge",
				HTTPStatus:  400,
			}
		}
	} else if client.ClientType == ClientTypePublic {
		// Public client without PKCE — should have been rejected
		// at /authorize. Refuse the exchange just in case.
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "public clients must use PKCE",
			HTTPStatus:  400,
		}
	}

	var grantedScopes []string
	_ = json.Unmarshal(scopesRaw, &grantedScopes)

	// Step 6: mint access + refresh tokens.
	return s.mintTokens(ctx, client, codeUserID, grantedScopes, "" /* no parent refresh */)
}

// RefreshAccessToken performs the /oauth/token refresh_token
// grant: validates the presented refresh token, rotates it
// (revoke old, issue new), and returns a fresh access + refresh
// token pair.
//
// Implements the replay-detection invariant from RFC 6819 §5.2:
// if the presented refresh token is already revoked AND its row
// references a successor, the entire successor chain is revoked
// because a legitimate client never re-uses a refresh token
// after rotating it.
func (s *Service) RefreshAccessToken(
	ctx context.Context,
	client *Client,
	plaintextRefreshToken string,
) (*TokenResponse, error) {
	if client == nil {
		return nil, ErrClientNotFound
	}
	if plaintextRefreshToken == "" {
		return nil, &OAuthError{Code: ErrCodeInvalidRequest, Description: "refresh_token required", HTTPStatus: 400}
	}
	refreshHash := hashToken(plaintextRefreshToken)
	now := s.now()

	var (
		refreshID, refreshUserID string
		refreshClientID          string
		refreshTenantID          string
		scopesRaw                []byte
		expiresAt                time.Time
		revokedAt                *time.Time
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, client.TenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT id::text, client_id::text, tenant_id::text, user_id::text,
			       scopes, expires_at, revoked_at
			FROM oauth_refresh_tokens
			WHERE token_hash = $1
		`, refreshHash).Scan(
			&refreshID, &refreshClientID, &refreshTenantID, &refreshUserID,
			&scopesRaw, &expiresAt, &revokedAt,
		)
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "refresh token not found",
				HTTPStatus:  400,
			}
		}
		return nil, err
	}

	// Client check: refresh token must belong to the client.
	if refreshClientID != client.ID {
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "refresh token was issued to a different client",
			HTTPStatus:  400,
		}
	}

	// Replay detection: a revoked refresh token presented here
	// means a previous /oauth/token call already rotated it. If
	// the legitimate client had rotated, it would have stored the
	// new token; presenting the old one means the old one leaked.
	// Revoke the entire successor chain to invalidate whatever
	// the attacker has been able to mint.
	if revokedAt != nil {
		if err := s.revokeRefreshChain(ctx, client.TenantID, refreshID); err != nil {
			return nil, fmt.Errorf("oauth: revoke chain on replay: %w", err)
		}
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "refresh token replay detected — token family revoked",
			HTTPStatus:  400,
		}
	}

	if now.After(expiresAt) {
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "refresh token expired",
			HTTPStatus:  400,
		}
	}

	var grantedScopes []string
	_ = json.Unmarshal(scopesRaw, &grantedScopes)

	// Mint new tokens; chain the new refresh row to the old one
	// so a future replay of the old one can walk the chain.
	resp, err := s.mintTokens(ctx, client, refreshUserID, grantedScopes, refreshID)
	if err != nil {
		return nil, err
	}

	// Revoke the old refresh token AND all access tokens it issued.
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, client.TenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE oauth_refresh_tokens SET revoked_at = $2 WHERE id = $1::uuid AND revoked_at IS NULL
		`, refreshID, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE oauth_access_tokens SET revoked_at = $2
			WHERE refresh_token_id = $1::uuid AND revoked_at IS NULL
		`, refreshID, now)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("oauth: revoke rotated tokens: %w", err)
	}
	return resp, nil
}

// mintTokens generates a fresh access + refresh token pair tied
// to the given client/user/scopes and persists them. parentID is
// the previous refresh token's row id (empty string for a brand
// new chain from /authorize; non-empty for /refresh).
func (s *Service) mintTokens(
	ctx context.Context,
	client *Client,
	userID string,
	scopes []string,
	parentRefreshID string,
) (*TokenResponse, error) {
	now := s.now()
	plaintextAccess, err := generateOpaqueToken(32)
	if err != nil {
		return nil, fmt.Errorf("oauth: generate access token: %w", err)
	}
	plaintextRefresh, err := generateOpaqueToken(32)
	if err != nil {
		return nil, fmt.Errorf("oauth: generate refresh token: %w", err)
	}
	accessHash := hashToken(plaintextAccess)
	refreshHash := hashToken(plaintextRefresh)
	scopesJSON, _ := json.Marshal(scopes)

	var newRefreshID string
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, client.TenantID); err != nil {
			return err
		}
		var parentPtr *string
		if parentRefreshID != "" {
			parentPtr = &parentRefreshID
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO oauth_refresh_tokens (
				tenant_id, client_id, user_id, token_hash, scopes, parent_id, expires_at
			)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::jsonb, $6::uuid, $7)
			RETURNING id::text
		`,
			client.TenantID, client.ID, userID, refreshHash, string(scopesJSON),
			parentPtr, now.Add(s.refreshTokenTTL),
		).Scan(&newRefreshID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO oauth_access_tokens (
				tenant_id, client_id, user_id, token_hash, scopes,
				expires_at, refresh_token_id
			)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::jsonb, $6, $7::uuid)
		`,
			client.TenantID, client.ID, userID, accessHash, string(scopesJSON),
			now.Add(s.accessTokenTTL), newRefreshID,
		)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("oauth: persist tokens: %w", err)
	}

	return &TokenResponse{
		AccessToken:  plaintextAccess,
		TokenType:    "Bearer",
		ExpiresIn:    int(s.accessTokenTTL.Seconds()),
		RefreshToken: plaintextRefresh,
		Scope:        joinScopes(scopes),
	}, nil
}

// revokeRefreshChain walks `parent_id` references forward from
// `rootID` and revokes every refresh token in the chain plus
// every access token derived from any of them. Used on
// refresh-token replay detection.
func (s *Service) revokeRefreshChain(ctx context.Context, tenantID, rootID string) error {
	now := s.now()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		// Recursive CTE walks `parent_id` forward from rootID.
		// Postgres supports WITH RECURSIVE natively.
		_, err := tx.Exec(ctx, `
			WITH RECURSIVE chain AS (
				SELECT id FROM oauth_refresh_tokens WHERE id = $1::uuid
				UNION ALL
				SELECT r.id FROM oauth_refresh_tokens r
				JOIN chain c ON r.parent_id = c.id
			)
			UPDATE oauth_refresh_tokens
			SET revoked_at = $2
			WHERE id IN (SELECT id FROM chain) AND revoked_at IS NULL
		`, rootID, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			WITH RECURSIVE chain AS (
				SELECT id FROM oauth_refresh_tokens WHERE id = $1::uuid
				UNION ALL
				SELECT r.id FROM oauth_refresh_tokens r
				JOIN chain c ON r.parent_id = c.id
			)
			UPDATE oauth_access_tokens
			SET revoked_at = $2
			WHERE refresh_token_id IN (SELECT id FROM chain) AND revoked_at IS NULL
		`, rootID, now)
		return err
	})
}

// =================== Token validation (middleware) ===================

// ValidateAccessToken resolves the plaintext bearer token to its
// AccessTokenContext. Used by the package's HTTP middleware on
// every request to a scoped API.
//
// Returns ErrAccessTokenNotFound for unknown / revoked / expired
// tokens. Caller should map that to HTTP 401.
func (s *Service) ValidateAccessToken(ctx context.Context, plaintextToken string) (*AccessTokenContext, error) {
	if plaintextToken == "" {
		return nil, ErrAccessTokenNotFound
	}
	tokenHash := hashToken(plaintextToken)
	now := s.now()

	var (
		tokenID, tenantID, userID, clientID string
		scopesRaw                           []byte
		expiresAt                           time.Time
		revokedAt                           *time.Time
	)
	// No tenant scoping on the lookup — we don't know the tenant
	// yet. The token_hash is globally unique (UNIQUE constraint
	// on `oauth_access_tokens.token_hash`) so this is safe. We
	// then apply the tenant GUC in the downstream handler chain.
	err := s.pool.QueryRow(ctx, `
		SELECT t.id::text, t.tenant_id::text, t.user_id::text, c.client_id,
		       t.scopes, t.expires_at, t.revoked_at
		FROM oauth_access_tokens t
		JOIN oauth_clients c ON c.id = t.client_id
		WHERE t.token_hash = $1
	`, tokenHash).Scan(
		&tokenID, &tenantID, &userID, &clientID, &scopesRaw, &expiresAt, &revokedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccessTokenNotFound
		}
		return nil, err
	}
	if revokedAt != nil || now.After(expiresAt) {
		return nil, ErrAccessTokenNotFound
	}
	var scopes []string
	_ = json.Unmarshal(scopesRaw, &scopes)
	return &AccessTokenContext{
		TokenID:   tokenID,
		TenantID:  tenantID,
		UserID:    userID,
		ClientID:  clientID,
		Scopes:    scopes,
		ExpiresAt: expiresAt,
	}, nil
}

// =================== Revocation ===================

// RevokeToken implements RFC 7009. The token type is auto-
// detected: we try the access-token table first, then the
// refresh-token table. Revoking a refresh token cascades to all
// access tokens it ever issued (rotation safety).
//
// Per the RFC, the endpoint MUST return 200 even for unknown
// tokens (to avoid leaking which tokens existed). Callers
// surface that behaviour in the handler.
func (s *Service) RevokeToken(ctx context.Context, client *Client, plaintextToken string) error {
	if client == nil {
		return ErrClientNotFound
	}
	tokenHash := hashToken(plaintextToken)
	now := s.now()
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, client.TenantID); err != nil {
			return err
		}
		var accessClientID string
		err := tx.QueryRow(ctx, `
			SELECT client_id::text FROM oauth_access_tokens WHERE token_hash = $1
		`, tokenHash).Scan(&accessClientID)
		if err == nil {
			if accessClientID != client.ID {
				return &OAuthError{
					Code:        ErrCodeUnauthorizedClient,
					Description: "token was issued to a different client",
					HTTPStatus:  400,
				}
			}
			_, err := tx.Exec(ctx, `
				UPDATE oauth_access_tokens SET revoked_at = $2
				WHERE token_hash = $1 AND revoked_at IS NULL
			`, tokenHash, now)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Try refresh tokens.
		var refreshID, refreshClientID string
		err = tx.QueryRow(ctx, `
			SELECT id::text, client_id::text FROM oauth_refresh_tokens WHERE token_hash = $1
		`, tokenHash).Scan(&refreshID, &refreshClientID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // RFC 7009 — silent success
			}
			return err
		}
		if refreshClientID != client.ID {
			return &OAuthError{
				Code:        ErrCodeUnauthorizedClient,
				Description: "token was issued to a different client",
				HTTPStatus:  400,
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE oauth_refresh_tokens SET revoked_at = $2 WHERE id = $1::uuid AND revoked_at IS NULL
		`, refreshID, now); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			UPDATE oauth_access_tokens SET revoked_at = $2
			WHERE refresh_token_id = $1::uuid AND revoked_at IS NULL
		`, refreshID, now)
		return err
	})
}

// =================== Helpers ===================

// generateOpaqueToken returns a URL-safe base64-encoded random
// token of `byteLen` bytes of entropy. 32 bytes ≈ 43 base64 chars
// and exceeds 256 bits of entropy — well above the RFC 6749
// recommendation.
func generateOpaqueToken(byteLen int) (string, error) {
	buf := make([]byte, byteLen)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashToken returns the lowercase hex SHA-256 digest of `plain`.
// Hex is chosen over base64 so the column comparison can use a
// btree index without case-folding surprises.
func hashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// verifyPKCE reports whether `codeVerifier` resolves to
// `codeChallenge` under `method`. Implements RFC 7636 §4.6 for
// S256 (BASE64URL(SHA256(verifier))) and §4.2 for plain (string
// equality). Uses constant-time comparison.
func verifyPKCE(codeChallenge, codeVerifier, method string) bool {
	switch method {
	case CodeChallengeMethodPlain:
		return subtle.ConstantTimeCompare([]byte(codeChallenge), []byte(codeVerifier)) == 1
	case CodeChallengeMethodS256:
		sum := sha256.Sum256([]byte(codeVerifier))
		want := base64.RawURLEncoding.EncodeToString(sum[:])
		return subtle.ConstantTimeCompare([]byte(codeChallenge), []byte(want)) == 1
	}
	return false
}

func redirectURIInAllowList(allow []string, want string) bool {
	for _, a := range allow {
		if a == want {
			return true
		}
	}
	return false
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func joinScopes(scopes []string) string {
	out := ""
	for i, s := range scopes {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
