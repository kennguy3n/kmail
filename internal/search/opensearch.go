// Package search — OpenSearch backend.
//
// Wraps OpenSearch's REST API. OpenSearch is API-compatible with
// Elasticsearch up to OS 1.x; we use the same endpoint shapes so
// the driver works against either ("/{index}/_doc/{id}",
// "/{index}/_search", "/{index}", "/{index}/_bulk"). Authentication
// is HTTP Basic against the configured username + password (the
// AWS IAM v4 path is out of scope for the MVP — operators that
// need it can run an `opensearch-proxy` sidecar that signs the
// request before it hits this driver).
package search

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenSearchBackend implements SearchBackend against OpenSearch.
type OpenSearchBackend struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
}

// NewOpenSearchBackend returns a backend wired against the given
// OpenSearch instance.
func NewOpenSearchBackend(baseURL, username, password string) *OpenSearchBackend {
	return &OpenSearchBackend{
		BaseURL:    baseURL,
		Username:   username,
		Password:   password,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Name returns "opensearch".
func (o *OpenSearchBackend) Name() string { return BackendOpenSearch }

// IndexMessage calls `PUT /:index/_doc/:id` so re-indexing a
// document is idempotent on (tenant, message_id).
func (o *OpenSearchBackend) IndexMessage(ctx context.Context, msg Message) error {
	endpoint := o.BaseURL + "/" + indexNameFor(msg.TenantID) + "/_doc/" + pathEscape(msg.MessageID)
	return o.do(ctx, http.MethodPut, endpoint, msg, nil)
}

// SearchMessages runs a `multi_match` against `subject`,
// `snippet`, `from`, `to`.
func (o *OpenSearchBackend) SearchMessages(ctx context.Context, tenantID, query string, limit int) ([]SearchHit, error) {
	endpoint := o.BaseURL + "/" + indexNameFor(tenantID) + "/_search"
	body := map[string]any{
		"size": limit,
		"query": map[string]any{
			"multi_match": map[string]any{
				"query":  query,
				"fields": []string{"subject", "snippet", "from", "to"},
			},
		},
	}
	var resp struct {
		Hits struct {
			Hits []struct {
				ID     string  `json:"_id"`
				Score  float64 `json:"_score"`
				Source struct {
					Subject string `json:"subject"`
					Snippet string `json:"snippet"`
				} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := o.do(ctx, http.MethodPost, endpoint, body, &resp); err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		out = append(out, SearchHit{
			MessageID: h.ID,
			Subject:   h.Source.Subject,
			Snippet:   h.Source.Snippet,
			Score:     h.Score,
		})
	}
	return out, nil
}

// DeleteIndex calls `DELETE /:index`.
func (o *OpenSearchBackend) DeleteIndex(ctx context.Context, tenantID string) error {
	endpoint := o.BaseURL + "/" + indexNameFor(tenantID)
	err := o.do(ctx, http.MethodDelete, endpoint, nil, nil)
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return nil
	}
	return err
}

// MigrateIndex bulk-imports through `POST /_bulk`. The bulk API
// uses an NDJSON-style format: alternating action header + source.
func (o *OpenSearchBackend) MigrateIndex(ctx context.Context, tenantID string, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	endpoint := o.BaseURL + "/_bulk"
	var buf bytes.Buffer
	for _, m := range msgs {
		header := map[string]any{
			"index": map[string]any{
				"_index": indexNameFor(tenantID),
				"_id":    m.MessageID,
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
		return fmt.Errorf("opensearch bulk: %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// ExportMessages scrolls every document in the tenant index. Uses
// OpenSearch's classic `_search?scroll=` API which is supported by
// every OpenSearch / Elasticsearch version KMail targets. The scroll
// keeps a server-side cursor open for `scroll` minutes so a large
// export doesn't have to fit in one HTTP response.
func (o *OpenSearchBackend) ExportMessages(ctx context.Context, tenantID string) ([]Message, error) {
	const pageSize = 1000
	scrollEndpoint := o.BaseURL + "/" + indexNameFor(tenantID) + "/_search?scroll=1m"
	body := map[string]any{
		"size":  pageSize,
		"query": map[string]any{"match_all": map[string]any{}},
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
	// Keep scrolling until a page returns zero hits.
	for len(resp.Hits.Hits) == pageSize {
		nextBody := map[string]any{"scroll": "1m", "scroll_id": resp.ScrollID}
		resp.Hits.Hits = nil
		if err := o.do(ctx, http.MethodPost, o.BaseURL+"/_search/scroll", nextBody, &resp); err != nil {
			// Best-effort cleanup, then surface the error.
			_ = o.do(ctx, http.MethodDelete, o.BaseURL+"/_search/scroll", map[string]any{"scroll_id": resp.ScrollID}, nil)
			return nil, err
		}
		for _, h := range resp.Hits.Hits {
			out = append(out, h.Source)
		}
	}
	// Always close the cursor so the cluster doesn't hold an open
	// PIT for the full `scroll=1m` window after we're done.
	if resp.ScrollID != "" {
		_ = o.do(ctx, http.MethodDelete, o.BaseURL+"/_search/scroll", map[string]any{"scroll_id": resp.ScrollID}, nil)
	}
	return out, nil
}

// do is a thin wrapper around httpJSON that injects basic auth.
func (o *OpenSearchBackend) do(ctx context.Context, method, endpoint string, body any, out any) error {
	headers := http.Header{}
	if o.Username != "" {
		headers.Set("Authorization", "Basic "+basicAuth(o.Username, o.Password))
	}
	return httpJSON(ctx, o.HTTPClient, method, endpoint, headers, body, out)
}

// basicAuth returns the base64-encoded `username:password` value
// suitable for the `Basic ` Authorization header. We avoid the
// stdlib `Request.SetBasicAuth` here because httpJSON builds the
// request internally.
func basicAuth(user, pass string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
}

// isNotFound matches the httpJSON error string for 404s.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), ": 404 ")
}
