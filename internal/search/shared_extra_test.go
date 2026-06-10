package search

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestSharedMeilisearch_EnsureIndex drives the eager settings push
// used by EnsureSharedIndexes at startup, plus the empty-shard guard.
func TestSharedMeilisearch_EnsureIndex(t *testing.T) {
	b, _ := newSharedMeili(t, "shard-9", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/indexes":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/settings"):
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	ctx := context.Background()
	if err := b.EnsureIndex(ctx, "shard-9"); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if err := b.EnsureIndex(ctx, ""); err == nil {
		t.Error("EnsureIndex with empty shardID should error")
	}
}

// TestSharedOpenSearch_SearchStripsDocIDPrefix exercises the
// fallback in SearchMessages where a hit's _source omits
// message_id, forcing the composite "<tenant>:<id>" _id to be
// stripped back down to the bare message id.
func TestSharedOpenSearch_SearchStripsDocIDPrefix(t *testing.T) {
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_search") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hits": map[string]any{
					"hits": []map[string]any{
						// _source has no message_id ⇒ strip the _id prefix.
						{"_id": "t1:m1", "_score": 1.0, "_source": map[string]any{"subject": "S", "snippet": "X"}},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	hits, err := b.SearchMessages(context.Background(), "t1", "q", 10)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 1 || hits[0].MessageID != "m1" {
		t.Errorf("hits=%+v want MessageID=m1 (prefix stripped)", hits)
	}
}

func TestStripDocIDPrefix(t *testing.T) {
	if got := stripDocIDPrefix("t1", "t1:abc"); got != "abc" {
		t.Errorf("stripDocIDPrefix prefixed=%q want abc", got)
	}
	// No prefix ⇒ returned unchanged.
	if got := stripDocIDPrefix("t1", "raw-id"); got != "raw-id" {
		t.Errorf("stripDocIDPrefix unprefixed=%q want raw-id", got)
	}
}
