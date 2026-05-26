// Package search — shared OpenSearch backend.
//
// Collapses N tenants on the same Stalwart shard into ONE
// OpenSearch index (`kmail_shared_<shard>`). Tenant isolation is
// enforced via a `term` filter on `tenant_id` on every read /
// delete query and a per-tenant `_id` prefix on every indexed
// document so the same `message_id` belonging to two different
// tenants does not collide.
//
// This is the auto-promotion target when a `shared_meilisearch`
// tenant outgrows the single-node Meilisearch ceiling.
package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// SharedOpenSearchBackend implements SearchBackend against a
// shared OpenSearch index per Stalwart shard. The Shards resolver
// is required.
type SharedOpenSearchBackend struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
	Shards     ShardResolver

	// mappingApplied tracks which shared indexes have already
	// had their mapping (in particular `tenant_id: keyword`)
	// pushed in this process. We track this so the lazy path
	// in `IndexMessage` / `MigrateIndex` pays the PUT cost at
	// most once per shard per process — but the lazy path
	// itself is load-bearing because without it OpenSearch
	// auto-creates the index with dynamic mapping on first
	// write, which silently turns `tenant_id` into a
	// `text`-analyzed field and breaks the `term` tenant
	// filter for every tenant on that shard. The map is small
	// (~one entry per Stalwart shard) and a single Mutex is
	// sufficient.
	mappingMu      sync.Mutex
	mappingApplied map[string]bool
}

// NewSharedOpenSearchBackend builds a SharedOpenSearchBackend
// wired against the given OpenSearch instance.
func NewSharedOpenSearchBackend(baseURL, username, password string, shards ShardResolver) (*SharedOpenSearchBackend, error) {
	if baseURL == "" {
		return nil, errors.New("search.NewSharedOpenSearchBackend: baseURL is required")
	}
	if shards == nil {
		return nil, errors.New("search.NewSharedOpenSearchBackend: shards resolver is required")
	}
	return &SharedOpenSearchBackend{
		BaseURL:        baseURL,
		Username:       username,
		Password:       password,
		HTTPClient:     &http.Client{Timeout: 10 * time.Second},
		Shards:         shards,
		mappingApplied: map[string]bool{},
	}, nil
}

// Name returns "shared_opensearch".
func (o *SharedOpenSearchBackend) Name() string { return BackendSharedOpenSearch }

// IndexMessage upserts one document. The OpenSearch `_id` is
// composed of `{tenant_id}:{message_id}` so two tenants sharing
// the same Stalwart-issued message id (which is per-account, not
// per-shard) cannot collide in the shared index.
//
// The mapping is ensured lazily on the first write per shard per
// process. Without that, a write that races ahead of
// `EnsureSharedIndexes` (e.g. startup ensure failed, or a new
// shard was registered after startup) would let OpenSearch
// auto-create the index with dynamic mapping — `tenant_id` would
// become a `text` field, the `term` filter would no longer match
// any document, and every tenant on the shard would silently get
// empty search results.
func (o *SharedOpenSearchBackend) IndexMessage(ctx context.Context, msg Message) error {
	index, err := o.indexFor(ctx, msg.TenantID)
	if err != nil {
		return err
	}
	if err := o.ensureMapping(ctx, index); err != nil {
		return fmt.Errorf("ensure mapping: %w", err)
	}
	docID := sharedDocID(msg.TenantID, msg.MessageID)
	endpoint := o.BaseURL + "/" + index + "/_doc/" + pathEscape(docID)
	return o.do(ctx, http.MethodPut, endpoint, msg, nil)
}

// SearchMessages runs a `bool { filter: tenant_id, must:
// multi_match }` query against the shared index. The `filter`
// clause is what enforces tenant isolation; the `must` is the
// free-text part that produces a relevance score.
func (o *SharedOpenSearchBackend) SearchMessages(ctx context.Context, tenantID, query string, limit int) ([]SearchHit, error) {
	index, err := o.indexFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	endpoint := o.BaseURL + "/" + index + "/_search"
	body := map[string]any{
		"size": limit,
		"query": map[string]any{
			"bool": map[string]any{
				"filter": []map[string]any{
					{"term": map[string]any{"tenant_id": tenantID}},
				},
				"must": []map[string]any{
					{"multi_match": map[string]any{
						"query":  query,
						"fields": []string{"subject", "snippet", "from", "to"},
					}},
				},
			},
		},
	}
	var resp struct {
		Hits struct {
			Hits []struct {
				ID     string  `json:"_id"`
				Score  float64 `json:"_score"`
				Source struct {
					MessageID string `json:"message_id"`
					Subject   string `json:"subject"`
					Snippet   string `json:"snippet"`
				} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := o.do(ctx, http.MethodPost, endpoint, body, &resp); err != nil {
		// 404 on a brand-new shard (index not yet created)
		// returns an empty result, not an error — same shape
		// the per-tenant backend returns for an empty index.
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]SearchHit, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		// Prefer the explicit `message_id` field over the
		// composite `_id` so the response is decoupled from
		// the per-tenant prefix encoding.
		mid := h.Source.MessageID
		if mid == "" {
			mid = stripDocIDPrefix(tenantID, h.ID)
		}
		out = append(out, SearchHit{
			MessageID: mid,
			Subject:   h.Source.Subject,
			Snippet:   h.Source.Snippet,
			Score:     h.Score,
		})
	}
	return out, nil
}

// DeleteIndex removes EVERY document owned by `tenantID` from the
// shared index via `_delete_by_query`. It must NOT drop the
// shared index itself — other tenants on the same shard depend
// on it.
func (o *SharedOpenSearchBackend) DeleteIndex(ctx context.Context, tenantID string) error {
	index, err := o.indexFor(ctx, tenantID)
	if err != nil {
		return err
	}
	endpoint := o.BaseURL + "/" + index + "/_delete_by_query?refresh=true&conflicts=proceed"
	body := map[string]any{
		"query": map[string]any{
			"term": map[string]any{"tenant_id": tenantID},
		},
	}
	if err := o.do(ctx, http.MethodPost, endpoint, body, nil); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// MigrateIndex bulk-imports `msgs` via `_bulk` into the shared
// index for `tenantID`. The bulk header uses the per-tenant doc id
// so re-indexing is idempotent on the (tenant, message) pair.
//
// MigrateIndex does NOT clear the tenant's documents first —
// `Service.reindexInto` (the only production caller) always calls
// `DeleteIndex` immediately before `MigrateIndex`, so doing the
// clear here too caused a redundant `_delete_by_query` round-trip
// per cutover. Mirroring `OpenSearchBackend.MigrateIndex` keeps
// the per-tenant and shared backends interchangeable behind the
// `Backend` interface so a future caller can opt out of the clear
// if it wants append-only behaviour.
func (o *SharedOpenSearchBackend) MigrateIndex(ctx context.Context, tenantID string, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	index, err := o.indexFor(ctx, tenantID)
	if err != nil {
		return err
	}
	// Same lazy mapping invariant as `IndexMessage`: the bulk
	// path is the cutover worker's write path and it MUST NOT
	// be the first call to touch a fresh shared index without
	// pinning `tenant_id: keyword` first.
	if err := o.ensureMapping(ctx, index); err != nil {
		return fmt.Errorf("migrate: ensure mapping: %w", err)
	}
	endpoint := o.BaseURL + "/_bulk"
	var buf bytes.Buffer
	for _, m := range msgs {
		header := map[string]any{
			"index": map[string]any{
				"_index": index,
				"_id":    sharedDocID(tenantID, m.MessageID),
			},
		}
		if err := json.NewEncoder(&buf).Encode(header); err != nil {
			return err
		}
		if err := json.NewEncoder(&buf).Encode(m); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	if o.Username != "" {
		req.SetBasicAuth(o.Username, o.Password)
	}
	resp, err := o.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// See OpenSearchBackend.MigrateIndex for the rationale on the
	// 1 MiB read cap and the `parseBulkResponse` post-check.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("opensearch shared bulk: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// `_bulk` returns HTTP 200 even when individual items fail.
	// In the shared-index model a partial failure is especially
	// dangerous: the cutover worker would mark the (tenant, target)
	// row `completed` while a fraction of the tenant's messages
	// never made it into the destination shared index, and the
	// next user search against the new backend would return
	// silently incomplete results. The lazy-mapping self-heal in
	// `ensureMapping` covers the "tenant_id was dynamically
	// mapped as text" case for new shards but cannot rescue a
	// per-item rejection on the destination side.
	if err := parseBulkResponse(body); err != nil {
		return fmt.Errorf("opensearch shared bulk: %w", err)
	}
	return nil
}

// ExportMessages scrolls every document owned by `tenantID` in
// the shared index. Used by the cutover worker as a migration
// source when promoting a tenant to `dedicated_opensearch` or
// when migrating a shard's tenants between shared indexes.
func (o *SharedOpenSearchBackend) ExportMessages(ctx context.Context, tenantID string) ([]Message, error) {
	index, err := o.indexFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	const pageSize = 1000
	scrollEndpoint := o.BaseURL + "/" + index + "/_search?scroll=1m"
	body := map[string]any{
		"size": pageSize,
		"query": map[string]any{
			"term": map[string]any{"tenant_id": tenantID},
		},
	}
	type hit struct {
		Source Message `json:"_source"`
	}
	var resp struct {
		ScrollID string `json:"_scroll_id"`
		Hits     struct {
			Hits []hit `json:"hits"`
		} `json:"hits"`
	}
	if err := o.do(ctx, http.MethodPost, scrollEndpoint, body, &resp); err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Message
	for _, h := range resp.Hits.Hits {
		out = append(out, h.Source)
	}
	for len(resp.Hits.Hits) == pageSize {
		nextBody := map[string]any{"scroll": "1m", "scroll_id": resp.ScrollID}
		resp.Hits.Hits = nil
		if err := o.do(ctx, http.MethodPost, o.BaseURL+"/_search/scroll", nextBody, &resp); err != nil {
			_ = o.do(ctx, http.MethodDelete, o.BaseURL+"/_search/scroll", map[string]any{"scroll_id": resp.ScrollID}, nil)
			return nil, err
		}
		for _, h := range resp.Hits.Hits {
			out = append(out, h.Source)
		}
	}
	if resp.ScrollID != "" {
		_ = o.do(ctx, http.MethodDelete, o.BaseURL+"/_search/scroll", map[string]any{"scroll_id": resp.ScrollID}, nil)
	}
	return out, nil
}

// EnsureIndex creates the shared index on `shardID` with the
// mapping required for tenant filtering (`tenant_id` as a
// `keyword` so the term filter hits the inverted index instead
// of a text analyzer). Idempotent — a 400 response with
// `resource_already_exists_exception` falls through to a
// `PUT /<index>/_mapping` call that re-asserts the mapping on
// the existing index. That self-heal step matters when an earlier
// process let OpenSearch dynamically map `tenant_id` as `text`
// (e.g. because a write raced an EnsureIndex failure) — without
// it a BFF restart could never recover the misconfigured index.
func (o *SharedOpenSearchBackend) EnsureIndex(ctx context.Context, shardID string) error {
	if shardID == "" {
		return errors.New("shardID required")
	}
	return o.ensureMapping(ctx, sharedIndexNameFor(shardID))
}

// ensureMapping is the lazy variant of EnsureIndex used by the
// per-message and bulk write paths. The expensive PUT calls run
// at most once per shared index per process. Safe to call
// concurrently — the underlying OpenSearch endpoints are
// idempotent on the mapping payload we send.
func (o *SharedOpenSearchBackend) ensureMapping(ctx context.Context, index string) error {
	if index == "" {
		return errors.New("index required")
	}
	o.mappingMu.Lock()
	if o.mappingApplied[index] {
		o.mappingMu.Unlock()
		return nil
	}
	o.mappingMu.Unlock()

	if err := o.applyIndexMapping(ctx, index); err != nil {
		return err
	}

	o.mappingMu.Lock()
	o.mappingApplied[index] = true
	o.mappingMu.Unlock()
	return nil
}

// applyIndexMapping performs the network calls underlying
// `ensureMapping` without touching the cache map. Split out so
// tests (and a future force-refresh code path) can drive the
// PUT explicitly. It first PUTs the full index (mappings +
// settings); on `resource_already_exists_exception` it falls
// through to a PUT `/<index>/_mapping` that re-applies just the
// `properties` block so an existing index with a stale dynamic
// mapping is healed at startup or on the first lazy call.
func (o *SharedOpenSearchBackend) applyIndexMapping(ctx context.Context, index string) error {
	props := map[string]any{
		"tenant_id":   map[string]any{"type": "keyword"},
		"mailbox_id":  map[string]any{"type": "keyword"},
		"message_id":  map[string]any{"type": "keyword"},
		"subject":     map[string]any{"type": "text"},
		"snippet":     map[string]any{"type": "text"},
		"from":        map[string]any{"type": "text"},
		"to":          map[string]any{"type": "text"},
		"received_at": map[string]any{"type": "date"},
	}
	createBody := map[string]any{
		"mappings": map[string]any{"properties": props},
	}
	if err := o.do(ctx, http.MethodPut, o.BaseURL+"/"+index, createBody, nil); err != nil {
		if !isAlreadyExists(err) {
			return err
		}
		// Index pre-existed — re-assert the mapping in case it was
		// dynamically created on first write with `tenant_id` as a
		// `text` field. OpenSearch will reject incompatible type
		// changes with a clear error (e.g. `mapper [tenant_id]
		// cannot be changed from type [text] to [keyword]`); we
		// surface that error so operators can recreate the index.
		mappingBody := map[string]any{"properties": props}
		if err := o.do(ctx, http.MethodPut, o.BaseURL+"/"+index+"/_mapping", mappingBody, nil); err != nil {
			return fmt.Errorf("update mapping on existing index %q: %w", index, err)
		}
	}
	return nil
}

// indexFor resolves the shared-index name for `tenantID` by
// looking up the tenant's Stalwart shard.
func (o *SharedOpenSearchBackend) indexFor(ctx context.Context, tenantID string) (string, error) {
	if tenantID == "" {
		return "", errors.New("tenantID required")
	}
	shardID, err := o.Shards.ShardForTenant(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("resolve shard: %w", err)
	}
	if shardID == "" {
		return "", errors.New("shard resolver returned empty shard id")
	}
	return sharedIndexNameFor(shardID), nil
}

// do is a thin wrapper around httpJSON that injects basic auth.
func (o *SharedOpenSearchBackend) do(ctx context.Context, method, endpoint string, body any, out any) error {
	headers := http.Header{}
	if o.Username != "" {
		headers.Set("Authorization", "Basic "+sharedBasicAuth(o.Username, o.Password))
	}
	return httpJSON(ctx, o.HTTPClient, method, endpoint, headers, body, out)
}

// sharedBasicAuth duplicates `basicAuth` from `opensearch.go` so
// the two drivers don't share state via package-level functions
// — keeps refactors localised.
func sharedBasicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// sharedDocID builds the per-tenant composite OpenSearch `_id`.
// We separate the components with a colon (`:`) because tenant
// ids are UUIDs (no colons) and message ids are JMAP-issued
// short strings (also no colons in practice). The composite is
// stable so two `IndexMessage` calls for the same (tenant,
// message) replace each other instead of double-indexing.
func sharedDocID(tenantID, messageID string) string {
	return tenantID + ":" + messageID
}

// stripDocIDPrefix removes a composite tenant prefix from a doc
// id, returning the message-id portion. Used when reading hits
// that were indexed before `_source.message_id` was populated.
func stripDocIDPrefix(tenantID, docID string) string {
	prefix := tenantID + ":"
	if strings.HasPrefix(docID, prefix) {
		return docID[len(prefix):]
	}
	return docID
}

// isAlreadyExists matches OpenSearch's 400 + resource_already_exists
// signal. The body shape is `{"error":{"type":"resource_already_exists_exception",...}}`
// per OpenSearch's standard error contract.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "resource_already_exists_exception") ||
		strings.Contains(msg, ": 409 ")
}
