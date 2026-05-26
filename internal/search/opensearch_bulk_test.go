package search

import (
	"errors"
	"strings"
	"testing"
)

// TestParseBulkResponse_AllSuccess pins the no-op path: a clean
// bulk response with `errors:false` must return nil even when
// the items array carries successful entries.
func TestParseBulkResponse_AllSuccess(t *testing.T) {
	body := []byte(`{
		"took": 12,
		"errors": false,
		"items": [
			{"index": {"_id": "a:1", "status": 201}},
			{"index": {"_id": "a:2", "status": 201}}
		]
	}`)
	if err := parseBulkResponse(body); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

// TestParseBulkResponse_PartialFailure is the regression guard:
// the previous code returned nil on HTTP 200 even when the body
// reported `errors:true`. The fix MUST surface a wrapped
// `errBulkPartialFailure` with item-level context.
func TestParseBulkResponse_PartialFailure(t *testing.T) {
	body := []byte(`{
		"took": 9,
		"errors": true,
		"items": [
			{"index": {"_id": "a:1", "status": 201}},
			{"index": {"_id": "a:2", "status": 400, "error": {"type": "mapper_parsing_exception", "reason": "field [tenant_id] is text"}}}
		]
	}`)
	err := parseBulkResponse(body)
	if err == nil {
		t.Fatalf("expected partial-failure error, got nil")
	}
	if !errors.Is(err, errBulkPartialFailure) {
		t.Fatalf("expected errBulkPartialFailure, got %v", err)
	}
	for _, want := range []string{`a:2`, `mapper_parsing_exception`, `1 of 2 items failed`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("expected error to contain %q, got %q", want, err.Error())
		}
	}
	// The helper MUST NOT prefix with "opensearch bulk:" itself —
	// each backend wraps with its own backend-specific prefix so
	// the shared backend's log line is not "opensearch shared
	// bulk: opensearch bulk: ..." (a real Devin Review flag).
	if strings.Contains(err.Error(), "opensearch bulk:") {
		t.Errorf("parseBulkResponse must be backend-neutral; got %q which would double-prefix when wrapped", err.Error())
	}
	if strings.Contains(err.Error(), "opensearch shared bulk:") {
		t.Errorf("parseBulkResponse must not embed backend names; got %q", err.Error())
	}
}

// TestParseBulkResponse_AllFailures keeps the failure count honest
// when EVERY item is rejected (a systemic mapping break, say).
// The error must still carry the first item's details, not blow
// up trying to enumerate all of them.
func TestParseBulkResponse_AllFailures(t *testing.T) {
	body := []byte(`{
		"took": 5,
		"errors": true,
		"items": [
			{"index": {"_id": "a:1", "status": 400, "error": {"type": "version_conflict_engine_exception", "reason": "version conflict"}}},
			{"index": {"_id": "a:2", "status": 400, "error": {"type": "version_conflict_engine_exception", "reason": "version conflict"}}},
			{"index": {"_id": "a:3", "status": 400, "error": {"type": "version_conflict_engine_exception", "reason": "version conflict"}}}
		]
	}`)
	err := parseBulkResponse(body)
	if err == nil || !errors.Is(err, errBulkPartialFailure) {
		t.Fatalf("expected errBulkPartialFailure, got %v", err)
	}
	if !strings.Contains(err.Error(), "3 of 3 items failed") {
		t.Errorf("expected '3 of 3 items failed', got %q", err.Error())
	}
}

// TestParseBulkResponse_EmptyBody covers the misconfigured-upstream
// case. The previous code silently returned nil; the fix MUST
// flag the empty body as an error so an operator notices the
// upstream is broken instead of letting cutover proceed against
// a backend that's not actually accepting writes.
func TestParseBulkResponse_EmptyBody(t *testing.T) {
	if err := parseBulkResponse(nil); err == nil {
		t.Fatalf("expected error for nil body, got nil")
	}
	if err := parseBulkResponse([]byte("")); err == nil {
		t.Fatalf("expected error for empty body, got nil")
	}
}

// TestParseBulkResponse_MalformedBody is the safety-net path:
// a partially-decoded body shouldn't silently look like a clean
// success.
func TestParseBulkResponse_MalformedBody(t *testing.T) {
	body := []byte(`{"took": 12, "errors":`)
	err := parseBulkResponse(body)
	if err == nil {
		t.Fatalf("expected decode error, got nil")
	}
	if !strings.Contains(err.Error(), "decode response") {
		t.Errorf("expected 'decode response' in error, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "opensearch bulk:") {
		t.Errorf("malformed-body error must be backend-neutral; got %q", err.Error())
	}
}

// TestParseBulkResponse_ErrorsTrueButNoItems handles the
// inconsistency the bot called out indirectly: a 200 with
// `errors:true` but no per-item error block. Treat it as a
// failure (consistent with errs > 0) rather than silently
// returning nil.
func TestParseBulkResponse_ErrorsTrueButNoItems(t *testing.T) {
	body := []byte(`{"took": 12, "errors": true, "items": []}`)
	err := parseBulkResponse(body)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, errBulkPartialFailure) {
		t.Fatalf("expected errBulkPartialFailure, got %v", err)
	}
}
