package search

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// newSharedOpenSearch wires the backend against a fake
// OpenSearch server backed by `handler`. The test resolver
// returns a fixed shard id so the index name in upstream calls
// is fully deterministic.
func newSharedOpenSearch(t *testing.T, shardID string, handler http.HandlerFunc) (*SharedOpenSearchBackend, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	resolver := &fakeShardResolver{shardID: shardID}
	b, err := NewSharedOpenSearchBackend(srv.URL, "user", "pass", resolver)
	if err != nil {
		t.Fatalf("NewSharedOpenSearchBackend: %v", err)
	}
	return b, srv
}

// TestSharedOpenSearch_NewValidation exercises the constructor
// rejecting invalid configurations.
func TestSharedOpenSearch_NewValidation(t *testing.T) {
	if _, err := NewSharedOpenSearchBackend("", "u", "p", &fakeShardResolver{}); err == nil {
		t.Fatal("expected error on empty baseURL")
	}
	if _, err := NewSharedOpenSearchBackend("http://x", "u", "p", nil); err == nil {
		t.Fatal("expected error on nil resolver")
	}
}

// TestSharedOpenSearch_Name pins the backend identifier.
func TestSharedOpenSearch_Name(t *testing.T) {
	b, _ := newSharedOpenSearch(t, "shard-1", func(http.ResponseWriter, *http.Request) {})
	if got := b.Name(); got != BackendSharedOpenSearch {
		t.Errorf("Name() = %q, want %q", got, BackendSharedOpenSearch)
	}
}

// TestSharedOpenSearch_IndexMessageRoutesAndPrefixes verifies
// the URL uses the shared index for the resolver's shard AND the
// document `_id` is namespaced with the tenant prefix so two
// tenants sharing the same per-account message id do not
// collide in the shared index.
func TestSharedOpenSearch_IndexMessageRoutesAndPrefixes(t *testing.T) {
	const shard = "stalwart-shard-b"
	var (
		mu         sync.Mutex
		gotPath    string
		gotAuth    string
		gotMessage Message
	)
	b, _ := newSharedOpenSearch(t, shard, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotMessage)
		w.WriteHeader(http.StatusCreated)
	})

	msg := Message{TenantID: "tenant-q", MessageID: "msg-1", Subject: "x"}
	if err := b.IndexMessage(context.Background(), msg); err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}
	wantIndex := "kmail_shared_" + strings.ReplaceAll(shard, "-", "")
	wantPath := "/" + wantIndex + "/_doc/" + "tenant-q:msg-1"
	if gotPath != wantPath {
		t.Errorf("URL path = %q, want %q", gotPath, wantPath)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("Authorization = %q, want Basic prefix", gotAuth)
	}
	if gotMessage.TenantID != "tenant-q" || gotMessage.MessageID != "msg-1" {
		t.Errorf("indexed doc = %+v, want tenant-q/msg-1", gotMessage)
	}
}

// TestSharedOpenSearch_SearchAppliesTenantFilter verifies that
// the search query body always contains a `term: tenant_id`
// filter so cross-tenant docs are dropped at query time.
func TestSharedOpenSearch_SearchAppliesTenantFilter(t *testing.T) {
	const tenantID = "tenant-search-os"
	var capturedBody map[string]any
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &capturedBody)
		_, _ = io.WriteString(w, `{"hits":{"hits":[]}}`)
	})

	if _, err := b.SearchMessages(context.Background(), tenantID, "quarterly", 10); err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	// Walk the body: query.bool.filter[0].term.tenant_id == tenantID.
	query, _ := capturedBody["query"].(map[string]any)
	boolQ, _ := query["bool"].(map[string]any)
	filterArr, _ := boolQ["filter"].([]any)
	if len(filterArr) != 1 {
		t.Fatalf("filter array len = %d, want 1", len(filterArr))
	}
	term, _ := filterArr[0].(map[string]any)["term"].(map[string]any)
	got, _ := term["tenant_id"].(string)
	if got != tenantID {
		t.Errorf("filter tenant_id = %q, want %q", got, tenantID)
	}
	// And the must clause is the user's text query.
	mustArr, _ := boolQ["must"].([]any)
	if len(mustArr) != 1 {
		t.Fatalf("must array len = %d, want 1", len(mustArr))
	}
	mm, _ := mustArr[0].(map[string]any)["multi_match"].(map[string]any)
	if mm["query"] != "quarterly" {
		t.Errorf("multi_match.query = %v, want 'quarterly'", mm["query"])
	}
}

// TestSharedOpenSearch_Search404IsEmpty verifies a brand-new
// shard whose index has not been created yet returns nil, nil
// rather than an error. Matches the per-tenant backend's
// established behaviour for an empty index.
func TestSharedOpenSearch_Search404IsEmpty(t *testing.T) {
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "index_not_found_exception", http.StatusNotFound)
	})
	hits, err := b.SearchMessages(context.Background(), "tenant-q", "hello", 5)
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("hits = %d, want 0 on 404", len(hits))
	}
}

// TestSharedOpenSearch_DeleteIndexIsByQuery verifies the shared
// backend NEVER drops the index — it uses `_delete_by_query`
// with a tenant filter so other tenants on the same shard keep
// their data.
func TestSharedOpenSearch_DeleteIndexIsByQuery(t *testing.T) {
	const tenantID = "tenant-del"
	var (
		mu              sync.Mutex
		deletes         int
		indexDropCalled bool
		capturedTenant  string
	)
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodDelete && !strings.Contains(r.URL.Path, "_delete_by_query") {
			indexDropCalled = true
			return
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "_delete_by_query") {
			deletes++
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			query, _ := parsed["query"].(map[string]any)
			term, _ := query["term"].(map[string]any)
			capturedTenant, _ = term["tenant_id"].(string)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := b.DeleteIndex(context.Background(), tenantID); err != nil {
		t.Fatalf("DeleteIndex: %v", err)
	}
	if indexDropCalled {
		t.Error("DeleteIndex issued DELETE /kmail_shared_... — must not drop the shared index")
	}
	if deletes != 1 || capturedTenant != tenantID {
		t.Errorf("delete_by_query calls=%d tenant=%q, want 1 with %q", deletes, capturedTenant, tenantID)
	}
}

// TestSharedOpenSearch_DeleteIndex404IsSuccess covers a fresh
// shard whose shared index has not been created yet. 404 must
// be treated as a no-op so a cutover retry does not loop.
func TestSharedOpenSearch_DeleteIndex404IsSuccess(t *testing.T) {
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "index_not_found_exception", http.StatusNotFound)
	})
	if err := b.DeleteIndex(context.Background(), "tenant-x"); err != nil {
		t.Errorf("DeleteIndex 404 returned %v, want nil", err)
	}
}

// TestSharedOpenSearch_MigrateIndexUsesBulk verifies the
// migration path issues a bulk request and namespaces every doc
// id with the tenant prefix so the shared index keeps tenant
// isolation even when two tenants share the same per-account
// message id. MigrateIndex itself MUST NOT issue a
// `_delete_by_query` — `Service.reindexInto` already calls
// `DeleteIndex` before this, and double-deleting wastes a network
// round-trip on every cutover.
func TestSharedOpenSearch_MigrateIndexUsesBulk(t *testing.T) {
	const tenantID = "tenant-bulk"
	var (
		mu          sync.Mutex
		deleteCalls int
		bulkCalls   int
		bulkBody    []byte
	)
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "_delete_by_query"):
			deleteCalls++
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/_bulk"):
			bulkCalls++
			bulkBody, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, `{"errors":false,"items":[]}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	msgs := []Message{
		{TenantID: tenantID, MessageID: "m1"},
		{TenantID: tenantID, MessageID: "m2"},
	}
	if err := b.MigrateIndex(context.Background(), tenantID, msgs); err != nil {
		t.Fatalf("MigrateIndex: %v", err)
	}
	if deleteCalls != 0 {
		t.Errorf("deleteCalls = %d, want 0 (MigrateIndex must not double-delete)", deleteCalls)
	}
	if bulkCalls != 1 {
		t.Fatalf("bulkCalls = %d, want 1", bulkCalls)
	}
	// Bulk is NDJSON: pairs of [header, source]. Confirm each
	// header carries the per-tenant `_id` so the shared index
	// can never collide two tenants on the same message id.
	scanner := bufio.NewScanner(bytes.NewReader(bulkBody))
	var headers []map[string]any
	lineIdx := 0
	for scanner.Scan() {
		if lineIdx%2 == 0 {
			var h map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &h); err != nil {
				t.Fatalf("bulk header parse: %v", err)
			}
			headers = append(headers, h)
		}
		lineIdx++
	}
	if len(headers) != 2 {
		t.Fatalf("got %d bulk headers, want 2", len(headers))
	}
	wantIDs := map[string]bool{tenantID + ":m1": false, tenantID + ":m2": false}
	for _, h := range headers {
		idx, _ := h["index"].(map[string]any)
		id, _ := idx["_id"].(string)
		if _, ok := wantIDs[id]; !ok {
			t.Errorf("bulk header _id = %q, want one of %v", id, wantIDs)
		}
		wantIDs[id] = true
	}
	for id, seen := range wantIDs {
		if !seen {
			t.Errorf("bulk did not include _id=%q", id)
		}
	}
}

// TestSharedOpenSearch_MigrateIndexEmptyIsNoop verifies a
// migration with zero messages issues NO HTTP calls at all.
// `Service.reindexInto` is responsible for clearing the
// destination before MigrateIndex, so the empty-msgs branch must
// stay a pure no-op — issuing a redundant `_delete_by_query` here
// would re-delete on every empty cutover.
func TestSharedOpenSearch_MigrateIndexEmptyIsNoop(t *testing.T) {
	var (
		deletes atomic.Int32
		bulks   atomic.Int32
	)
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "_delete_by_query") {
			deletes.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/_bulk") {
			bulks.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := b.MigrateIndex(context.Background(), "tenant-empty", nil); err != nil {
		t.Fatalf("MigrateIndex(nil): %v", err)
	}
	if deletes.Load() != 0 {
		t.Errorf("deletes = %d, want 0 (MigrateIndex must not double-delete)", deletes.Load())
	}
	if bulks.Load() != 0 {
		t.Errorf("bulks = %d, want 0 on empty migration", bulks.Load())
	}
}

// TestSharedOpenSearch_EnsureIndexCreatesWithMapping verifies
// that EnsureIndex pushes a mapping with `tenant_id` as a
// `keyword` field so the term filter hits the inverted index
// instead of a text analyzer (a text-analysed tenant_id would
// silently break tenant isolation).
func TestSharedOpenSearch_EnsureIndexCreatesWithMapping(t *testing.T) {
	const shard = "shard-mapping"
	var (
		mu       sync.Mutex
		gotPath  string
		gotBody  map[string]any
		gotCalls int
	)
	b, _ := newSharedOpenSearch(t, "ignored", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotPath = r.URL.Path
		gotCalls++
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	})
	if err := b.EnsureIndex(context.Background(), shard); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	wantPath := "/kmail_shared_" + strings.ReplaceAll(shard, "-", "")
	if gotPath != wantPath {
		t.Errorf("EnsureIndex path = %q, want %q", gotPath, wantPath)
	}
	mappings, _ := gotBody["mappings"].(map[string]any)
	props, _ := mappings["properties"].(map[string]any)
	tenantProp, _ := props["tenant_id"].(map[string]any)
	if tenantProp["type"] != "keyword" {
		t.Errorf("tenant_id mapping = %v, want keyword (text would defeat the term filter)", tenantProp["type"])
	}
}

// TestSharedOpenSearch_EnsureIndexSelfHealsExistingIndex pins
// the self-heal contract: when the shared index already exists
// (e.g. it was previously auto-created with a dynamic
// `tenant_id: text` mapping after a startup ensure failed),
// `EnsureIndex` must follow up with a `PUT /<index>/_mapping`
// that re-asserts the correct keyword mapping. Without this
// step a BFF restart could never recover the misconfigured
// index — operators would have to manually rebuild it.
func TestSharedOpenSearch_EnsureIndexSelfHealsExistingIndex(t *testing.T) {
	var (
		mu             sync.Mutex
		createCalls    int
		mappingCalls   int
		mappingBody    map[string]any
		mappingMethod  string
	)
	b, _ := newSharedOpenSearch(t, "ignored", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/_mapping"):
			mappingCalls++
			mappingMethod = r.Method
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &mappingBody)
			w.WriteHeader(http.StatusOK)
		default:
			createCalls++
			// Simulate "index already exists" on the create PUT.
			http.Error(w, `{"error":{"type":"resource_already_exists_exception"}}`, http.StatusBadRequest)
		}
	})
	if err := b.EnsureIndex(context.Background(), "shard-1"); err != nil {
		t.Fatalf("EnsureIndex on existing index returned %v, want nil", err)
	}
	if createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (initial PUT /<index> must be attempted)", createCalls)
	}
	if mappingCalls != 1 {
		t.Fatalf("mappingCalls = %d, want 1 (PUT /<index>/_mapping must run as self-heal)", mappingCalls)
	}
	if mappingMethod != http.MethodPut {
		t.Errorf("mapping method = %q, want PUT", mappingMethod)
	}
	props, _ := mappingBody["properties"].(map[string]any)
	tenantProp, _ := props["tenant_id"].(map[string]any)
	if tenantProp["type"] != "keyword" {
		t.Errorf("self-heal mapping tenant_id = %v, want keyword", tenantProp["type"])
	}
}

// TestSharedOpenSearch_EnsureIndexSelfHealFailureBubbles guards
// the fail-loud contract: if the existing index has `tenant_id`
// dynamically mapped as `text`, OpenSearch will reject the
// `PUT /_mapping` with `illegal_argument_exception`. We must
// surface that error rather than silently caching the index as
// "healed", so operators see the indexing failures and recreate
// the index.
func TestSharedOpenSearch_EnsureIndexSelfHealFailureBubbles(t *testing.T) {
	b, _ := newSharedOpenSearch(t, "ignored", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_mapping"):
			http.Error(w, `{"error":{"type":"illegal_argument_exception","reason":"mapper [tenant_id] cannot be changed from type [text] to [keyword]"}}`, http.StatusBadRequest)
		default:
			http.Error(w, `{"error":{"type":"resource_already_exists_exception"}}`, http.StatusBadRequest)
		}
	})
	err := b.EnsureIndex(context.Background(), "shard-1")
	if err == nil {
		t.Fatal("EnsureIndex returned nil, want error from incompatible mapping update")
	}
	if !strings.Contains(err.Error(), "update mapping on existing index") {
		t.Errorf("err = %v, want wrapped 'update mapping on existing index'", err)
	}
	// Second call should NOT cache the failure as success — the
	// mapping was never applied, so the lazy path must keep
	// retrying until the operator fixes the underlying mapping.
	err2 := b.EnsureIndex(context.Background(), "shard-1")
	if err2 == nil {
		t.Errorf("second EnsureIndex returned nil; failures must not be cached as success")
	}
}

// TestSharedOpenSearch_IndexMessageLazyMapping pins the
// load-bearing lazy mapping invariant: a write that races
// EnsureSharedIndexes (or arrives after a new shard is
// registered post-startup) MUST trigger a PUT /<index> with
// `tenant_id: keyword` BEFORE the document write hits
// OpenSearch — otherwise OpenSearch's dynamic mapping would
// auto-create the index with `tenant_id` as a `text` field and
// silently break the tenant filter for every tenant on that
// shard. After the first call the mapping is cached, so the
// second IndexMessage skips the ensure step.
func TestSharedOpenSearch_IndexMessageLazyMapping(t *testing.T) {
	var (
		mu             sync.Mutex
		mappingPuts    int
		docPuts        int
		firstCallOrder []string
	)
	b, _ := newSharedOpenSearch(t, "shard-lazy", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.Contains(r.URL.Path, "/_doc/"):
			docPuts++
			firstCallOrder = append(firstCallOrder, "doc")
			w.WriteHeader(http.StatusCreated)
		default:
			// PUT /<index> for the create — succeeds (no
			// already_exists) so no follow-up mapping call.
			mappingPuts++
			firstCallOrder = append(firstCallOrder, "mapping")
			w.WriteHeader(http.StatusOK)
		}
	})
	msg := Message{TenantID: "tenant-lazy", MessageID: "m1"}
	if err := b.IndexMessage(context.Background(), msg); err != nil {
		t.Fatalf("IndexMessage #1: %v", err)
	}
	if err := b.IndexMessage(context.Background(), msg); err != nil {
		t.Fatalf("IndexMessage #2: %v", err)
	}
	if mappingPuts != 1 {
		t.Errorf("mappingPuts = %d, want 1 (cached after first call)", mappingPuts)
	}
	if docPuts != 2 {
		t.Errorf("docPuts = %d, want 2", docPuts)
	}
	if len(firstCallOrder) < 2 || firstCallOrder[0] != "mapping" || firstCallOrder[1] != "doc" {
		t.Errorf("call order = %v, want [mapping, doc, ...] (mapping must precede first doc write)", firstCallOrder)
	}
}

// TestSharedOpenSearch_MigrateIndexLazyMapping verifies the
// bulk migration path also lazy-ensures the mapping before its
// `_bulk` payload lands — the cutover worker is precisely the
// path most likely to write into a fresh shared index, so its
// first call has to pin the keyword mapping.
func TestSharedOpenSearch_MigrateIndexLazyMapping(t *testing.T) {
	var (
		mu          sync.Mutex
		mappingPuts int
		bulkPosts   int
		order       []string
	)
	b, _ := newSharedOpenSearch(t, "shard-mig", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/_bulk"):
			bulkPosts++
			order = append(order, "bulk")
			_, _ = io.WriteString(w, `{"errors":false,"items":[]}`)
		default:
			mappingPuts++
			order = append(order, "mapping")
			w.WriteHeader(http.StatusOK)
		}
	})
	msgs := []Message{{TenantID: "tenant-mig", MessageID: "m1"}}
	if err := b.MigrateIndex(context.Background(), "tenant-mig", msgs); err != nil {
		t.Fatalf("MigrateIndex: %v", err)
	}
	if mappingPuts != 1 {
		t.Errorf("mappingPuts = %d, want 1 (mapping must be ensured before bulk)", mappingPuts)
	}
	if bulkPosts != 1 {
		t.Errorf("bulkPosts = %d, want 1", bulkPosts)
	}
	if len(order) != 2 || order[0] != "mapping" || order[1] != "bulk" {
		t.Errorf("call order = %v, want [mapping, bulk]", order)
	}
}

// TestSharedOpenSearch_ExportTermFiltersByTenant verifies the
// migration source path (`ExportMessages`) only returns docs
// owned by `tenantID`. Mirrors the cutover worker's call shape.
func TestSharedOpenSearch_ExportTermFiltersByTenant(t *testing.T) {
	const tenantID = "tenant-export"
	var capturedTenant string
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		// Only capture the tenant filter on the initial scroll
		// open — the scroll-clear DELETE that runs at the end
		// posts a different body shape (`{scroll_id}`) which
		// would silently overwrite our capture with "".
		if strings.HasSuffix(r.URL.Path, "/_search") {
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			query, _ := parsed["query"].(map[string]any)
			term, _ := query["term"].(map[string]any)
			capturedTenant, _ = term["tenant_id"].(string)
			_, _ = io.WriteString(w, `{"_scroll_id":"s1","hits":{"hits":[{"_source":{"tenant_id":"`+tenantID+`","message_id":"m1"}}]}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	out, err := b.ExportMessages(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("ExportMessages: %v", err)
	}
	if capturedTenant != tenantID {
		t.Errorf("export term.tenant_id = %q, want %q", capturedTenant, tenantID)
	}
	if len(out) != 1 || out[0].MessageID != "m1" {
		t.Errorf("export = %+v, want 1 message m1", out)
	}
}

// TestSharedOpenSearch_MigrateIndexPartialBulkFailureSurfaces
// is the regression guard for the Devin-Review flag that the
// previous code returned nil on HTTP 200 even when the bulk
// response body's `errors:true` flag was set. The cutover worker
// would then have called `MarkCompleted` while a fraction of the
// tenant's messages were missing from the destination shared
// index — a silent data-loss path.
func TestSharedOpenSearch_MigrateIndexPartialBulkFailureSurfaces(t *testing.T) {
	const tenantID = "tenant-partial"
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_bulk") {
			// HTTP 200 but `errors:true` with a per-item
			// rejection — exactly the silent-failure shape the
			// bot flagged.
			_, _ = io.WriteString(w, `{
				"took": 5,
				"errors": true,
				"items": [
					{"index": {"_id": "tenant-partial:m1", "status": 201}},
					{"index": {"_id": "tenant-partial:m2", "status": 400, "error": {"type": "mapper_parsing_exception", "reason": "field [tenant_id] is text, expected keyword"}}}
				]
			}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	msgs := []Message{
		{TenantID: tenantID, MessageID: "m1"},
		{TenantID: tenantID, MessageID: "m2"},
	}
	err := b.MigrateIndex(context.Background(), tenantID, msgs)
	if err == nil {
		t.Fatal("MigrateIndex returned nil on errors:true; expected error to bubble so the cutover worker does NOT MarkCompleted")
	}
	if !errors.Is(err, errBulkPartialFailure) {
		t.Errorf("err = %v, want wrapped errBulkPartialFailure", err)
	}
	if !strings.Contains(err.Error(), "tenant-partial:m2") {
		t.Errorf("err = %q, want item id 'tenant-partial:m2' in message", err.Error())
	}
	// Pin the no-double-prefix invariant. The shared caller wraps
	// the helper's neutral message with "opensearch shared bulk:",
	// so the final shape MUST:
	//   - contain "opensearch shared bulk:" exactly once
	//   - NOT contain "opensearch bulk:" anywhere (would mean the
	//     per-tenant prefix leaked into a shared-path log line)
	//   - contain exactly one "bulk:" segment overall (would mean
	//     the helper re-added its own "bulk:" producing
	//     "opensearch shared bulk: bulk: per-item failure", the
	//     real Devin Review flag we are guarding against).
	if !strings.Contains(err.Error(), "opensearch shared bulk:") {
		t.Errorf("err = %q, want 'opensearch shared bulk:' prefix", err.Error())
	}
	if strings.Contains(err.Error(), "opensearch bulk:") {
		t.Errorf("err = %q, must not double-prefix with 'opensearch bulk:' inside the shared wrapper", err.Error())
	}
	if got := strings.Count(err.Error(), "bulk:"); got != 1 {
		t.Errorf("err = %q, want exactly one 'bulk:' segment, got %d (double-stage prefix regression)", err.Error(), got)
	}
}

// TestSharedOpenSearch_MigrateIndexBulkErrorsFalseSucceeds pins
// the no-op path so an entirely-successful bulk (the common case)
// does NOT erroneously trip the partial-failure check.
func TestSharedOpenSearch_MigrateIndexBulkErrorsFalseSucceeds(t *testing.T) {
	b, _ := newSharedOpenSearch(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/_bulk") {
			_, _ = io.WriteString(w, `{"took":3,"errors":false,"items":[{"index":{"_id":"t:1","status":201}}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := b.MigrateIndex(context.Background(), "t", []Message{{TenantID: "t", MessageID: "1"}}); err != nil {
		t.Fatalf("MigrateIndex on clean bulk returned %v, want nil", err)
	}
}

// TestSharedOpenSearch_ResolverErrorIsBubbled confirms a shard
// resolver failure bubbles up so callers (writes, reads, the
// init loop) can decide whether to retry or alert.
func TestSharedOpenSearch_ResolverErrorIsBubbled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	resolver := &fakeShardResolver{err: errors.New("boom")}
	b, err := NewSharedOpenSearchBackend(srv.URL, "u", "p", resolver)
	if err != nil {
		t.Fatalf("NewSharedOpenSearchBackend: %v", err)
	}
	if err := b.IndexMessage(context.Background(), Message{TenantID: "t1"}); err == nil || !strings.Contains(err.Error(), "resolve shard") {
		t.Errorf("IndexMessage err = %v, want 'resolve shard' wrapped", err)
	}
}
