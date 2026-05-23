// Package search — Meilisearch backend.
//
// Wraps Meilisearch's REST API (https://www.meilisearch.com/docs/reference/api/overview).
// Authentication is the master / search key passed via the
// `Authorization: Bearer <key>` header.
//
// Indexing model: one index per tenant (`kmail_<tenant>`). The
// document schema mirrors `Message`; the searchable attributes are
// `subject`, `snippet`, `from`, `to`. Meilisearch handles
// tokenisation, prefix search, and ranking out of the box.
package search

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// MeilisearchBackend implements SearchBackend against
// Meilisearch's HTTP API.
type MeilisearchBackend struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// NewMeilisearchBackend returns a backend wired against the given
// Meilisearch instance.
func NewMeilisearchBackend(baseURL, apiKey string) *MeilisearchBackend {
	return &MeilisearchBackend{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Name returns "meilisearch".
func (m *MeilisearchBackend) Name() string { return BackendMeilisearch }

// IndexMessage upserts a single document via `POST /indexes/:i/documents`.
func (m *MeilisearchBackend) IndexMessage(ctx context.Context, msg Message) error {
	endpoint := m.BaseURL + "/indexes/" + indexNameFor(msg.TenantID) + "/documents"
	return httpJSON(ctx, m.HTTPClient, http.MethodPost, endpoint, m.headers(), []Message{msg}, nil)
}

// SearchMessages calls `POST /indexes/:i/search`.
func (m *MeilisearchBackend) SearchMessages(ctx context.Context, tenantID, query string, limit int) ([]SearchHit, error) {
	endpoint := m.BaseURL + "/indexes/" + indexNameFor(tenantID) + "/search"
	body := map[string]any{
		"q":     query,
		"limit": limit,
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

// DeleteIndex calls `DELETE /indexes/:i`.
func (m *MeilisearchBackend) DeleteIndex(ctx context.Context, tenantID string) error {
	endpoint := m.BaseURL + "/indexes/" + indexNameFor(tenantID)
	err := httpJSON(ctx, m.HTTPClient, http.MethodDelete, endpoint, m.headers(), nil, nil)
	if err == nil {
		return nil
	}
	// Meilisearch returns 404 if the index never existed; treat
	// as success — the caller wanted the index gone.
	if isNotFound(err) {
		return nil
	}
	return err
}

// MigrateIndex bulk-imports `msgs` via the documents endpoint.
// Meilisearch accepts an array of documents in one call.
func (m *MeilisearchBackend) MigrateIndex(ctx context.Context, tenantID string, msgs []Message) error {
	endpoint := m.BaseURL + "/indexes/" + indexNameFor(tenantID) + "/documents"
	return httpJSON(ctx, m.HTTPClient, http.MethodPost, endpoint, m.headers(), msgs, nil)
}

// ExportMessages pages through every document in the tenant
// index. Meilisearch's `/documents` endpoint supports `offset` +
// `limit` for cursor-style pagination; we use a page size of 1000
// to keep memory bounded during the cutover.
func (m *MeilisearchBackend) ExportMessages(ctx context.Context, tenantID string) ([]Message, error) {
	const pageSize = 1000
	var out []Message
	offset := 0
	for {
		endpoint := fmt.Sprintf("%s/indexes/%s/documents?limit=%d&offset=%d",
			m.BaseURL, indexNameFor(tenantID), pageSize, offset)
		var resp struct {
			Results []Message `json:"results"`
			Total   int       `json:"total"`
		}
		if err := httpJSON(ctx, m.HTTPClient, http.MethodGet, endpoint, m.headers(), nil, &resp); err != nil {
			// 404 means the tenant has no index yet — nothing
			// to export, return an empty slice.
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

func (m *MeilisearchBackend) headers() http.Header {
	h := http.Header{}
	if m.APIKey != "" {
		h.Set("Authorization", "Bearer "+m.APIKey)
	}
	return h
}
