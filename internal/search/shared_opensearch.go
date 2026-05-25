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
		BaseURL:    baseURL,
		Username:   username,
		Password:   password,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
		Shards:     shards,
	}, nil
}

// Name returns "shared_opensearch".
func (o *SharedOpenSearchBackend) Name() string { return BackendSharedOpenSearch }

// IndexMessage upserts one document. The OpenSearch `_id` is
// composed of `{tenant_id}:{message_id}` so two tenants sharing
// the same Stalwart-issued message id (which is per-account, not
// per-shard) cannot collide in the shared index.
func (o *SharedOpenSearchBackend) IndexMessage(ctx context.Context, msg Message) error {
	index, err := o.indexFor(ctx, msg.TenantID)
	if err != nil {
		return err
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

// MigrateIndex deletes the tenant's documents and then bulk-
// imports `msgs` via `_bulk`. The bulk header uses the per-
// tenant doc id so re-indexing is idempotent on the (tenant,
// message) pair.
func (o *SharedOpenSearchBackend) MigrateIndex(ctx context.Context, tenantID string, msgs []Message) error {
	if err := o.DeleteIndex(ctx, tenantID); err != nil {
		return fmt.Errorf("migrate: clear tenant: %w", err)
	}
	if len(msgs) == 0 {
		return nil
	}
	index, err := o.indexFor(ctx, tenantID)
	if err != nil {
		return err
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
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("opensearch shared bulk: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
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
// `resource_already_exists_exception` is treated as success.
func (o *SharedOpenSearchBackend) EnsureIndex(ctx context.Context, shardID string) error {
	if shardID == "" {
		return errors.New("shardID required")
	}
	index := sharedIndexNameFor(shardID)
	body := map[string]any{
		"mappings": map[string]any{
			"properties": map[string]any{
				"tenant_id":   map[string]any{"type": "keyword"},
				"mailbox_id":  map[string]any{"type": "keyword"},
				"message_id":  map[string]any{"type": "keyword"},
				"subject":     map[string]any{"type": "text"},
				"snippet":     map[string]any{"type": "text"},
				"from":        map[string]any{"type": "text"},
				"to":          map[string]any{"type": "text"},
				"received_at": map[string]any{"type": "date"},
			},
		},
	}
	if err := o.do(ctx, http.MethodPut, o.BaseURL+"/"+index, body, nil); err != nil {
		// OpenSearch returns 400 (`resource_already_exists_exception`)
		// when the index is present from a previous startup.
		if isAlreadyExists(err) {
			return nil
		}
		return err
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
