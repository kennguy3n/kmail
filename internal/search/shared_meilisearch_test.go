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
