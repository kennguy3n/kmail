// Package iamcore is KMail's client for the iam-core
// (uneycom/iam-core) Identity and Access Management platform.
//
// When iam-core is configured as KMail's OIDC identity provider
// (see docs/IAM_CORE_INTEGRATION.md), the BFF no longer mints its
// own OAuth2 tokens. This package provides the two halves of the
// remaining control-plane coupling:
//
//   - Client: a machine-to-machine (M2M) client that obtains
//     Client Credentials tokens from iam-core's `/oauth2/token`
//     endpoint and calls the iam-core Management API
//     (`/api/v1/management/...`) to read users and tenants. Used by
//     the lazy-provisioning middleware and operational tooling that
//     needs to reconcile KMail's control plane against iam-core.
//   - WebhookReceiver (webhooks.go): an HTTP handler that ingests
//     iam-core lifecycle events (tenant/user create/update/delete)
//     and drives the matching KMail tenant.Service provisioning.
//
// iam-core scopes M2M clients per tenant. KMail registers its M2M
// application in a single management tenant; the token request
// carries that tenant as the `X-Tenant-ID` header (derived from the
// audience host) and the management calls target an arbitrary
// tenant via their own `X-Tenant-ID` header. See token.sh /
// users/_common.sh in the iam-core repo for the canonical request
// shapes this client mirrors.
package iamcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// tokenRefreshSkew is how far ahead of the actual expiry the client
// proactively refreshes the cached M2M token. A non-trivial skew
// avoids handing out a token that expires mid-flight on a slow
// management call.
const tokenRefreshSkew = 30 * time.Second

// defaultHTTPTimeout bounds every iam-core request. The Management
// API is a control-plane dependency on the request hot path (lazy
// provisioning), so a hung iam-core must not wedge a KMail request
// indefinitely.
const defaultHTTPTimeout = 10 * time.Second

// Config wires a Client. ClientID/ClientSecret/Audience are the
// Client Credentials parameters iam-core issued for KMail's M2M
// application; MgmtURL is the iam-core base URL.
type Config struct {
	// MgmtURL is the iam-core base URL. The token endpoint is
	// `<MgmtURL>/oauth2/token`; management resources are under
	// `<MgmtURL>/api/v1/management/...`. Required.
	MgmtURL string

	// ClientID / ClientSecret are the Client Credentials grant
	// parameters. Required.
	ClientID     string
	ClientSecret string

	// Audience is the token `audience`. iam-core encodes the M2M
	// client's home tenant in the audience host (e.g.
	// `https://<mgmt-tenant>/api/v1/management/`); MgmtTenantID is
	// derived from it when not set explicitly. Required.
	Audience string

	// MgmtTenantID is the `X-Tenant-ID` header sent on the token
	// request — the tenant KMail's M2M client is registered in.
	// When empty it is derived from the Audience host. Management
	// calls override this header with their own target tenant.
	MgmtTenantID string

	// HTTPClient is optional; a 10s-timeout client is used when nil.
	HTTPClient *http.Client

	// Logger is optional; log.Default() is used when nil.
	Logger *log.Logger
}

// Client is a cached M2M client for the iam-core Management API.
// It is safe for concurrent use: the token cache is guarded by a
// mutex so a single in-flight refresh is shared across callers.
type Client struct {
	mgmtURL      string
	clientID     string
	clientSecret string
	audience     string
	mgmtTenantID string
	httpClient   *http.Client
	logger       *log.Logger

	mu          sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

// User mirrors the iam-core Management API user representation
// (`GET /api/v1/management/users/{user_id}`). Field tags match
// iam-core's `json_name` annotations in
// api/management/v1/management.proto.
type User struct {
	UserID        string `json:"user_id"`
	TenantID      string `json:"tenant_id"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	GivenName     string `json:"given_name"`
	FamilyName    string `json:"family_name"`
	Picture       string `json:"picture"`
	Blocked       bool   `json:"blocked"`
	LoginCount    int    `json:"login_count"`
	LastLoginAt   string `json:"last_login_at"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

// ListUsersResponse mirrors `GET /api/v1/management/users`.
type ListUsersResponse struct {
	Users   []User `json:"users"`
	Total   int    `json:"total"`
	Page    int    `json:"page"`
	PerPage int    `json:"per_page"`
}

// Tenant mirrors `GET /api/v1/management/tenants/{tenant_id}`.
// Only the fields KMail consumes are modelled; iam-core may return
// more (config, branding, discovery URLs) which json.Unmarshal
// discards.
type Tenant struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Domain     string `json:"domain"`
	TenantType string `json:"tenant_type"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// ErrNotFound is returned when iam-core responds 404 for a user or
// tenant lookup.
var ErrNotFound = errors.New("iamcore: not found")

// New constructs a Client from cfg. It validates the required
// fields and derives MgmtTenantID from the audience host when the
// caller did not set it explicitly.
func New(cfg Config) (*Client, error) {
	mgmtURL := strings.TrimRight(strings.TrimSpace(cfg.MgmtURL), "/")
	if mgmtURL == "" {
		return nil, errors.New("iamcore: MgmtURL is required")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("iamcore: ClientID and ClientSecret are required")
	}
	if cfg.Audience == "" {
		return nil, errors.New("iamcore: Audience is required")
	}
	mgmtTenant := cfg.MgmtTenantID
	if mgmtTenant == "" {
		mgmtTenant = tenantFromAudience(cfg.Audience)
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Client{
		mgmtURL:      mgmtURL,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		audience:     cfg.Audience,
		mgmtTenantID: mgmtTenant,
		httpClient:   httpClient,
		logger:       logger,
	}, nil
}

// tenantFromAudience extracts the tenant identifier iam-core
// encodes in the audience host. `https://acme/api/v1/management/`
// yields `acme`. Returns "" when the audience is not a parseable
// URL with a host (in which case the token request omits the
// X-Tenant-ID header and relies on iam-core's default resolution).
func tenantFromAudience(aud string) string {
	u, err := url.Parse(strings.TrimSpace(aud))
	if err != nil {
		return ""
	}
	return u.Host
}

// tokenResponse is the subset of the RFC 6749 token response KMail
// consumes from `/oauth2/token`.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// token returns a valid M2M access token, refreshing the cache when
// it is empty or within tokenRefreshSkew of expiry. The refresh is
// performed under the mutex so concurrent callers share one token
// request rather than stampeding the iam-core token endpoint.
func (c *Client) token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedToken != "" && time.Now().Before(c.tokenExpiry.Add(-tokenRefreshSkew)) {
		return c.cachedToken, nil
	}
	tok, expiry, err := c.requestToken(ctx)
	if err != nil {
		return "", err
	}
	c.cachedToken = tok
	c.tokenExpiry = expiry
	return tok, nil
}

// requestToken performs the Client Credentials grant against
// `<MgmtURL>/oauth2/token`. The body is form-encoded and the M2M
// client's home tenant is sent as `X-Tenant-ID` (per iam-core's
// per-tenant client scoping).
func (c *Client) requestToken(ctx context.Context) (string, time.Time, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("audience", c.audience)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.mgmtURL+"/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("iamcore: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.mgmtTenantID != "" {
		req.Header.Set("X-Tenant-ID", c.mgmtTenantID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("iamcore: token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("iamcore: token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("iamcore: decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, errors.New("iamcore: token response missing access_token")
	}
	// Default to a conservative 5-minute lifetime when iam-core
	// omits expires_in so we still refresh rather than caching a
	// token forever.
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return tr.AccessToken, time.Now().Add(ttl), nil
}

// GetUser fetches a single user from the iam-core Management API.
// tenantID is sent as the `X-Tenant-ID` header (the target tenant);
// userID is the iam-core user id (the `sub` claim of a user token).
func (c *Client) GetUser(ctx context.Context, tenantID, userID string) (*User, error) {
	if tenantID == "" || userID == "" {
		return nil, errors.New("iamcore: GetUser requires tenantID and userID")
	}
	var u User
	if err := c.doManagementGET(ctx, tenantID, "/api/v1/management/users/"+url.PathEscape(userID), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// ListUsers fetches all users for a tenant. iam-core paginates;
// callers that need every page can drive the Page field, but the
// first page is sufficient for the reconciliation paths KMail uses
// today.
func (c *Client) ListUsers(ctx context.Context, tenantID string) (*ListUsersResponse, error) {
	if tenantID == "" {
		return nil, errors.New("iamcore: ListUsers requires tenantID")
	}
	var out ListUsersResponse
	if err := c.doManagementGET(ctx, tenantID, "/api/v1/management/users", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTenant fetches tenant metadata from the iam-core Management
// API. tenantID is both the path parameter and the `X-Tenant-ID`
// header.
func (c *Client) GetTenant(ctx context.Context, tenantID string) (*Tenant, error) {
	if tenantID == "" {
		return nil, errors.New("iamcore: GetTenant requires tenantID")
	}
	var t Tenant
	if err := c.doManagementGET(ctx, tenantID, "/api/v1/management/tenants/"+url.PathEscape(tenantID), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// doManagementGET issues an authenticated GET against the
// management API, sets the target-tenant header, and decodes the
// JSON body into out. A 404 maps to ErrNotFound; any other non-2xx
// is surfaced with the response body for diagnosis.
func (c *Client) doManagementGET(ctx context.Context, targetTenantID, path string, out any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.mgmtURL+path, nil)
	if err != nil {
		return fmt.Errorf("iamcore: build request %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Tenant-ID", targetTenantID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("iamcore: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("iamcore: GET %s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("iamcore: decode %s response: %w", path, err)
	}
	return nil
}
