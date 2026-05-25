// Package tenant — Stalwart alias sync (HTTP implementation).
//
// Mirror alias CRUD into Stalwart's principal database so inbound
// SMTP for the alias address routes to the user's account. The
// BFF row is authoritative for the admin console; Stalwart sync
// is best-effort and surfaces failures to the audit log.
package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// StalwartShardResolver returns the Stalwart base URL for a tenant.
// `*ShardService` satisfies this interface; tests can substitute a
// simple func resolver without depending on Postgres.
type StalwartShardResolver interface {
	GetTenantShard(ctx context.Context, tenantID string) (string, error)
}

// StalwartAliasHTTPSync talks to Stalwart's management API at
// `PATCH /api/principal/{name}` to add or remove an alias on a
// principal's `emails` field.
//
// Stalwart's principal-update wire format is a JSON array of
// operation objects:
//
//	[{"action":"addItem","field":"emails","value":"alias@example.com"}]
//
// The "emails" field on a principal holds the principal's primary
// address and every alias (Stalwart treats them all as deliverable
// recipients). `addItem` / `removeItem` are the documented atomic
// operations and are idempotent on the server side.
//
// Authentication is HTTP Basic against the Stalwart admin
// credentials configured at deploy time. The recovery admin in
// dev-compose (`admin:kmail-dev`) is fine for local testing;
// production wires a long-lived management account via
// `KMAIL_STALWART_ADMIN_USER` / `KMAIL_STALWART_ADMIN_PASS`.
//
// Fields are unexported so the constructor invariants (non-nil
// resolver, non-empty admin user, fixed-timeout HTTP client) can't
// be violated post-construction by a caller that imports the
// package.
type StalwartAliasHTTPSync struct {
	resolver   StalwartShardResolver
	adminUser  string
	adminPass  string
	httpClient *http.Client
}

// NewStalwartAliasHTTPSync constructs a sync wired to the given
// shard resolver and admin credentials. Returns an error when
// either the resolver or admin user is unset so misconfiguration
// fails at startup rather than at first alias mutation.
func NewStalwartAliasHTTPSync(resolver StalwartShardResolver, adminUser, adminPass string) (*StalwartAliasHTTPSync, error) {
	if resolver == nil {
		return nil, errors.New("stalwart alias sync: shard resolver is required")
	}
	if strings.TrimSpace(adminUser) == "" {
		return nil, errors.New("stalwart alias sync: admin user is required")
	}
	return &StalwartAliasHTTPSync{
		resolver:  resolver,
		adminUser: adminUser,
		adminPass: adminPass,
		// 10s matches the JMAP proxy's default Stalwart timeout
		// for non-streaming admin calls. A Stalwart blip should
		// fail fast and let the operator retry, rather than
		// hanging the create-alias HTTP request.
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// WithHTTPClient returns a copy of the sync wired to a custom
// HTTP client. Used by tests to point the sync at an httptest
// server with a tighter timeout than the production default.
func (s *StalwartAliasHTTPSync) WithHTTPClient(c *http.Client) *StalwartAliasHTTPSync {
	if c == nil {
		return s
	}
	cp := *s
	cp.httpClient = c
	return &cp
}

// AddAlias appends the alias address to the principal's `emails`
// field. Idempotent: Stalwart returns 200 on a duplicate addItem.
func (s *StalwartAliasHTTPSync) AddAlias(ctx context.Context, tenantID, stalwartAccountID, aliasEmail string) error {
	return s.patchPrincipal(ctx, tenantID, stalwartAccountID, "addItem", aliasEmail)
}

// RemoveAlias drops the alias address from the principal's `emails`
// field. Idempotent: Stalwart returns 200 on a missing removeItem.
func (s *StalwartAliasHTTPSync) RemoveAlias(ctx context.Context, tenantID, stalwartAccountID, aliasEmail string) error {
	return s.patchPrincipal(ctx, tenantID, stalwartAccountID, "removeItem", aliasEmail)
}

// stalwartPrincipalOp is one entry in the principal-update array
// (Stalwart 0.16 management API).
type stalwartPrincipalOp struct {
	Action string `json:"action"`
	Field  string `json:"field"`
	Value  string `json:"value"`
}

func (s *StalwartAliasHTTPSync) patchPrincipal(ctx context.Context, tenantID, stalwartAccountID, action, value string) error {
	if strings.TrimSpace(stalwartAccountID) == "" {
		return errors.New("stalwart alias sync: stalwart account id is required")
	}
	if strings.TrimSpace(value) == "" {
		return errors.New("stalwart alias sync: alias email is required")
	}
	shardURL, err := s.resolver.GetTenantShard(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("resolve tenant shard: %w", err)
	}
	body, err := json.Marshal([]stalwartPrincipalOp{{
		Action: action,
		Field:  "emails",
		Value:  value,
	}})
	if err != nil {
		return fmt.Errorf("marshal principal op: %w", err)
	}
	endpoint := strings.TrimRight(shardURL, "/") + "/api/principal/" + url.PathEscape(stalwartAccountID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build principal request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(s.adminUser, s.adminPass)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("call stalwart admin api: %w", err)
	}
	// Drain the body before Close so the underlying HTTP/1.1
	// connection is returned to the keep-alive pool. Without
	// the explicit Copy-then-Close, a non-empty 200 response
	// (Stalwart 0.16 returns a small JSON status envelope on
	// principal updates) leaves bytes unread and forces the
	// transport to close the connection, defeating connection
	// reuse across the worker's batched retries. A bounded
	// LimitReader caps the worst case so an admin endpoint that
	// regresses into returning a multi-MB body cannot pin the
	// goroutine.
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Drain a bounded slice of the error body so the audit log
	// captures the Stalwart error code without spamming on a
	// large HTML 500 page from a misrouted call. The deferred
	// LimitReader above still runs after we read here, so any
	// residual bytes past the 1KB prefix are still discarded
	// and the connection is returned to the pool.
	body, _ = io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("stalwart admin api returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

