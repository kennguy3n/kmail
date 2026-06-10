package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenSearchBackend(t *testing.T) {
	ctx := context.Background()
	var sawBulk, sawScrollDelete bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/_doc/"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_search"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"_scroll_id": "scroll-1",
				"hits": map[string]any{
					"hits": []map[string]any{
						{"_id": "m1", "_score": 2.0, "_source": map[string]any{"message_id": "m1", "tenant_id": "t1", "subject": "Hello", "snippet": "world"}},
					},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/_bulk":
			sawBulk = true
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": false, "items": []any{}})
		case r.Method == http.MethodDelete && r.URL.Path == "/_search/scroll":
			sawScrollDelete = true
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	be := NewOpenSearchBackend(srv.URL, "admin", "secret")
	if be.Name() != BackendOpenSearch {
		t.Errorf("Name=%q", be.Name())
	}

	if err := be.IndexMessage(ctx, Message{TenantID: "t1", MessageID: "m1", Subject: "Hi"}); err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}

	hits, err := be.SearchMessages(ctx, "t1", "hello", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 || hits[0].MessageID != "m1" || hits[0].Subject != "Hello" || hits[0].Score != 2.0 {
		t.Errorf("hits=%+v", hits)
	}

	if err := be.MigrateIndex(ctx, "t1", []Message{{TenantID: "t1", MessageID: "m2"}}); err != nil {
		t.Fatalf("MigrateIndex: %v", err)
	}
	if !sawBulk {
		t.Error("MigrateIndex did not POST /_bulk")
	}
	// Empty migrate is a no-op (no HTTP call).
	if err := be.MigrateIndex(ctx, "t1", nil); err != nil {
		t.Fatalf("MigrateIndex empty: %v", err)
	}

	msgs, err := be.ExportMessages(ctx, "t1")
	if err != nil {
		t.Fatalf("ExportMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].MessageID != "m1" {
		t.Errorf("ExportMessages=%+v", msgs)
	}
	if !sawScrollDelete {
		t.Error("ExportMessages did not close the scroll cursor")
	}

	if err := be.DeleteIndex(ctx, "t1"); err != nil {
		t.Fatalf("DeleteIndex: %v", err)
	}
}

func TestOpenSearchBulkPerItemFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 but errors:true → MigrateIndex must surface a failure.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": true,
			"items": []map[string]any{
				{"index": map[string]any{"status": 400, "error": map[string]any{"type": "mapper_parsing_exception"}}},
			},
		})
	}))
	defer srv.Close()
	be := NewOpenSearchBackend(srv.URL, "", "")
	err := be.MigrateIndex(context.Background(), "t1", []Message{{TenantID: "t1", MessageID: "m1"}})
	if err == nil || !strings.Contains(err.Error(), "opensearch bulk") {
		t.Errorf("expected opensearch bulk per-item failure, got %v", err)
	}
}

func TestOpenSearchDeleteIndex404IsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	be := NewOpenSearchBackend(srv.URL, "u", "p")
	if err := be.DeleteIndex(context.Background(), "t1"); err != nil {
		t.Errorf("DeleteIndex 404 should be nil, got %v", err)
	}
	msgs, err := be.ExportMessages(context.Background(), "t1")
	if err != nil || msgs != nil {
		t.Errorf("ExportMessages 404=%+v err=%v want nil,nil", msgs, err)
	}
}

func TestBasicAuthHelper(t *testing.T) {
	// admin:secret base64 → YWRtaW46c2VjcmV0
	if got := basicAuth("admin", "secret"); got != "YWRtaW46c2VjcmV0" {
		t.Errorf("basicAuth=%q", got)
	}
}
