package jmap

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestSplitHeadersBody(t *testing.T) {
	t.Parallel()
	h, b := splitHeadersBody([]byte("Subject: x\r\nFrom: a\r\n\r\nhello world"))
	if string(h) != "Subject: x\r\nFrom: a" || string(b) != "hello world" {
		t.Fatalf("CRLF split = (%q,%q)", h, b)
	}
	h, b = splitHeadersBody([]byte("Subject: x\n\nbody"))
	if string(h) != "Subject: x" || string(b) != "body" {
		t.Fatalf("LF split = (%q,%q)", h, b)
	}
	h, b = splitHeadersBody([]byte("no blank line"))
	if string(h) != "no blank line" || b != nil {
		t.Fatalf("no-blank split = (%q,%q)", h, b)
	}
}

func TestAttachmentBlobIDs(t *testing.T) {
	t.Parallel()
	bs := map[string]any{
		"type": "multipart/mixed",
		"subParts": []any{
			map[string]any{"partId": "1", "type": "text/plain"},
			map[string]any{"partId": "2", "type": "application/pdf", "disposition": "attachment", "blobId": "ba1"},
			map[string]any{
				"type": "multipart/related",
				"subParts": []any{
					map[string]any{"partId": "3", "disposition": "Attachment", "blobId": "ba2"},
					map[string]any{"partId": "4", "disposition": "inline", "blobId": "bi1"},
				},
			},
		},
	}
	got := attachmentBlobIDs(bs)
	if want := []string{"ba1", "ba2"}; !equalStrings(got, want) {
		t.Fatalf("attachmentBlobIDs = %v, want %v", got, want)
	}
}

func TestStalwartEmailExporter_FetchFullMessages(t *testing.T) {
	t.Parallel()

	blobs := map[string]string{
		"b1":  "Subject: One\r\nFrom: a@x.com\r\n\r\nbody1",
		"b2":  "Subject: Two\r\n\r\nbody2",
		"ba1": "ATTACHMENT-BYTES",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/jmap/download/") {
			// /jmap/download/{accountId}/{blobId}/{name}
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jmap/download/"), "/")
			blobID := parts[1]
			content, ok := blobs[blobID]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(content))
			return
		}
		// Email/get
		e1 := map[string]any{
			"id":         "e1",
			"blobId":     "b1",
			"subject":    "One",
			"receivedAt": "2026-01-02T03:04:05Z",
			"from":       []any{map[string]any{"name": "A", "email": "a@x.com"}},
			"bodyStructure": map[string]any{
				"type": "multipart/mixed",
				"subParts": []any{
					map[string]any{"partId": "1", "type": "text/plain"},
					map[string]any{"partId": "2", "blobId": "ba1", "disposition": "attachment", "name": "a.pdf"},
				},
			},
		}
		e2 := map[string]any{
			"id":            "e2",
			"blobId":        "b2",
			"subject":       "Two",
			"from":          []any{},
			"bodyStructure": map[string]any{"type": "text/plain", "partId": "1"},
		}
		resp := map[string]any{
			"methodResponses": []any{
				[]any{"Email/get", map[string]any{"list": []any{e1, e2}}, "g0"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	accts := []tenantAccount{{"u1", "acc-1"}}
	c := newOperatorTestProxy(t, srv, accts)
	ex, err := NewStalwartEmailExporter(c, c.proxy.cfg.Pool, c.proxy.Logger())
	if err != nil {
		t.Fatalf("NewStalwartEmailExporter: %v", err)
	}
	ex.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return accts, nil }

	// e3 is requested but not returned by Email/get → skipped.
	got, err := ex.FetchFullMessages(context.Background(), "t1",
		[]string{"acc-1:e1", "acc-1:e2", "acc-1:e3"})
	if err != nil {
		t.Fatalf("FetchFullMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (e3 skipped)", len(got))
	}

	m1 := got[0]
	if m1.ID != "acc-1:e1" || m1.BlobID != "b1" || m1.From != "a@x.com" || m1.Subject != "One" {
		t.Errorf("m1 metadata = %+v", m1)
	}
	if m1.ReceivedAt.IsZero() {
		t.Errorf("m1.ReceivedAt not parsed")
	}
	if string(m1.Raw) != blobs["b1"] {
		t.Errorf("m1.Raw = %q", m1.Raw)
	}
	if string(m1.Headers) != "Subject: One\r\nFrom: a@x.com" || string(m1.Body) != "body1" {
		t.Errorf("m1 split = (%q,%q)", m1.Headers, m1.Body)
	}
	if len(m1.Attachments) != 1 || string(m1.Attachments[0]) != "ATTACHMENT-BYTES" {
		t.Errorf("m1.Attachments = %v", m1.Attachments)
	}

	m2 := got[1]
	if m2.ID != "acc-1:e2" || len(m2.Attachments) != 0 || string(m2.Body) != "body2" {
		t.Errorf("m2 = %+v", m2)
	}
}

func TestStalwartEmailExporter_FetchFullMessages_BatchesPerAccount(t *testing.T) {
	t.Parallel()

	// More ids for one account than emailGetBatchSize must be split
	// across multiple Email/get calls so we never trip the server's
	// maxObjectsInGet limit.
	const total = emailGetBatchSize + 5

	var getCalls atomic.Int32
	var maxBatch atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/jmap/download/") {
			_, _ = w.Write([]byte("Subject: x\r\n\r\nbody"))
			return
		}
		getCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		calls, _ := req["methodCalls"].([]any)
		call, _ := calls[0].([]any)
		args, _ := call[1].(map[string]any)
		ids, _ := args["ids"].([]any)
		for {
			cur := maxBatch.Load()
			if int64(len(ids)) <= cur || maxBatch.CompareAndSwap(cur, int64(len(ids))) {
				break
			}
		}
		list := make([]any, 0, len(ids))
		for _, idv := range ids {
			id, _ := idv.(string)
			list = append(list, map[string]any{
				"id":            id,
				"blobId":        "blob-" + id,
				"bodyStructure": map[string]any{"type": "text/plain", "partId": "1"},
			})
		}
		resp := map[string]any{
			"methodResponses": []any{
				[]any{"Email/get", map[string]any{"list": list}, "g0"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	accts := []tenantAccount{{"u1", "acc-1"}}
	c := newOperatorTestProxy(t, srv, accts)
	ex, _ := NewStalwartEmailExporter(c, c.proxy.cfg.Pool, c.proxy.Logger())
	ex.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return accts, nil }

	ids := make([]string, total)
	for i := range ids {
		ids[i] = QualifyEmailID("acc-1", fmt.Sprintf("e%04d", i))
	}

	got, err := ex.FetchFullMessages(context.Background(), "t1", ids)
	if err != nil {
		t.Fatalf("FetchFullMessages: %v", err)
	}
	if len(got) != total {
		t.Fatalf("len = %d, want %d", len(got), total)
	}
	if want := int32(2); getCalls.Load() != want {
		t.Errorf("Email/get calls = %d, want %d (batched)", getCalls.Load(), want)
	}
	if maxBatch.Load() > emailGetBatchSize {
		t.Errorf("a batch had %d ids, exceeds emailGetBatchSize=%d", maxBatch.Load(), emailGetBatchSize)
	}
}

func TestStalwartEmailExporter_FetchFullMessages_DedupsInput(t *testing.T) {
	t.Parallel()

	// Duplicate input IDs must be coalesced: fetched once (the
	// server should never see a repeated id in the Email/get set)
	// and emitted once, in first-seen order.
	var sawIDs atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/jmap/download/") {
			_, _ = w.Write([]byte("Subject: x\r\n\r\nbody"))
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		calls, _ := req["methodCalls"].([]any)
		call, _ := calls[0].([]any)
		args, _ := call[1].(map[string]any)
		ids, _ := args["ids"].([]any)
		sawIDs.Add(int64(len(ids)))
		list := make([]any, 0, len(ids))
		for _, idv := range ids {
			id, _ := idv.(string)
			list = append(list, map[string]any{
				"id":            id,
				"blobId":        "blob-" + id,
				"bodyStructure": map[string]any{"type": "text/plain", "partId": "1"},
			})
		}
		resp := map[string]any{
			"methodResponses": []any{
				[]any{"Email/get", map[string]any{"list": list}, "g0"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	accts := []tenantAccount{{"u1", "acc-1"}}
	c := newOperatorTestProxy(t, srv, accts)
	ex, _ := NewStalwartEmailExporter(c, c.proxy.cfg.Pool, c.proxy.Logger())
	ex.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return accts, nil }

	// e1 appears three times, e2 twice — two distinct ids.
	got, err := ex.FetchFullMessages(context.Background(), "t1",
		[]string{"acc-1:e1", "acc-1:e2", "acc-1:e1", "acc-1:e2", "acc-1:e1"})
	if err != nil {
		t.Fatalf("FetchFullMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 distinct", len(got))
	}
	if got[0].ID != "acc-1:e1" || got[1].ID != "acc-1:e2" {
		t.Errorf("order = [%s, %s], want [acc-1:e1, acc-1:e2]", got[0].ID, got[1].ID)
	}
	if n := sawIDs.Load(); n != 2 {
		t.Errorf("server saw %d ids, want 2 (deduped before Email/get)", n)
	}
}

func TestStalwartEmailExporter_FetchFullMessages_MalformedID(t *testing.T) {
	t.Parallel()
	ex := &StalwartEmailExporter{}
	ex.accountsFn = func(_ context.Context, _ string) ([]tenantAccount, error) { return nil, nil }
	if _, err := ex.FetchFullMessages(context.Background(), "t1", []string{"noColon"}); err == nil {
		t.Fatal("expected error for malformed qualified id")
	}
}
