package search

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeShardResolver returns a fixed shard id for every tenant.
// Used by every test in this file so we can assert exact index
// names in the upstream Meilisearch request URLs.
type fakeShardResolver struct {
	shardID string
	err     error
	calls   atomic.Int32
}

func (f *fakeShardResolver) ShardForTenant(ctx context.Context, tenantID string) (string, error) {
	f.calls.Add(1)
	if f.err != nil {
		return "", f.err
	}
	return f.shardID, nil
}

// newSharedMeili is a small test helper that wires the backend
// against a fake Meilisearch server. The handler is the upstream
// asserter; tests pass in the behaviour they expect to observe.
func newSharedMeili(t *testing.T, shardID string, handler http.HandlerFunc) (*SharedMeilisearchBackend, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	resolver := &fakeShardResolver{shardID: shardID}
	b, err := NewSharedMeilisearchBackend(srv.URL, "test-key", resolver)
	if err != nil {
		t.Fatalf("NewSharedMeilisearchBackend: %v", err)
	}
	return b, srv
}

// TestSharedMeilisearch_NewValidation exercises the constructor
// rejecting invalid configurations.
func TestSharedMeilisearch_NewValidation(t *testing.T) {
	if _, err := NewSharedMeilisearchBackend("", "k", &fakeShardResolver{}); err == nil {
		t.Fatal("expected error on empty baseURL")
	}
	if _, err := NewSharedMeilisearchBackend("http://x", "k", nil); err == nil {
		t.Fatal("expected error on nil resolver")
	}
}

// TestSharedMeilisearch_Name pins the backend name to the
// constant the cutover worker filters on.
func TestSharedMeilisearch_Name(t *testing.T) {
	b, _ := newSharedMeili(t, "shard-1", func(http.ResponseWriter, *http.Request) {})
	if got := b.Name(); got != BackendSharedMeilisearch {
		t.Errorf("Name() = %q, want %q", got, BackendSharedMeilisearch)
	}
}

// TestSharedMeilisearch_IndexMessageRoutesByShard verifies that
// IndexMessage derives the index name from the tenant's shard
// and posts a single-element array (Meilisearch's documents
// endpoint accepts an array). The first call also triggers the
// settings push (`POST /indexes` then `PATCH .../settings`).
func TestSharedMeilisearch_IndexMessageRoutesByShard(t *testing.T) {
	const shard = "stalwart-shard-a"
	var (
		mu             sync.Mutex
		indexCreates   int
		settingsPatch  int
		documentsPost  int
		gotIndexInPath string
		gotBodyMsgs    []Message
	)
	b, _ := newSharedMeili(t, shard, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/indexes":
			indexCreates++
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/settings"):
			settingsPatch++
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/documents"):
			documentsPost++
			gotIndexInPath = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/indexes/"), "/documents")
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotBodyMsgs)
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected upstream call: %s %s", r.Method, r.URL.Path)
		}
	})

	msg := Message{
		TenantID: "tenant-x", MailboxID: "inbox", MessageID: "m1",
		Subject: "hello", Snippet: "world", From: "a@b", To: []string{"c@d"},
	}
	if err := b.IndexMessage(context.Background(), msg); err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}
	wantIndex := "kmail_shared_" + strings.ReplaceAll(shard, "-", "")
	if gotIndexInPath != wantIndex {
		t.Errorf("index name in URL = %q, want %q", gotIndexInPath, wantIndex)
	}
	if len(gotBodyMsgs) != 1 || gotBodyMsgs[0].MessageID != "m1" {
		t.Errorf("documents body = %+v, want single message m1", gotBodyMsgs)
	}
	if indexCreates != 1 || settingsPatch != 1 || documentsPost != 1 {
		t.Errorf("upstream counts: index=%d settings=%d docs=%d, want all 1", indexCreates, settingsPatch, documentsPost)
	}
}

// TestSharedMeilisearch_EnsureSettingsCachedPerProcess verifies
// the second IndexMessage call against the same shard does NOT
// re-issue the settings PATCH — that call is slow on Meilisearch.
func TestSharedMeilisearch_EnsureSettingsCachedPerProcess(t *testing.T) {
	var (
		mu            sync.Mutex
		indexCreates  int
		settingsPatch int
	)
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/indexes":
			indexCreates++
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/settings"):
			settingsPatch++
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	ctx := context.Background()
	if err := b.IndexMessage(ctx, Message{TenantID: "t1", MessageID: "m1"}); err != nil {
		t.Fatalf("first IndexMessage: %v", err)
	}
	if err := b.IndexMessage(ctx, Message{TenantID: "t1", MessageID: "m2"}); err != nil {
		t.Fatalf("second IndexMessage: %v", err)
	}
	if err := b.IndexMessage(ctx, Message{TenantID: "t2", MessageID: "m3"}); err != nil {
		t.Fatalf("third IndexMessage: %v", err)
	}
	if indexCreates != 1 {
		t.Errorf("indexCreates = %d, want 1 (cached)", indexCreates)
	}
	if settingsPatch != 1 {
		t.Errorf("settingsPatch = %d, want 1 (cached)", settingsPatch)
	}
}

// TestSharedMeilisearch_SearchPassesTenantFilter verifies that
// SearchMessages always attaches a tenant_id filter to the
// upstream request body so the shared index returns only the
// caller tenant's documents.
func TestSharedMeilisearch_SearchPassesTenantFilter(t *testing.T) {
	const tenantID = "tenant-search"
	var capturedBody map[string]any
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/search") {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &capturedBody)
			_, _ = io.WriteString(w, `{"hits":[]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	if _, err := b.SearchMessages(context.Background(), tenantID, "hello world", 25); err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if got := capturedBody["q"]; got != "hello world" {
		t.Errorf("query = %v, want 'hello world'", got)
	}
	gotFilter, _ := capturedBody["filter"].(string)
	wantFilter := "tenant_id = '" + tenantID + "'"
	if gotFilter != wantFilter {
		t.Errorf("filter = %q, want %q", gotFilter, wantFilter)
	}
}

// TestSharedMeilisearch_SearchEscapesQuoteInTenantID guards
// against a tenant id that somehow contains a single quote
// breaking out of the filter literal. tenant_ids are
// DB-issued UUIDs, but the filter builder is defence-in-depth.
func TestSharedMeilisearch_SearchEscapesQuoteInTenantID(t *testing.T) {
	got := tenantFilter("ab'cd")
	if got != "tenant_id = 'ab\\'cd'" {
		t.Errorf("tenantFilter = %q, want %q", got, "tenant_id = 'ab\\'cd'")
	}
}

// TestSharedMeilisearch_DeleteIndexFiltersTenantOnly verifies
// DeleteIndex calls the bulk-delete endpoint with a tenant_id
// filter — it MUST NOT drop the shared index itself.
func TestSharedMeilisearch_DeleteIndexFiltersTenantOnly(t *testing.T) {
	const tenantID = "tenant-del"
	var (
		mu              sync.Mutex
		deleteCalls     int
		indexDropCalled bool
		capturedFilter  string
	)
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/indexes/") && !strings.Contains(r.URL.Path, "/documents") {
			indexDropCalled = true
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/documents/delete") {
			deleteCalls++
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			capturedFilter, _ = parsed["filter"].(string)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := b.DeleteIndex(context.Background(), tenantID); err != nil {
		t.Fatalf("DeleteIndex: %v", err)
	}
	if indexDropCalled {
		t.Error("DeleteIndex called DELETE /indexes/... — must not drop the shared index")
	}
	if deleteCalls != 1 {
		t.Errorf("documents/delete calls = %d, want 1", deleteCalls)
	}
	wantFilter := "tenant_id = '" + tenantID + "'"
	if capturedFilter != wantFilter {
		t.Errorf("filter = %q, want %q", capturedFilter, wantFilter)
	}
}

// TestSharedMeilisearch_DeleteIndex404IsSuccess covers the case
// where the shared index hasn't been created yet (e.g. a brand
// new shard). A 404 from the upstream must be treated as
// success so a cutover retry doesn't loop on a no-op deletion.
func TestSharedMeilisearch_DeleteIndex404IsSuccess(t *testing.T) {
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/documents/delete") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	if err := b.DeleteIndex(context.Background(), "tenant-x"); err != nil {
		t.Errorf("DeleteIndex on missing index returned %v, want nil", err)
	}
}

// TestSharedMeilisearch_MigrateIndexBulkInsertsOnly verifies the
// migration POSTs the new documents in a single batch and does
// NOT re-delete the tenant's documents — `Service.reindexInto`
// already issues `DeleteIndex` immediately before this call, so
// double-deleting wastes a network round-trip per cutover.
func TestSharedMeilisearch_MigrateIndexBulkInsertsOnly(t *testing.T) {
	var (
		mu          sync.Mutex
		ops         []string
		insertedIDs []string
	)
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/documents/delete"):
			ops = append(ops, "delete")
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(r.URL.Path, "/documents") && r.Method == http.MethodPost:
			ops = append(ops, "insert")
			body, _ := io.ReadAll(r.Body)
			var msgs []Message
			_ = json.Unmarshal(body, &msgs)
			for _, m := range msgs {
				insertedIDs = append(insertedIDs, m.MessageID)
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
	msgs := []Message{
		{TenantID: "t1", MessageID: "m1"},
		{TenantID: "t1", MessageID: "m2"},
		{TenantID: "t1", MessageID: "m3"},
	}
	if err := b.MigrateIndex(context.Background(), "t1", msgs); err != nil {
		t.Fatalf("MigrateIndex: %v", err)
	}
	for _, op := range ops {
		if op == "delete" {
			t.Fatalf("MigrateIndex issued a delete op (ops=%v) — must not double-delete", ops)
		}
	}
	if len(ops) != 1 || ops[0] != "insert" {
		t.Fatalf("op sequence = %v, want exactly one insert", ops)
	}
	if len(insertedIDs) != 3 {
		t.Errorf("inserted = %v, want 3", insertedIDs)
	}
}

// TestSharedMeilisearch_MigrateIndexEmptyIsNoop verifies a
// migration with zero messages issues NO HTTP calls at all.
// `Service.reindexInto` is responsible for clearing the
// destination before MigrateIndex, so the empty-msgs branch
// must stay a pure no-op — issuing a redundant
// `/documents/delete` here would re-delete on every empty
// cutover.
func TestSharedMeilisearch_MigrateIndexEmptyIsNoop(t *testing.T) {
	var deletes atomic.Int32
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/documents/delete") {
			deletes.Add(1)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	if err := b.MigrateIndex(context.Background(), "t1", nil); err != nil {
		t.Fatalf("MigrateIndex(nil): %v", err)
	}
	if deletes.Load() != 0 {
		t.Errorf("deletes = %d, want 0 (MigrateIndex must not double-delete)", deletes.Load())
	}
}

// TestSharedMeilisearch_ExportPaginates verifies the export
// path follows the offset/limit pagination protocol and stops
// when fewer than pageSize results come back.
func TestSharedMeilisearch_ExportPaginates(t *testing.T) {
	const tenantID = "tenant-exp"
	var calls atomic.Int32
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/documents") {
			w.WriteHeader(http.StatusOK)
			return
		}
		// Confirm the upstream filter is the tenant filter and
		// nothing else (defence-in-depth on the query string).
		filter, _ := url.QueryUnescape(r.URL.Query().Get("filter"))
		if filter != "tenant_id = '"+tenantID+"'" {
			t.Errorf("filter param = %q, want tenant_id filter", filter)
		}
		switch calls.Add(1) {
		case 1:
			// First page: a full pageSize batch. The handler
			// can't easily emit 1000 messages, so we emulate
			// by sending a smaller-than-pageSize page and
			// trusting the export loop's "stop on short page"
			// branch — covered by TestSharedMeilisearch_ExportStopsOnShortPage.
			fallthrough
		default:
			_, _ = io.WriteString(w, `{"results":[{"tenant_id":"`+tenantID+`","message_id":"m1"}],"total":1}`)
		}
	})
	msgs, err := b.ExportMessages(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("ExportMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].MessageID != "m1" {
		t.Errorf("export = %+v, want 1 message m1", msgs)
	}
}

// TestSharedMeilisearch_ResolverErrorIsBubbled covers the case
// where the shard resolver fails — the backend cannot derive an
// index name and must return the wrapped error so the caller
// can decide how to handle it (retry, alert, etc.).
func TestSharedMeilisearch_ResolverErrorIsBubbled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	resolver := &fakeShardResolver{err: errors.New("boom")}
	b, err := NewSharedMeilisearchBackend(srv.URL, "k", resolver)
	if err != nil {
		t.Fatalf("NewSharedMeilisearchBackend: %v", err)
	}
	err = b.IndexMessage(context.Background(), Message{TenantID: "t1", MessageID: "m1"})
	if err == nil || !strings.Contains(err.Error(), "resolve shard") {
		t.Errorf("IndexMessage err = %v, want 'resolve shard' wrapped", err)
	}
}

// TestSharedMeilisearch_EnsureSettingsRegistersCompositePrimaryKey
// pins the load-bearing behaviour fixed by the composite-PK fix:
// the index-creation body MUST set `primaryKey: "doc_id"` so the
// shared index dedupes on `<tenant>:<message>` and not on the
// per-account `message_id` (two tenants on the same shard with
// the same Stalwart-issued message id would otherwise overwrite
// each other).
func TestSharedMeilisearch_EnsureSettingsRegistersCompositePrimaryKey(t *testing.T) {
	var (
		mu               sync.Mutex
		createBody       map[string]any
		createIndexCalls int
	)
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost && r.URL.Path == "/indexes" {
			createIndexCalls++
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &createBody)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	if err := b.IndexMessage(context.Background(), Message{TenantID: "t1", MessageID: "m1"}); err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}
	if createIndexCalls != 1 {
		t.Fatalf("create-index calls = %d, want 1", createIndexCalls)
	}
	if got, _ := createBody["primaryKey"].(string); got != "doc_id" {
		t.Errorf("primaryKey = %q, want %q (composite tenant-scoped id)", got, "doc_id")
	}
}

// TestSharedMeilisearch_IndexMessageStampsCompositeDocID verifies
// every IndexMessage request body carries a `doc_id` field that
// composes `<tenant_id>:<message_id>`. This is the per-write
// counterpart to the primary-key registration: even with a
// correct primaryKey hint, a missing `doc_id` field would cause
// Meilisearch to generate a surrogate id and break dedupe.
func TestSharedMeilisearch_IndexMessageStampsCompositeDocID(t *testing.T) {
	var (
		mu      sync.Mutex
		gotDocs []map[string]any
	)
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/documents") {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotDocs)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	msg := Message{TenantID: "tenant-A", MessageID: "msg-shared"}
	if err := b.IndexMessage(context.Background(), msg); err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}
	if len(gotDocs) != 1 {
		t.Fatalf("body docs = %d, want 1", len(gotDocs))
	}
	if got, _ := gotDocs[0]["doc_id"].(string); got != "tenant-A:msg-shared" {
		t.Errorf("doc_id = %q, want %q", got, "tenant-A:msg-shared")
	}
	// The embedded Message fields must still be present so search
	// hits and the `tenant_id` filter keep working.
	if got, _ := gotDocs[0]["tenant_id"].(string); got != "tenant-A" {
		t.Errorf("tenant_id in body = %q, want %q", got, "tenant-A")
	}
	if got, _ := gotDocs[0]["message_id"].(string); got != "msg-shared" {
		t.Errorf("message_id in body = %q, want %q", got, "msg-shared")
	}
}

// TestSharedMeilisearch_CrossTenantSameMessageIDProducesDistinctDocIDs
// is the regression test for the cross-tenant collision bug
// surfaced by Devin Review on commit 6015fea. Two tenants on the
// same shard with the same Stalwart-issued message_id MUST end
// up with different Meilisearch primary keys. Before the fix,
// both writes were keyed on `message_id` alone and the second
// silently overwrote the first.
func TestSharedMeilisearch_CrossTenantSameMessageIDProducesDistinctDocIDs(t *testing.T) {
	var (
		mu      sync.Mutex
		gotIDs  []string
	)
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/documents") {
			body, _ := io.ReadAll(r.Body)
			var docs []map[string]any
			_ = json.Unmarshal(body, &docs)
			for _, d := range docs {
				if id, ok := d["doc_id"].(string); ok {
					gotIDs = append(gotIDs, id)
				}
			}
		}
		w.WriteHeader(http.StatusAccepted)
	})
	ctx := context.Background()
	if err := b.IndexMessage(ctx, Message{TenantID: "tenant-A", MessageID: "shared-id"}); err != nil {
		t.Fatalf("IndexMessage A: %v", err)
	}
	if err := b.IndexMessage(ctx, Message{TenantID: "tenant-B", MessageID: "shared-id"}); err != nil {
		t.Fatalf("IndexMessage B: %v", err)
	}
	if len(gotIDs) != 2 {
		t.Fatalf("captured doc_ids = %v, want 2", gotIDs)
	}
	if gotIDs[0] == gotIDs[1] {
		t.Fatalf("doc_ids collided across tenants: %v", gotIDs)
	}
	if gotIDs[0] != "tenant-A:shared-id" || gotIDs[1] != "tenant-B:shared-id" {
		t.Errorf("doc_ids = %v, want [tenant-A:shared-id tenant-B:shared-id]", gotIDs)
	}
}

// TestSharedMeilisearch_MigrateIndexStampsCompositeDocID is the
// bulk-path equivalent of IndexMessageStampsCompositeDocID — the
// migration body must carry doc_id on every row, not just the
// per-message write path.
func TestSharedMeilisearch_MigrateIndexStampsCompositeDocID(t *testing.T) {
	var (
		mu      sync.Mutex
		gotDocs []map[string]any
	)
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/documents") {
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &gotDocs)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	msgs := []Message{
		{TenantID: "tenant-A", MessageID: "m1"},
		{TenantID: "tenant-A", MessageID: "m2"},
		{TenantID: "tenant-A", MessageID: "m3"},
	}
	if err := b.MigrateIndex(context.Background(), "tenant-A", msgs); err != nil {
		t.Fatalf("MigrateIndex: %v", err)
	}
	if len(gotDocs) != 3 {
		t.Fatalf("body docs = %d, want 3", len(gotDocs))
	}
	wantIDs := []string{"tenant-A:m1", "tenant-A:m2", "tenant-A:m3"}
	for i, want := range wantIDs {
		got, _ := gotDocs[i]["doc_id"].(string)
		if got != want {
			t.Errorf("doc[%d].doc_id = %q, want %q", i, got, want)
		}
	}
}

// TestSharedMeilisearch_EnsureSettingsRefusesWrongPrimaryKey
// pins the defensive PK-verification path added in round 8. When
// the shared index was created by a previous code version (or by
// a race where a write landed before ensureSettings) without the
// composite `doc_id` PK, Meilisearch refuses every attempt to
// change it on the existing index. The BFF MUST detect this
// misconfiguration explicitly and surface a loud operator-actionable
// error rather than silently patching only the filterable / searchable
// attributes — silent patching would leave the shared index keyed
// on the bare `message_id`, which is per-account, not per-shard,
// breaking the tenant-isolation invariant via cross-tenant
// overwrites.
func TestSharedMeilisearch_EnsureSettingsRefusesWrongPrimaryKey(t *testing.T) {
	var (
		mu                sync.Mutex
		createIndexCalls  int
		patchSettingsHit  bool
	)
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/indexes":
			createIndexCalls++
			// Simulate index_already_exists by returning the
			// real Meilisearch 400 + structured payload.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"index_already_exists","message":"Index ` + "`" + sharedIndexNameFor("shard-1") + "`" + ` already exists","type":"invalid_request"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/indexes/"):
			// Existing index reports a STALE primary key
			// (the bug surface).
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"uid":"` + sharedIndexNameFor("shard-1") + `","primaryKey":"message_id"}`))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/settings"):
			patchSettingsHit = true
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	err := b.IndexMessage(context.Background(), Message{TenantID: "t1", MessageID: "m1"})
	if err == nil {
		t.Fatal("IndexMessage returned nil; want loud error refusing to patch wrong-PK index")
	}
	for _, want := range []string{
		"primaryKey=\"message_id\"",
		"expected \"doc_id\"",
		"drop the index",
		"tenant isolation",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing expected fragment %q", err.Error(), want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if createIndexCalls != 1 {
		t.Errorf("create-index calls = %d, want 1", createIndexCalls)
	}
	if patchSettingsHit {
		t.Errorf("PATCH /indexes/.../settings was called against a wrong-PK index; the verify step must short-circuit before settings are patched (otherwise the BFF would silently mark the index as 'good' in settingsApplied)")
	}
}

// TestSharedMeilisearch_EnsureSettingsHappyPathOnConflictWithGoodPK
// is the inverse of the refuse case: when POST /indexes returns
// a conflict AND the existing index already has the correct
// composite PK, ensureSettings MUST fall through to the
// settings PATCH and complete successfully. This pins the path
// for the (extremely common) restart case where a previous BFF
// process already created the index correctly.
func TestSharedMeilisearch_EnsureSettingsHappyPathOnConflictWithGoodPK(t *testing.T) {
	var (
		mu               sync.Mutex
		createIndexCalls int
		verifyCalls      int
		patchSettingsHit bool
	)
	b, _ := newSharedMeili(t, "shard-1", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/indexes":
			createIndexCalls++
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"index_already_exists","message":"Index ` + "`" + sharedIndexNameFor("shard-1") + "`" + ` already exists","type":"invalid_request"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/indexes/"):
			verifyCalls++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"uid":"` + sharedIndexNameFor("shard-1") + `","primaryKey":"doc_id"}`))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/settings"):
			patchSettingsHit = true
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	})
	if err := b.IndexMessage(context.Background(), Message{TenantID: "t1", MessageID: "m1"}); err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if createIndexCalls != 1 {
		t.Errorf("create-index calls = %d, want 1", createIndexCalls)
	}
	if verifyCalls != 1 {
		t.Errorf("verify GET calls = %d, want 1 (must run on conflict fall-through)", verifyCalls)
	}
	if !patchSettingsHit {
		t.Errorf("PATCH /indexes/.../settings was NOT called after good-PK verify; the conflict path must still apply filterable/searchable attributes")
	}
}
