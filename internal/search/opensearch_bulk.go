// Package search — OpenSearch `_bulk` response handling.
//
// OpenSearch's `_bulk` endpoint is HTTP-only-2xx even for partial
// per-item failures: an HTTP 200 carries a JSON body whose
// top-level `errors: true` flag, and per-item `error` blocks,
// describe the actual outcome. Treating status >= 400 as the only
// failure signal means a bulk write where (e.g.) one of 1000
// documents was rejected by mapping or a `version_conflict`
// silently returns nil, and the cutover worker would proceed to
// `MarkCompleted` while a fraction of the tenant's messages are
// missing from the destination index.
//
// `parseBulkResponse` is the shared helper both
// `OpenSearchBackend.MigrateIndex` and
// `SharedOpenSearchBackend.MigrateIndex` use to decode the body
// and report per-item failure counts. Wire-level (resp.StatusCode
// >= 400) errors stay separate so the caller's error message
// preserves the existing "bulk: %d %s" shape — only the bot-flagged
// "200 with errors:true in body" path is added.
//
// Returned error messages are intentionally backend-neutral (no
// "opensearch bulk:" / "opensearch shared bulk:" prefix). Each
// caller wraps the result with its own backend-specific prefix so
// the final log line identifies which backend the failure came
// from without producing a double-prefixed message like
// "opensearch shared bulk: opensearch bulk: ...".
package search

import (
	"encoding/json"
	"errors"
	"fmt"
)

// bulkResponse mirrors the subset of an OpenSearch `_bulk`
// response that we inspect. Other fields (`took`, etc.) are
// preserved by the JSON decoder but not surfaced.
type bulkResponse struct {
	Errors bool `json:"errors"`
	Items  []map[string]bulkItem `json:"items"`
}

// bulkItem is the per-operation entry under each items[].action
// map. OpenSearch returns one wrapper per item keyed by the
// operation name (`index`, `create`, `update`, `delete`).
type bulkItem struct {
	ID     string         `json:"_id"`
	Status int            `json:"status"`
	Error  map[string]any `json:"error"`
}

// errBulkPartialFailure is the sentinel error returned by
// `parseBulkResponse` when the response body's top-level `errors`
// flag is true. Callers wrap it with their own context (which
// backend + endpoint) so the surfaced error pinpoints the failing
// _bulk call. The sentinel itself is backend-neutral on purpose:
// the per-tenant and shared callers each add their own
// "opensearch bulk:" / "opensearch shared bulk:" prefix.
var errBulkPartialFailure = errors.New("bulk: per-item failure")

// parseBulkResponse decodes the raw `_bulk` response body and
// returns a non-nil error iff at least one item failed. The
// returned error embeds the first failing item's `_id`, HTTP
// status, and error `type`/`reason` so an operator can act
// without having to dig through the raw response.
//
// If the body cannot be decoded as JSON, parseBulkResponse
// returns a wrapping error rather than nil: an undecodable bulk
// response is a stronger signal of a misconfigured upstream than
// a clean 200, and silently treating it as success would re-open
// the exact correctness gap Devin Review flagged.
func parseBulkResponse(body []byte) error {
	if len(body) == 0 {
		return fmt.Errorf("bulk: empty response body")
	}
	var resp bulkResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("bulk: decode response: %w", err)
	}
	if !resp.Errors {
		return nil
	}
	// Find the first failing item to provide actionable context.
	// We DO NOT iterate every item — a bulk of 1000 docs with a
	// systemic mapping error would otherwise produce a 50 KB
	// log line.
	failed := 0
	var firstFailureMsg string
	for _, item := range resp.Items {
		for _, entry := range item {
			if entry.Error == nil && entry.Status < 400 {
				continue
			}
			failed++
			if firstFailureMsg == "" {
				firstFailureMsg = formatBulkItemError(entry)
			}
		}
	}
	if failed == 0 {
		// errors:true with no per-item failure shouldn't happen
		// in practice, but if it does we still want to surface
		// the inconsistency rather than silently succeed.
		return fmt.Errorf("%w: response.errors=true but no per-item error found", errBulkPartialFailure)
	}
	return fmt.Errorf("%w: %d of %d items failed; first: %s",
		errBulkPartialFailure, failed, len(resp.Items), firstFailureMsg)
}

// formatBulkItemError flattens a single failing bulk item into a
// log-friendly one-liner. Falls back gracefully if OpenSearch
// returns an unexpected error shape.
func formatBulkItemError(item bulkItem) string {
	errType, _ := item.Error["type"].(string)
	errReason, _ := item.Error["reason"].(string)
	if errType == "" && errReason == "" {
		return fmt.Sprintf("id=%q status=%d", item.ID, item.Status)
	}
	return fmt.Sprintf("id=%q status=%d type=%s reason=%s",
		item.ID, item.Status, errType, errReason)
}
