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
	"strings"
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
		if err := validateRedirectURI(ru); err != nil {
			return nil, "", err
		}
	}
	if homepageURL != "" {
		if err := validateExternalDisplayURL("homepage_url", homepageURL); err != nil {
			return nil, "", err
		}
	}
	if logoURL != "" {
		if err := validateExternalDisplayURL("logo_url", logoURL); err != nil {
			return nil, "", err
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

	var (
		c           Client
		redirectRaw []byte
		scopesRaw   []byte
	)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var (
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
	// Materialise the JSONB columns into the typed struct from
	// the RETURNING scan directly. Previously this routine did
	// a second SELECT (getClientByPK) after the INSERT to work
	// around the byte-vs-[]string mismatch — wasteful, and the
	// intermediate `json.Unmarshal([]byte("[]"), ...)` was a no-op
	// against a literal rather than the scanned bytes.
	if err := json.Unmarshal(redirectRaw, &c.RedirectURIs); err != nil {
		return nil, "", fmt.Errorf("oauth: decode redirect_uris from INSERT: %w", err)
	}
	if err := json.Unmarshal(scopesRaw, &c.AllowedScopes); err != nil {
		return nil, "", fmt.Errorf("oauth: decode allowed_scopes from INSERT: %w", err)
	}
	return &c, plaintextSecret, nil
}

// GetClient fetches a client by its client_id (the public
// identifier). Tenant-scoped read. Returns ErrClientNotFound for
// both unknown and deactivated rows so callers cannot distinguish
// the two cases (deactivation should look identical to deletion
// to anyone outside the admin surface).
//
// Defence in depth: although SetTenantGUC already restricts the
// row set via Postgres RLS (`migrations/046_oauth_clients.sql`
// `rls_oauth_clients USING (tenant_id = current_setting(
// 'app.tenant_id', true)::uuid)`), the WHERE clause ALSO carries
// an explicit `tenant_id = $1::uuid` predicate. Two reasons:
//
//  1. If a future migration ever drops the RLS policy or toggles
//     `FORCE ROW LEVEL SECURITY` off, the explicit predicate
//     still pins the read to the caller's tenant. RLS is the
//     primary guard; the WHERE clause is the seatbelt.
//  2. The query planner reads the explicit predicate before
//     applying the RLS rewrite, so when `tenant_id` is part of
//     the WHERE the planner can prune partitions / pick the
//     `(tenant_id, client_id)` index directly. The RLS-only
//     formulation works but is slightly more opaque to EXPLAIN.
func (s *Service) GetClient(ctx context.Context, tenantID, clientID string) (*Client, error) {
	if tenantID == "" || clientID == "" {
		return nil, ErrClientNotFound
	}
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
			WHERE tenant_id = $1::uuid AND client_id = $2 AND active = true
		`, tenantID, clientID).Scan(
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
	if err := json.Unmarshal(redirectRaw, &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("oauth: decode redirect_uris: %w", err)
	}
	if err := json.Unmarshal(scopesRaw, &c.AllowedScopes); err != nil {
		return nil, fmt.Errorf("oauth: decode allowed_scopes: %w", err)
	}
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
//
// RLS interaction: this query does NOT call middleware.SetTenantGUC
// before issuing the SELECT, so the row is read across all tenants.
// That works today because `migrations/046_oauth_clients.sql` does
// NOT use FORCE ROW LEVEL SECURITY — the table owner (the role the
// BFF's pgxpool runs as) bypasses the RLS policy by default. This
// matches the existing cross-tenant lookup pattern at
// `internal/tenant/service.go:252` and `internal/scim/service.go:57`.
// If a future migration ever toggles FORCE ROW LEVEL SECURITY on
// `oauth_clients`, this method will start returning zero rows for
// every lookup — the wire protocol does not carry tenant context
// before client resolution, so there is nothing to set the GUC to.
// Don't enable FORCE on this table without first redesigning the
// /oauth/token wire protocol to carry tenant scoping (e.g. a tenant
// prefix on client_id, or a tenant-resolving subdomain).
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
		WHERE client_id = $1 AND active = true
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
	if err := json.Unmarshal(redirectRaw, &c.RedirectURIs); err != nil {
		return nil, fmt.Errorf("oauth: decode redirect_uris: %w", err)
	}
	if err := json.Unmarshal(scopesRaw, &c.AllowedScopes); err != nil {
		return nil, fmt.Errorf("oauth: decode allowed_scopes: %w", err)
	}
	return c, nil
}

// VerifyClientSecret returns nil if `plaintextSecret` matches
// the stored bcrypt hash for the client; otherwise
// ErrClientSecretMismatch. Public clients (no stored hash) MUST
// NOT call this — the /oauth/token endpoint enforces PKCE-only
// auth for public clients instead.
//
// RLS interaction: like LookupClientForExchange and ValidateAccessToken
// above, this query does NOT call middleware.SetTenantGUC before the
// SELECT — VerifyClientSecret is invoked from the /oauth/token wire
// path, where the tenant context is derived FROM the resolved client
// row (which the caller has already obtained via LookupClientForExchange).
// Cross-tenant access works today because `migrations/046_oauth_clients.sql`
// does NOT use FORCE ROW LEVEL SECURITY on `oauth_clients`. If a future
// migration toggles FORCE on, this query (and the two methods named
// above) will silently return zero rows. Don't enable FORCE on
// `oauth_clients` without first redesigning the /oauth/token wire
// protocol to carry tenant scoping before client resolution.
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
			// RFC 7636 §4.3: "If the client does not send the
			// `code_challenge_method` parameter, the server MUST
			// use 'plain'." Persisting 'S256' here would silently
			// break interop with any spec-conformant client that
			// computed challenge == verifier and omitted the
			// method param — their PKCE exchange would fail every
			// time because we'd hash their verifier and compare
			// against their plaintext challenge. The verification
			// path in ExchangeAuthorizationCode mirrors this
			// default for the same reason.
			method = CodeChallengeMethodPlain
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

	// Single-transaction code exchange: claim the code, validate
	// it matches the presenting client / redirect_uri / PKCE
	// verifier, decode its granted scopes, and mint the access +
	// refresh token pair — all under one `pgx.BeginFunc`.
	//
	// Why everything in one tx? On any failure between the
	// UPDATE-consume and the mint INSERTs, the rollback unwinds
	// the consumed_at stamp so the legit client can retry with
	// the same code. The previous shape (consume in tx #1,
	// validate + mint outside in tx #2) had two bad failure
	// modes:
	//
	//   (a) A transient DB / pool failure between the two txs
	//       left the code consumed without ever returning tokens
	//       to the caller. The legit client had to restart the
	//       entire /authorize flow even though no security
	//       violation occurred.
	//   (b) A validation failure (client_id mismatch,
	//       redirect_uri mismatch, PKCE verifier mismatch) burned
	//       the code so a third-party attacker who somehow had
	//       intercepted the redirect could DoS the legit client.
	//       PKCE already protects the exchange (256 bits of
	//       verifier entropy, infeasible to brute-force inside
	//       the 60-second code TTL even at line rate), and the
	//       /oauth/token endpoint is rate-limited, so burning the
	//       code on failed validation adds no security — just a
	//       DoS lever for an attacker who learned the code.
	//
	// Mirrors the pattern in RefreshAccessToken (lines ~617-745)
	// which is structured the same way and explicitly documents:
	// "Mint the new pair INSIDE this tx so a partial failure
	// rolls back both the claim and the new rows — never
	// leaving orphaned tokens whose plaintext was never returned
	// to a caller."
	//
	// The atomic `UPDATE ... WHERE consumed_at IS NULL AND
	// expires_at > $2 RETURNING ...` is still load-bearing: a
	// concurrent /oauth/token call presenting the same plaintext
	// code cannot both pass through the WHERE filter, so only
	// one of them claims and mints. The other gets pgx.ErrNoRows.
	var resp *TokenResponse
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, client.TenantID); err != nil {
			return err
		}

		var (
			codeID, codeUserID, codeRedirect string
			codeChallenge                    *string
			codeChallengeMethod              *string
			scopesRaw                        []byte
			expiresAt                        time.Time
			codeClientID, codeTenantID       string
		)

		// Step 1: atomically consume the code. UPDATE ...
		// RETURNING marks consumed_at AND returns the row's
		// fields in one statement. The WHERE clause filters
		// BOTH `consumed_at IS NULL` AND `expires_at > $2` so
		// expired codes are never "consumed" in the first place
		// (avoids ambiguous "expired vs already consumed" wire
		// envelopes on a forensic-audit retry).
		err := tx.QueryRow(ctx, `
			UPDATE oauth_authorization_codes
			SET consumed_at = $2
			WHERE code_hash = $1
			  AND consumed_at IS NULL
			  AND expires_at > $2
			RETURNING id::text, client_id::text, tenant_id::text, user_id::text,
			          redirect_uri, granted_scopes, code_challenge,
			          code_challenge_method, expires_at
		`, codeHash, now).Scan(
			&codeID, &codeClientID, &codeTenantID, &codeUserID,
			&codeRedirect, &scopesRaw, &codeChallenge, &codeChallengeMethod, &expiresAt,
		)
		if err != nil {
			return err
		}
		_ = codeID

		// Step 2: code-to-client binding. A code issued for
		// client A cannot be exchanged by client B. Returning
		// an OAuthError here rolls back the consume.
		if codeClientID != client.ID {
			return &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "code was issued to a different client",
				HTTPStatus:  400,
			}
		}
		if codeTenantID != client.TenantID {
			// Defence-in-depth — should be impossible given RLS,
			// but belt-and-braces in case the policy is ever
			// loosened.
			return &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "code tenant mismatch",
				HTTPStatus:  400,
			}
		}

		// Step 3: redirect URI must match the one used at
		// /authorize.
		if codeRedirect != redirectURI {
			return &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "redirect_uri does not match the value used at /authorize",
				HTTPStatus:  400,
			}
		}

		// Step 4: PKCE verification.
		//
		// Expiry is no longer checked here because Step 1's
		// WHERE clause already filtered expired codes —
		// reaching this point implies `expires_at > now()` at
		// the moment of consumption. expiresAt is kept on the
		// return path so a caller-side guard (e.g. a future
		// audit hook) can still reason about lifetime if
		// needed.
		_ = expiresAt
		if codeChallenge != nil && *codeChallenge != "" {
			if codeVerifier == "" {
				return &OAuthError{
					Code:        ErrCodeInvalidRequest,
					Description: "code_verifier required (PKCE in use)",
					HTTPStatus:  400,
				}
			}
			// RFC 7636 §4.3: default method is 'plain' when
			// omitted. This mirrors the IssueAuthorizationCode
			// default — if a pre-RFC-7636-compliance code was
			// persisted before this fix, the stored methodPtr
			// will be 'S256' (the old default) and that wire
			// value will still win below; the default only
			// applies to legacy stored rows where the method
			// column is NULL (those don't exist after the
			// IssueAuthorizationCode change, but we keep the
			// defensive default for grandfather safety).
			method := CodeChallengeMethodPlain
			if codeChallengeMethod != nil {
				method = *codeChallengeMethod
			}
			if !verifyPKCE(*codeChallenge, codeVerifier, method) {
				return &OAuthError{
					Code:        ErrCodeInvalidGrant,
					Description: "code_verifier does not match challenge",
					HTTPStatus:  400,
				}
			}
		} else if client.ClientType == ClientTypePublic {
			// Public client without PKCE — should have been
			// rejected at /authorize. Refuse the exchange just in
			// case.
			return &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "public clients must use PKCE",
				HTTPStatus:  400,
			}
		}

		var grantedScopes []string
		// JSONB columns enforce well-formed JSON at the storage
		// layer, so this scopesRaw is structurally guaranteed to
		// decode. We still check the error to match the explicit-
		// check pattern used in RefreshAccessToken — a silent
		// fallthrough to an empty slice would mint a zero-scope
		// access token, which is the worst possible failure mode
		// (call succeeds, caller has no scopes, every downstream
		// resource access returns 403). Better to fail closed
		// and surface the surprise; the rollback also unwinds
		// the consume so a corrupted-JSON row doesn't burn an
		// otherwise-valid code.
		if err := json.Unmarshal(scopesRaw, &grantedScopes); err != nil {
			return fmt.Errorf("oauth: decode granted_scopes from authorization_code: %w", err)
		}

		// Step 5: mint access + refresh tokens INSIDE this tx so
		// a partial failure rolls back the consume too.
		minted, _, mintErr := s.mintTokensTx(ctx, tx, client, codeUserID, grantedScopes, "" /* no parent refresh */, now)
		if mintErr != nil {
			return mintErr
		}
		resp = minted
		return nil
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "authorization code not found, expired, or already consumed",
				HTTPStatus:  400,
			}
		}
		var oerr *OAuthError
		if errors.As(err, &oerr) {
			return nil, oerr
		}
		return nil, err
	}
	return resp, nil
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

	// The entire rotation (claim → validate → mint new pair →
	// revoke old access tokens) runs inside ONE transaction so a
	// concurrent caller racing with the same plaintext refresh
	// token cannot pass the revoked_at check twice. The atomic
	// `UPDATE ... WHERE revoked_at IS NULL RETURNING` mirrors the
	// pattern ExchangeAuthorizationCode uses for one-shot
	// authorization codes (RFC 6749 §4.1.2 / RFC 6819 §5.2.2):
	// the row's revoked_at flips and the row is returned in a
	// single statement, so only one concurrent caller wins the
	// claim. If the tx later fails for any reason (mint error,
	// access-token revoke error), the rollback unrevokes the
	// row — at most one in-flight mint can succeed at a time.
	var (
		resp        *TokenResponse
		replayRowID string
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, client.TenantID); err != nil {
			return err
		}

		var (
			claimedID, claimedUserID, claimedClientID string
			scopesRaw                                 []byte
			expiresAt                                 time.Time
		)
		err := tx.QueryRow(ctx, `
			UPDATE oauth_refresh_tokens
			   SET revoked_at = $2
			 WHERE token_hash = $1
			   AND revoked_at IS NULL
			   AND expires_at  > $2
			RETURNING id::text, client_id::text, user_id::text,
			          scopes, expires_at
		`, refreshHash, now).Scan(
			&claimedID, &claimedClientID, &claimedUserID,
			&scopesRaw, &expiresAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			// The atomic UPDATE didn't match. Three reasons —
			// distinguish them with a single follow-up SELECT
			// so the caller gets a precise wire envelope:
			//
			//   (a) the token row genuinely doesn't exist →
			//       ErrRefreshTokenNotFound (caller learned
			//       the wrong secret or replayed after a hard
			//       revoke).
			//   (b) the row exists but revoked_at IS NOT NULL
			//       (someone else consumed it, OR the chain
			//       was already revoked because of an earlier
			//       replay attempt) → ErrRefreshTokenReplay,
			//       which triggers chain revocation outside
			//       this tx.
			//   (c) the row exists, is not revoked, but is
			//       already past expires_at → typed OAuthError
			//       "refresh token expired". No chain
			//       revocation: an expired-but-unconsumed
			//       token is the legitimate user's own token
			//       that timed out, not a leaked replay.
			//
			// Doing the UPDATE WHERE expires_at > $2 (instead
			// of accepting the row and bouncing it on a
			// post-claim check) means an expired row stays
			// available for the (c) detection path here, and
			// also keeps the auth-code and refresh-token
			// patterns symmetric — they both refuse to consume
			// an expired row atomically.
			var (
				existingID    string
				existingRev   *time.Time
				existingExp   time.Time
			)
			selErr := tx.QueryRow(ctx, `
				SELECT id::text, revoked_at, expires_at
				  FROM oauth_refresh_tokens
				 WHERE token_hash = $1
			`, refreshHash).Scan(&existingID, &existingRev, &existingExp)
			if errors.Is(selErr, pgx.ErrNoRows) {
				return ErrRefreshTokenNotFound
			}
			if selErr != nil {
				return selErr
			}
			if existingRev != nil {
				replayRowID = existingID
				return ErrRefreshTokenReplay
			}
			// (c) — expired but never revoked.
			_ = existingExp
			return &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "refresh token expired",
				HTTPStatus:  400,
			}
		}
		if err != nil {
			return err
		}

		if claimedClientID != client.ID {
			// Forces tx rollback → claim is undone. Return a
			// typed OAuthError so the handler maps it to the
			// right wire envelope.
			return &OAuthError{
				Code:        ErrCodeInvalidGrant,
				Description: "refresh token was issued to a different client",
				HTTPStatus:  400,
			}
		}
		// The UPDATE filter (`AND expires_at > $2`) already refused
		// to consume an expired row, so by this point expiresAt is
		// guaranteed to be in the future. The previous post-claim
		// `now.After(expiresAt)` guard would have been triggered by
		// the rollback path, leaving the revoked_at stamp undone —
		// the new atomic filter avoids the lock-and-rollback churn
		// entirely.
		_ = expiresAt

		var grantedScopes []string
		if err := json.Unmarshal(scopesRaw, &grantedScopes); err != nil {
			return fmt.Errorf("oauth: decode scopes from claimed refresh row: %w", err)
		}

		// Mint the new pair INSIDE this tx so a partial failure
		// rolls back both the claim and the new rows — never
		// leaving orphaned tokens whose plaintext was never
		// returned to a caller.
		minted, _, err := s.mintTokensTx(ctx, tx, client, claimedUserID, grantedScopes, claimedID, now)
		if err != nil {
			return err
		}

		// Revoke every access token issued under the claimed
		// refresh token — those are the ones the caller had
		// alongside the now-rotated refresh token.
		if _, err := tx.Exec(ctx, `
			UPDATE oauth_access_tokens SET revoked_at = $2
			WHERE refresh_token_id = $1::uuid AND revoked_at IS NULL
		`, claimedID, now); err != nil {
			return err
		}

		resp = minted
		return nil
	})

	switch {
	case errors.Is(err, ErrRefreshTokenNotFound):
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "refresh token not found",
			HTTPStatus:  400,
		}
	case errors.Is(err, ErrRefreshTokenReplay):
		// The tx rolled back without revoking anything; now
		// fire the replay-revocation in a fresh tx. The chain
		// revocation is independent of whether we won the
		// rotation race, so doing it after rollback is fine
		// and avoids holding row locks longer than needed.
		if cerr := s.revokeRefreshChain(ctx, client.TenantID, replayRowID); cerr != nil {
			return nil, fmt.Errorf("oauth: revoke chain on replay: %w", cerr)
		}
		return nil, &OAuthError{
			Code:        ErrCodeInvalidGrant,
			Description: "refresh token replay detected — token family revoked",
			HTTPStatus:  400,
		}
	case err != nil:
		var oerr *OAuthError
		if errors.As(err, &oerr) {
			return nil, oerr
		}
		return nil, err
	}
	return resp, nil
}

// mintTokensTx generates a fresh access + refresh token pair tied
// to the given client/user/scopes and persists them inside the
// caller's transaction. parentRefreshID is the previous refresh
// token's row id (empty string for a brand-new chain from
// /authorize; non-empty for /refresh).
//
// The caller MUST have already opened a transaction AND called
// middleware.SetTenantGUC on it before invoking this helper — that
// invariant is what makes the mint atomic with the surrounding
// claim (auth-code consume or refresh-token rotate). The extra
// returned string is the new refresh token's row id, useful for
// callers that want to chain further work (e.g. update related rows
// in the same transaction).
//
// There used to be a standalone `mintTokens(ctx, ...)` wrapper here
// that opened its own transaction and called this helper, used
// during the two-tx era when /authorize and /refresh minted outside
// their claim transactions. After both call sites were collapsed
// into single-transaction flows (so a mint failure rolls back the
// claim too, preventing orphaned tokens whose plaintext was never
// returned to a caller), the wrapper became dead code and was
// removed — there is no remaining callsite that wants to mint in
// its own short-lived transaction.
func (s *Service) mintTokensTx(
	ctx context.Context,
	tx pgx.Tx,
	client *Client,
	userID string,
	scopes []string,
	parentRefreshID string,
	now time.Time,
) (*TokenResponse, string, error) {
	plaintextAccess, err := generateOpaqueToken(32)
	if err != nil {
		return nil, "", fmt.Errorf("oauth: generate access token: %w", err)
	}
	plaintextRefresh, err := generateOpaqueToken(32)
	if err != nil {
		return nil, "", fmt.Errorf("oauth: generate refresh token: %w", err)
	}
	accessHash := hashToken(plaintextAccess)
	refreshHash := hashToken(plaintextRefresh)
	scopesJSON, _ := json.Marshal(scopes)

	var parentPtr *string
	if parentRefreshID != "" {
		parentPtr = &parentRefreshID
	}
	var newRefreshID string
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
		return nil, "", err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO oauth_access_tokens (
			tenant_id, client_id, user_id, token_hash, scopes,
			expires_at, refresh_token_id
		)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5::jsonb, $6, $7::uuid)
	`,
		client.TenantID, client.ID, userID, accessHash, string(scopesJSON),
		now.Add(s.accessTokenTTL), newRefreshID,
	); err != nil {
		return nil, "", err
	}

	return &TokenResponse{
		AccessToken:  plaintextAccess,
		TokenType:    "Bearer",
		ExpiresIn:    int(s.accessTokenTTL.Seconds()),
		RefreshToken: plaintextRefresh,
		Scope:        joinScopes(scopes),
	}, newRefreshID, nil
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
//
// RLS interaction: like LookupClientForExchange above, this query
// does NOT call middleware.SetTenantGUC before the SELECT — the
// access token IS the tenant resolution mechanism (the row's
// tenant_id is what populates the returned context's TenantID,
// which is then used by downstream handlers to set the per-request
// GUC for scoped queries). Cross-tenant SELECT works today because
// `migrations/046_oauth_clients.sql` does NOT use FORCE ROW LEVEL
// SECURITY on `oauth_access_tokens`. If a future migration toggles
// FORCE on, every bearer-token validation will return zero rows.
// Don't enable FORCE without redesigning the bearer wire protocol
// (e.g. tenant-scoped JWT with claims that drive a pre-validation
// SetTenantGUC).
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
	)
	// No tenant scoping on the lookup — we don't know the tenant
	// yet. The token_hash is globally unique (UNIQUE constraint
	// on `oauth_access_tokens.token_hash`) so this is safe. We
	// then apply the tenant GUC in the downstream handler chain.
	//
	// The JOIN also requires the issuing client to still be
	// `active = true` — otherwise a deactivated app's existing
	// tokens would keep working until natural expiry, which
	// defeats the operator action of deactivating it. An
	// already-issued bearer token presented after deactivation
	// looks identical to a revoked token from the caller's
	// perspective.
	//
	// Revocation and expiry are filtered in the WHERE clause
	// rather than re-checked in application code after the SELECT:
	// (a) avoids transferring rows the application would just
	// discard (one round trip's worth of bytes on every
	// authenticated request — this path is hot); (b) makes the
	// SQL the single source of truth on which rows are "valid",
	// so a future refactor that drops a post-fetch check cannot
	// silently let a revoked/expired token slip through. Both
	// failure modes (revoked, expired) collapse to ErrAccessToken-
	// NotFound — RFC 6750 §3.1 requires `invalid_token` for both,
	// and the caller cannot distinguish them anyway.
	// Project `t.client_id::text` (the UUID FK on
	// oauth_access_tokens that references oauth_clients.id), NOT
	// `c.client_id` (the TEXT public identifier from
	// oauth_clients). AccessTokenContext.ClientID is consumed by
	// downstream handlers (e.g. internal/integrations) that pass
	// it to SQL queries which cast the value `::uuid` to scope
	// webhook_endpoints / oauth_access_tokens rows by their
	// owning client. The TEXT public identifier (`dBjftJeZ4CVP...`)
	// is NOT a valid UUID, so every such cast would fail at
	// runtime — the wrong projection would 500 every integration
	// webhook operation. RevokeToken below uses the same UUID
	// projection from `oauth_access_tokens.client_id::text`
	// (lines ~1054 / 1076) for the same reason: the bearer-token
	// → client identity link is the UUID, not the text label.
	err := s.pool.QueryRow(ctx, `
		SELECT t.id::text, t.tenant_id::text, t.user_id::text, t.client_id::text,
		       t.scopes, t.expires_at
		FROM oauth_access_tokens t
		JOIN oauth_clients c ON c.id = t.client_id
		WHERE t.token_hash = $1
		  AND c.active = true
		  AND t.revoked_at IS NULL
		  AND t.expires_at > $2
	`, tokenHash, now).Scan(
		&tokenID, &tenantID, &userID, &clientID, &scopesRaw, &expiresAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAccessTokenNotFound
		}
		return nil, err
	}
	var scopes []string
	// Match the explicit-check pattern in RefreshAccessToken /
	// ExchangeAuthorizationCode rather than silently swallowing
	// a decode error. A token row whose `scopes` JSONB is
	// somehow malformed (impossible given column-level JSONB
	// validation, but defensive against e.g. a manual SQL edit
	// in an incident shell) MUST fail closed so the caller gets
	// 401 / ErrAccessTokenNotFound rather than a zero-scope
	// access pass.
	if err := json.Unmarshal(scopesRaw, &scopes); err != nil {
		return nil, fmt.Errorf("oauth: decode access_token scopes: %w", err)
	}
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

// validateRedirectURI mirrors what RFC 6749 §3.1.2 requires of a
// redirect_uri: an absolute URI, non-empty, with a scheme. The
// scheme MUST be one of http/https for browser-facing flows; we
// also permit a native app's custom scheme (e.g. com.example.app://
// per RFC 8252 §7.1) so installed apps can complete the
// authorization_code grant.
//
// Disallowing javascript:, data:, file:, and similar "active" URI
// schemes here is defence-in-depth: html/template's URL filter at
// render time already blocks javascript: in href/src contexts, but
// catching it at registration means a confused-deputy attack on the
// consent screen (admin pastes a malicious URL, html/template
// silently filters it to "#ZgotmplZ", consent screen has a broken
// link) never reaches the rendering layer in the first place.
func validateRedirectURI(raw string) error {
	if raw == "" {
		return errors.New("oauth: redirect_uri must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("oauth: invalid redirect_uri %q: %w", raw, err)
	}
	if u.Scheme == "" {
		return fmt.Errorf("oauth: redirect_uri %q must include a scheme", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "https", "http":
		// http is permitted to support localhost development per
		// RFC 8252 §8.3; production deployments should restrict
		// via the chart's allowed_scopes / network policy. The
		// scheme by itself is not where we enforce TLS.
		return nil
	case "javascript", "data", "vbscript", "file":
		return fmt.Errorf("oauth: redirect_uri %q uses a dangerous scheme", raw)
	default:
		// RFC 8252 §7.1 custom URI scheme for native apps
		// (e.g. com.example.app://callback). Permit but require
		// a reverse-DNS-style scheme so adversaries can't
		// register "*://anything" wildcards.
		if !strings.Contains(scheme, ".") {
			return fmt.Errorf("oauth: redirect_uri %q uses a non-reverse-DNS custom scheme", raw)
		}
		return nil
	}
}

// validateExternalDisplayURL checks a URL that will be rendered
// (NOT redirected to) on the consent screen — homepage_url and
// logo_url. These appear inside <a href=...> and <img src=...>
// contexts. html/template's auto-escaping URL filter would
// neutralise a javascript: URL at render time, but we still want
// to refuse it at registration so the consent screen never has a
// "broken link to #ZgotmplZ" UX. https-only here because we don't
// want a mixed-content warning on the consent screen, and a
// homepage_url ought to be securable.
func validateExternalDisplayURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("oauth: invalid %s %q: %w", field, raw, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return fmt.Errorf("oauth: %s %q must use http or https (got %q)", field, raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("oauth: %s %q must include a host", field, raw)
	}
	return nil
}

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
