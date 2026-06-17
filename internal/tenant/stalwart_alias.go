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
	"strconv"
	"strings"
	"time"
)

// StalwartShardResolver returns the Stalwart base URL for a tenant.
// `*ShardService` satisfies this interface; tests can substitute a
// simple func resolver without depending on Postgres.
type StalwartShardResolver interface {
	GetTenantShard(ctx context.Context, tenantID string) (string, error)
}

// StalwartAliasHTTPSync mirrors alias CRUD into Stalwart over the
// custom `x:`-namespaced JMAP management methods served at
// `POST {shard}/jmap`. Stalwart 0.16 dropped the REST
// `/api/principal/{name}` surface this used to call; aliases now
// live in a structured `aliases` list on the principal and are
// edited with the JMAP `x:Account/set` patch grammar:
//
//	x:Account/set {"update": {"<accountId>": {"aliases/<n>": {...}}}}
//
// where each list entry is an EmailAlias object
// `{enabled, name (local-part), domainId, description}` and the
// `aliases/<n>` patch path either sets (object value) or removes
// (null value) the entry at integer index `n`. Because the patch
// is index-addressed rather than value-addressed, every mutation
// is a read-modify-write: the principal is fetched, the existing
// aliases are inspected for idempotency, and a free index is
// chosen (add) or the matching index located (remove).
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

// AddAlias adds an EmailAlias entry for aliasEmail to the
// principal. Idempotent: a no-op when the principal already has a
// matching (local-part + domain) alias.
func (s *StalwartAliasHTTPSync) AddAlias(ctx context.Context, tenantID, stalwartAccountID, aliasEmail string) error {
	return s.syncAlias(ctx, tenantID, stalwartAccountID, aliasEmail, true)
}

// RemoveAlias removes the EmailAlias entry for aliasEmail from the
// principal. Idempotent: a no-op when no matching alias exists.
func (s *StalwartAliasHTTPSync) RemoveAlias(ctx context.Context, tenantID, stalwartAccountID, aliasEmail string) error {
	return s.syncAlias(ctx, tenantID, stalwartAccountID, aliasEmail, false)
}

// stalwartEmailAlias mirrors Stalwart's EmailAlias struct. `name`
// is the address local-part; `domainId` references a Domain
// principal. `enabled` defaults to true and `description` is
// optional, so both are omitted from add patches when zero.
type stalwartEmailAlias struct {
	Enabled     bool    `json:"enabled"`
	Name        string  `json:"name"`
	DomainID    string  `json:"domainId"`
	Description *string `json:"description,omitempty"`
}

// stalwartAccount is the slice of a Stalwart principal the alias
// sync reads back. `aliases` is a sparse map keyed by the integer
// list index (as a string), matching Stalwart's wire encoding of
// its internal `VecMap<u32, EmailAlias>`.
type stalwartAccount struct {
	ID           string                        `json:"id"`
	Name         string                        `json:"name"`
	EmailAddress string                        `json:"emailAddress"`
	Aliases      map[string]stalwartEmailAlias `json:"aliases"`
}

func (s *StalwartAliasHTTPSync) syncAlias(ctx context.Context, tenantID, stalwartAccountID, aliasEmail string, add bool) error {
	if strings.TrimSpace(stalwartAccountID) == "" {
		return errors.New("stalwart alias sync: stalwart account id is required")
	}
	localPart, domain, err := splitAliasEmail(aliasEmail)
	if err != nil {
		return err
	}
	shardURL, err := s.resolver.GetTenantShard(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("resolve tenant shard: %w", err)
	}
	account, err := s.resolvePrincipal(ctx, shardURL, stalwartAccountID)
	if err != nil {
		return err
	}
	domainID, err := s.resolveDomainID(ctx, shardURL, domain)
	if err != nil {
		return err
	}
	// Locate an existing alias matching local-part + domain so add
	// and remove are both idempotent. Local-parts are matched
	// case-insensitively to avoid creating case-variant duplicates.
	existingIdx, found := "", false
	for idx, a := range account.Aliases {
		if strings.EqualFold(a.Name, localPart) && a.DomainID == domainID {
			existingIdx, found = idx, true
			break
		}
	}
	if add {
		if found {
			return nil
		}
		patch := map[string]any{
			"aliases/" + nextAliasIndex(account.Aliases): stalwartEmailAlias{
				Enabled:  true,
				Name:     localPart,
				DomainID: domainID,
			},
		}
		return s.setAccount(ctx, shardURL, account.ID, patch)
	}
	if !found {
		return nil
	}
	return s.setAccount(ctx, shardURL, account.ID, map[string]any{
		"aliases/" + existingIdx: nil,
	})
}

// resolvePrincipal maps the stored stalwart_account_id to a live
// Stalwart principal. The stored value is not uniform across
// deployments — dev/CI seeds a JMAP id, while prod's signup wiring
// stores the user's email as a placeholder — so three strategies
// are tried in order of precision:
//
//  1. Treat it as a JMAP id directly. x:Account/get returns the
//     principal for a real id and an empty list (not an error) for
//     a non-id string, so this is a clean probe.
//  2. Resolve by principal name (the login local-part Stalwart
//     stores as `name`) via x:Account/query filter:{name}.
//  3. Resolve by email address. Stalwart's account filter rejects
//     an `email` key (unsupportedFilter) but its `text` filter
//     matches the full address; we text-search then confirm with an
//     exact, case-insensitive emailAddress match so a substring
//     collision can never bind us to the wrong principal.
func (s *StalwartAliasHTTPSync) resolvePrincipal(ctx context.Context, shardURL, identifier string) (stalwartAccount, error) {
	byID, err := s.getAccounts(ctx, shardURL, []string{identifier})
	if err != nil {
		return stalwartAccount{}, err
	}
	for _, a := range byID {
		if a.ID == identifier {
			return a, nil
		}
	}

	ids, err := s.queryAccountIDs(ctx, shardURL, map[string]any{"name": identifier})
	if err != nil {
		return stalwartAccount{}, err
	}
	if len(ids) > 0 {
		byName, err := s.getAccounts(ctx, shardURL, ids[:1])
		if err != nil {
			return stalwartAccount{}, err
		}
		for _, a := range byName {
			if a.ID == ids[0] {
				return a, nil
			}
		}
	}

	if strings.Contains(identifier, "@") {
		ids, err := s.queryAccountIDs(ctx, shardURL, map[string]any{"text": identifier})
		if err != nil {
			return stalwartAccount{}, err
		}
		if len(ids) > 0 {
			byEmail, err := s.getAccounts(ctx, shardURL, ids)
			if err != nil {
				return stalwartAccount{}, err
			}
			for _, a := range byEmail {
				if strings.EqualFold(a.EmailAddress, identifier) {
					return a, nil
				}
			}
		}
	}

	return stalwartAccount{}, fmt.Errorf("stalwart alias sync: principal %q not found", identifier)
}

// resolveDomainID maps a domain name to its Stalwart Domain
// principal id, which EmailAlias.domainId must reference.
func (s *StalwartAliasHTTPSync) resolveDomainID(ctx context.Context, shardURL, domain string) (string, error) {
	raw, err := s.jmapCall(ctx, shardURL, "x:Domain/query", map[string]any{
		"filter": map[string]any{"name": domain},
	})
	if err != nil {
		return "", err
	}
	var res struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("decode x:Domain/query: %w", err)
	}
	if len(res.IDs) == 0 {
		return "", fmt.Errorf("stalwart alias sync: domain %q not found", domain)
	}
	return res.IDs[0], nil
}

// getAccounts fetches principals by id. Unknown ids are silently
// omitted from `list` by Stalwart rather than erroring.
func (s *StalwartAliasHTTPSync) getAccounts(ctx context.Context, shardURL string, ids []string) ([]stalwartAccount, error) {
	raw, err := s.jmapCall(ctx, shardURL, "x:Account/get", map[string]any{"ids": ids})
	if err != nil {
		return nil, err
	}
	var res struct {
		List []stalwartAccount `json:"list"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode x:Account/get: %w", err)
	}
	return res.List, nil
}

func (s *StalwartAliasHTTPSync) queryAccountIDs(ctx context.Context, shardURL string, filter map[string]any) ([]string, error) {
	raw, err := s.jmapCall(ctx, shardURL, "x:Account/query", map[string]any{"filter": filter})
	if err != nil {
		return nil, err
	}
	var res struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode x:Account/query: %w", err)
	}
	return res.IDs, nil
}

// setAccount applies an x:Account/set update patch to one
// principal and reports the per-object result. The envelope
// `accountId` is intentionally omitted: Stalwart ignores it for
// this management method, and omitting it keeps the call
// independent of whichever account the admin principal maps to
// (which differs between deployments).
func (s *StalwartAliasHTTPSync) setAccount(ctx context.Context, shardURL, jmapID string, patch map[string]any) error {
	raw, err := s.jmapCall(ctx, shardURL, "x:Account/set", map[string]any{
		"update": map[string]any{jmapID: patch},
	})
	if err != nil {
		return err
	}
	var res struct {
		Updated    map[string]json.RawMessage `json:"updated"`
		NotUpdated map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"notUpdated"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("decode x:Account/set: %w", err)
	}
	if _, ok := res.Updated[jmapID]; ok {
		return nil
	}
	if e, ok := res.NotUpdated[jmapID]; ok {
		return fmt.Errorf("stalwart alias sync: account %s not updated: %s: %s", jmapID, e.Type, strings.TrimSpace(e.Description))
	}
	return fmt.Errorf("stalwart alias sync: account %s missing from x:Account/set response", jmapID)
}

// jmapCall issues a single-method JMAP request against the shard's
// `/jmap` endpoint with admin Basic auth and returns the raw
// arguments object of the (sole) method response. A method-level
// JMAP error (response name "error") and a non-2xx HTTP status are
// both surfaced as Go errors.
func (s *StalwartAliasHTTPSync) jmapCall(ctx context.Context, shardURL, method string, args any) (json.RawMessage, error) {
	reqBody, err := json.Marshal(map[string]any{
		"using":       []string{"urn:ietf:params:jmap:core"},
		"methodCalls": []any{[]any{method, args, "0"}},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal jmap request: %w", err)
	}
	endpoint := strings.TrimRight(shardURL, "/") + "/jmap"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build jmap request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(s.adminUser, s.adminPass)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call stalwart jmap: %w", err)
	}
	// Bound the body read, then drain any residual bytes before
	// Close so the keep-alive connection is returned to the pool
	// across the worker's batched retries.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stalwart jmap returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if readErr != nil {
		return nil, fmt.Errorf("read jmap response: %w", readErr)
	}
	var envelope struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode jmap response: %w", err)
	}
	if len(envelope.MethodResponses) == 0 || len(envelope.MethodResponses[0]) < 2 {
		return nil, errors.New("stalwart jmap: empty method response")
	}
	var name string
	if err := json.Unmarshal(envelope.MethodResponses[0][0], &name); err != nil {
		return nil, fmt.Errorf("decode jmap method name: %w", err)
	}
	respArgs := envelope.MethodResponses[0][1]
	if name == "error" {
		var je struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(respArgs, &je)
		return nil, fmt.Errorf("stalwart jmap method error: %s: %s", je.Type, strings.TrimSpace(je.Description))
	}
	return respArgs, nil
}

// splitAliasEmail splits an address into local-part and domain,
// rejecting empty input and malformed addresses (no `@`, empty
// local-part, or empty domain).
func splitAliasEmail(aliasEmail string) (localPart, domain string, err error) {
	aliasEmail = strings.TrimSpace(aliasEmail)
	if aliasEmail == "" {
		return "", "", errors.New("stalwart alias sync: alias email is required")
	}
	at := strings.LastIndex(aliasEmail, "@")
	if at <= 0 || at == len(aliasEmail)-1 {
		return "", "", fmt.Errorf("stalwart alias sync: invalid alias email %q", aliasEmail)
	}
	return aliasEmail[:at], aliasEmail[at+1:], nil
}

// nextAliasIndex returns the smallest non-negative integer index
// not already used as an aliases-map key, so adds reuse gaps left
// by prior removes rather than growing the index space unbounded.
func nextAliasIndex(aliases map[string]stalwartEmailAlias) string {
	used := make(map[uint64]bool, len(aliases))
	for k := range aliases {
		if n, err := strconv.ParseUint(k, 10, 32); err == nil {
			used[n] = true
		}
	}
	var n uint64
	for used[n] {
		n++
	}
	return strconv.FormatUint(n, 10)
}
