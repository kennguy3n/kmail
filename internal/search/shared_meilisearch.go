// Package search — shared Meilisearch backend.
//
// The "shared" model collapses N tenants on the same Stalwart
// shard into ONE Meilisearch index (`kmail_shared_<shard>`).
// Tenant isolation is enforced at query time via the index's
// `filterableAttributes: ["tenant_id"]` setting plus an explicit
// `filter` clause on every search and delete-by-query call.
//
// This is the production default for new tenants (migration 050).
// The per-tenant `MeilisearchBackend` stays around for legacy
// tenants and is still used as a migration source by the cutover
// worker.
package search

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sharedMeiliDoc is the document shape sent to Meilisearch in
// the shared-index model. It embeds the public `Message` shape
// and adds a composite `DocID` that scopes the Meilisearch
// primary key by tenant.
//
// This is load-bearing: Stalwart-issued `message_id`s are
// per-ACCOUNT, not globally unique across tenants on the same
// shard. Without the composite key, two tenants on the same
// shard with the same `message_id` would collide in the shared
// index and the second write would silently overwrite the first.
// `SharedOpenSearchBackend` already uses the same
// `sharedDocID(tenant, message)` shape via the OpenSearch `_id`
// header; this type is the Meilisearch equivalent so both shared
// backends preserve tenant isolation under the same invariant.
type sharedMeiliDoc struct {
	Message
	// DocID is the Meilisearch document primary key. Set by
	// the backend before every write. Format:
	// `<tenant_id>:<message_id>`.
	DocID string `json:"doc_id"`
}

// SharedMeilisearchBackend implements SearchBackend against a
// shared Meilisearch index per Stalwart shard. Every document
// carries a `tenant_id` field; reads and deletes filter on that
// field to keep tenants isolated.
type SharedMeilisearchBackend struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client

	// Shards resolves the Stalwart shard a tenant is assigned
	// to. The backend caches the result per-tenant via the
	// shard service's own LRU; we re-resolve on every call here
	// rather than caching twice.
	Shards ShardResolver

	// settingsApplied tracks which shared indexes have already
	// had `filterableAttributes` and `searchableAttributes`
	// pushed. Meilisearch settings calls are slow (they enqueue
	// a re-indexing task), so we do this exactly once per
	// process per index. The map is small (~one entry per
	// Stalwart shard) and read-mostly so a single Mutex is
	// fine.
	settingsMu      sync.Mutex
	settingsApplied map[string]bool
}

// NewSharedMeilisearchBackend builds a SharedMeilisearchBackend
// against the given Meilisearch instance. The Shards resolver is
// required — without it the backend cannot derive an index name.
func NewSharedMeilisearchBackend(baseURL, apiKey string, shards ShardResolver) (*SharedMeilisearchBackend, error) {
	if baseURL == "" {
		return nil, errors.New("search.NewSharedMeilisearchBackend: baseURL is required")
	}
	if shards == nil {
		return nil, errors.New("search.NewSharedMeilisearchBackend: shards resolver is required")
	}
	return &SharedMeilisearchBackend{
		BaseURL:         baseURL,
		APIKey:          apiKey,
		HTTPClient:      &http.Client{Timeout: 10 * time.Second},
		Shards:          shards,
		settingsApplied: map[string]bool{},
	}, nil
}

// Name returns "shared_meilisearch".
func (m *SharedMeilisearchBackend) Name() string { return BackendSharedMeilisearch }

// IndexMessage upserts one document. The shared-index settings
// (filterable + searchable attributes) are pushed lazily on the
// first call for a given shard so a Meilisearch reset doesn't
// leave the BFF stuck with a misconfigured index.
func (m *SharedMeilisearchBackend) IndexMessage(ctx context.Context, msg Message) error {
	index, err := m.indexFor(ctx, msg.TenantID)
	if err != nil {
		return err
	}
	if err := m.ensureSettings(ctx, index); err != nil {
		return fmt.Errorf("ensure settings: %w", err)
	}
	doc := sharedMeiliDoc{
		Message: msg,
		DocID:   sharedDocID(msg.TenantID, msg.MessageID),
	}
	endpoint := m.BaseURL + "/indexes/" + index + "/documents"
	return httpJSON(ctx, m.HTTPClient, http.MethodPost, endpoint, m.headers(), []sharedMeiliDoc{doc}, nil)
}

// SearchMessages calls `POST /indexes/:i/search` with a
// `tenant_id` filter so the shared index returns only documents
// owned by the calling tenant. Defence-in-depth: even if the
// caller (Service.Search) ever forgot to validate the tenant id,
// the filter would still block cross-tenant reads.
func (m *SharedMeilisearchBackend) SearchMessages(ctx context.Context, tenantID, query string, limit int) ([]SearchHit, error) {
	index, err := m.indexFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	endpoint := m.BaseURL + "/indexes/" + index + "/search"
	body := map[string]any{
		"q":      query,
		"limit":  limit,
		"filter": tenantFilter(tenantID),
	}
	var resp struct {
		Hits []struct {
			MessageID string  `json:"message_id"`
			Subject   string  `json:"subject"`
			Snippet   string  `json:"snippet"`
			Score     float64 `json:"_score"`
		} `json:"hits"`
	}
	if err := httpJSON(ctx, m.HTTPClient, http.MethodPost, endpoint, m.headers(), body, &resp); err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(resp.Hits))
	for _, h := range resp.Hits {
		out = append(out, SearchHit{
			MessageID: h.MessageID,
			Subject:   h.Subject,
			Snippet:   h.Snippet,
			Score:     h.Score,
		})
	}
	return out, nil
}

// DeleteIndex removes EVERY document owned by `tenantID` from
// the shared index. It MUST NOT drop the shared index itself —
// other tenants on the same shard depend on it. We use
// Meilisearch's `documents/delete` endpoint with a `tenant_id`
// filter rather than the per-index `DELETE` call the legacy
// MeilisearchBackend uses.
func (m *SharedMeilisearchBackend) DeleteIndex(ctx context.Context, tenantID string) error {
	index, err := m.indexFor(ctx, tenantID)
	if err != nil {
		return err
	}
	endpoint := m.BaseURL + "/indexes/" + index + "/documents/delete"
	body := map[string]any{"filter": tenantFilter(tenantID)}
	if err := httpJSON(ctx, m.HTTPClient, http.MethodPost, endpoint, m.headers(), body, nil); err != nil {
		// 404 means the shared index was never created — nothing
		// to delete. Treat as success.
		if isNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// MigrateIndex bulk-imports `msgs` into the shared index for
// `tenantID`. It does NOT clear the tenant's documents first —
// `Service.reindexInto` (the only production caller) always calls
// `DeleteIndex` immediately before `MigrateIndex`, so doing the
// clear here too caused a redundant `_delete_by_query` round-trip
// per cutover. Mirroring the per-tenant `MeilisearchBackend.
// MigrateIndex` semantics keeps the shared and per-tenant backends
// interchangeable behind the `Backend` interface and means a
// future caller can opt out of the clear if it wants append-only
// behaviour. The empty-msgs branch is preserved so the caller can
// fan out a "make sure the index exists + settings are applied"
// pass without a write payload.
func (m *SharedMeilisearchBackend) MigrateIndex(ctx context.Context, tenantID string, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	index, err := m.indexFor(ctx, tenantID)
	if err != nil {
		return err
	}
	if err := m.ensureSettings(ctx, index); err != nil {
		return fmt.Errorf("migrate: ensure settings: %w", err)
	}
	// Same composite-primary-key invariant as IndexMessage: wrap
	// every Message with a `doc_id = <tenant>:<message>` so the
	// bulk import can't collide two tenants with the same
	// Stalwart-issued message id.
	docs := make([]sharedMeiliDoc, len(msgs))
	for i, msg := range msgs {
		docs[i] = sharedMeiliDoc{
			Message: msg,
			DocID:   sharedDocID(msg.TenantID, msg.MessageID),
		}
	}
	endpoint := m.BaseURL + "/indexes/" + index + "/documents"
	return httpJSON(ctx, m.HTTPClient, http.MethodPost, endpoint, m.headers(), docs, nil)
}

// ExportMessages pages through every document owned by `tenantID`
// in the shared index. Used by the cutover worker as the migration
// source when promoting a tenant from `shared_meilisearch` to
// `shared_opensearch`.
func (m *SharedMeilisearchBackend) ExportMessages(ctx context.Context, tenantID string) ([]Message, error) {
	index, err := m.indexFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	const pageSize = 1000
	offset := 0
	var out []Message
	for {
		// Meilisearch's `GET /documents` accepts a `filter`
		// query-string param of the same shape as the search
		// filter expression; we URL-encode the value below.
		endpoint := fmt.Sprintf("%s/indexes/%s/documents?limit=%d&offset=%d&filter=%s",
			m.BaseURL, index, pageSize, offset, pathEscape(tenantFilter(tenantID)))
		var resp struct {
			Results []Message `json:"results"`
			Total   int       `json:"total"`
		}
		if err := httpJSON(ctx, m.HTTPClient, http.MethodGet, endpoint, m.headers(), nil, &resp); err != nil {
			if isNotFound(err) {
				return nil, nil
			}
			return nil, err
		}
		if len(resp.Results) == 0 {
			break
		}
		out = append(out, resp.Results...)
		if len(resp.Results) < pageSize {
			break
		}
		offset += pageSize
	}
	return out, nil
}

// EnsureIndex pushes the shared-index settings for `shardID` (so
// the shared index is filterable on `tenant_id` and searchable
// on the expected fields). Used by `EnsureSharedIndexes` at
// startup so the first per-tenant document doesn't have to pay
// the ensureSettings cost. Idempotent.
func (m *SharedMeilisearchBackend) EnsureIndex(ctx context.Context, shardID string) error {
	if shardID == "" {
		return errors.New("shardID required")
	}
	return m.ensureSettings(ctx, sharedIndexNameFor(shardID))
}

// ensureSettings is the lazy variant of EnsureIndex used by the
// per-message paths. Safe to call concurrently — the only side
// effect is a Meilisearch PATCH that's idempotent on the
// settings shape we pass.
func (m *SharedMeilisearchBackend) ensureSettings(ctx context.Context, index string) error {
	m.settingsMu.Lock()
	if m.settingsApplied[index] {
		m.settingsMu.Unlock()
		return nil
	}
	m.settingsMu.Unlock()

	// Meilisearch auto-creates an index on first document write,
	// but only the documents-write path picks up the primaryKey
	// hint. We push the primary key explicitly as `doc_id`
	// (composite `<tenant_id>:<message_id>` via `sharedDocID`)
	// so the shared index can never collide two tenants on the
	// same Stalwart-issued `message_id`. `message_id` alone is
	// per-account, not per-shard — keying on it directly would
	// silently overwrite cross-tenant documents. See the
	// `sharedMeiliDoc` doc comment for the full invariant.
	createBody := map[string]any{
		"uid":        index,
		"primaryKey": "doc_id",
	}
	if err := httpJSON(ctx, m.HTTPClient, http.MethodPost, m.BaseURL+"/indexes", m.headers(), createBody, nil); err != nil {
		// `400` is returned when the index already exists, in
		// which case we want to fall through to the PATCH. Any
		// other error means we cannot make progress.
		if !isConflict(err) {
			return err
		}
	}

	settingsBody := map[string]any{
		"filterableAttributes": []string{"tenant_id", "mailbox_id"},
		"searchableAttributes": []string{"subject", "snippet", "from", "to"},
		"sortableAttributes":   []string{"received_at"},
	}
	if err := httpJSON(ctx, m.HTTPClient, http.MethodPatch, m.BaseURL+"/indexes/"+index+"/settings", m.headers(), settingsBody, nil); err != nil {
		return err
	}

	m.settingsMu.Lock()
	m.settingsApplied[index] = true
	m.settingsMu.Unlock()
	return nil
}

// indexFor resolves the shared-index name for `tenantID` by
// looking up the tenant's Stalwart shard. Returns an error if
// the resolver fails — the caller cannot proceed without a
// destination index.
func (m *SharedMeilisearchBackend) indexFor(ctx context.Context, tenantID string) (string, error) {
	if tenantID == "" {
		return "", errors.New("tenantID required")
	}
	shardID, err := m.Shards.ShardForTenant(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("resolve shard: %w", err)
	}
	if shardID == "" {
		return "", errors.New("shard resolver returned empty shard id")
	}
	return sharedIndexNameFor(shardID), nil
}

func (m *SharedMeilisearchBackend) headers() http.Header {
	h := http.Header{}
	if m.APIKey != "" {
		h.Set("Authorization", "Bearer "+m.APIKey)
	}
	return h
}

// tenantFilter builds the Meilisearch filter clause that enforces
// per-tenant isolation. We embed the tenantID directly because
// Meilisearch's filter grammar requires it as a string literal,
// but we quote with single quotes and escape any embedded quote
// so an attacker who somehow influences a tenant id (impossible
// given they're DB-issued UUIDs, but defence in depth) can't
// break out of the filter expression.
func tenantFilter(tenantID string) string {
	return "tenant_id = '" + escapeMeiliFilterValue(tenantID) + "'"
}

// escapeMeiliFilterValue escapes single quotes in a string literal
// used inside Meilisearch's filter grammar. The grammar uses
// backslash-escapes inside single-quoted strings (per the
// Meilisearch documentation on filter expressions).
func escapeMeiliFilterValue(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' || c == '\'' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	return string(out)
}

// isConflict matches the httpJSON error string for the
// index-already-exists response from `POST /indexes`.
// Meilisearch emits HTTP 400 (not 409) with an `index_already_exists`
// error code; we match either signal so the caller is robust to
// future Meilisearch versions that switch to 409.
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, ": 409 ") ||
		strings.Contains(msg, "index_already_exists") ||
		(strings.Contains(msg, "Index `") && strings.Contains(msg, "already exists"))
}
