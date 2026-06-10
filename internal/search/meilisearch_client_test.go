package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMeilisearchBackend(t *testing.T) {
	ctx := context.Background()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hits": []map[string]any{
					{"message_id": "m1", "subject": "Hello", "snippet": "world", "_score": 1.5},
				},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/documents"):
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/documents"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"results": []Message{{TenantID: "t1", MessageID: "m1", Subject: "Hi"}},
				"total":   1,
			})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	be := NewMeilisearchBackend(srv.URL, "master-key")
	if be.Name() != BackendMeilisearch {
		t.Errorf("Name=%q", be.Name())
	}

	if err := be.IndexMessage(ctx, Message{TenantID: "t1", MessageID: "m1", Subject: "Hi"}); err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}
	if gotAuth != "Bearer master-key" {
		t.Errorf("auth header=%q want Bearer master-key", gotAuth)
	}

	hits, err := be.SearchMessages(ctx, "t1", "hello", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 || hits[0].MessageID != "m1" || hits[0].Score != 1.5 {
		t.Errorf("hits=%+v", hits)
	}

	if err := be.MigrateIndex(ctx, "t1", []Message{{TenantID: "t1", MessageID: "m2"}}); err != nil {
		t.Fatalf("MigrateIndex: %v", err)
	}

	msgs, err := be.ExportMessages(ctx, "t1")
	if err != nil || len(msgs) != 1 || msgs[0].MessageID != "m1" {
		t.Fatalf("ExportMessages=%+v err=%v", msgs, err)
	}

	if err := be.DeleteIndex(ctx, "t1"); err != nil {
		t.Fatalf("DeleteIndex: %v", err)
	}
}

func TestMeilisearchDeleteIndex404IsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	be := NewMeilisearchBackend(srv.URL, "")
	// A 404 from DELETE means the index was already gone → success.
	if err := be.DeleteIndex(context.Background(), "t1"); err != nil {
		t.Errorf("DeleteIndex 404 should be nil, got %v", err)
	}
	// ExportMessages on a 404 index returns no rows, no error.
	msgs, err := be.ExportMessages(context.Background(), "t1")
	if err != nil || msgs != nil {
		t.Errorf("ExportMessages 404=%+v err=%v want nil,nil", msgs, err)
	}
}

func TestMeilisearchServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	be := NewMeilisearchBackend(srv.URL, "k")
	if err := be.IndexMessage(context.Background(), Message{TenantID: "t1", MessageID: "m1"}); err == nil {
		t.Error("IndexMessage on 500 should error")
	}
	if _, err := be.SearchMessages(context.Background(), "t1", "q", 5); err == nil {
		t.Error("SearchMessages on 500 should error")
	}
}
