package undosend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// HeaderOptIn is the request header a JMAP client sets to opt in
// to the undo-send hold path. We deliberately make this opt-in
// (header) rather than universal so:
//
//   - Programmatic submitters (sync agents, calendar-bridge
//     internal callers, retention export, etc.) keep their
//     existing "submit means submit" semantics.
//   - The interceptor's failure modes are bounded: a header that
//     was never sent can never hit the hold path.
const HeaderOptIn = "X-KMail-Undo-Send"

// Response headers stamped on the synthesised JMAP response.
const (
	HeaderPendingID = "X-KMail-Pending-Send-Id"
	HeaderDeadline  = "X-KMail-Undo-Deadline"
)

// HookConfig configures the proxy hook.
//
// `Service` is the Valkey-backed hold queue. `Forwarder` is the
// inner client used to forward the *stripped* JMAP request (with
// `EmailSubmission/set` removed) to Stalwart so the draft is
// still minted. In production this is the proxy's own internal
// client; tests inject a fake.
type HookConfig struct {
	Service   *Service
	Forwarder InternalSubmitter
	// AccountResolver returns the Stalwart account ID for a
	// `(tenantID, kchatUserID)` pair. The proxy already holds
	// this resolver behind `Proxy.ResolveAccountID`; we accept
	// the surface as a function so the hook isn't bound to the
	// concrete proxy type (and tests can substitute a fixed id).
	AccountResolver func(ctx context.Context, tenantID, kchatUserID string) (string, error)
}

// Hook is the `jmap.SendInterceptor` implementation.
type Hook struct {
	cfg HookConfig
}

// NewHook validates HookConfig and returns a *Hook ready to be
// wired into `ProxyConfig.SendInterceptor`.
func NewHook(cfg HookConfig) (*Hook, error) {
	if cfg.Service == nil {
		return nil, errors.New("undosend.NewHook: Service is required")
	}
	if cfg.Forwarder == nil {
		return nil, errors.New("undosend.NewHook: Forwarder is required")
	}
	if cfg.AccountResolver == nil {
		return nil, errors.New("undosend.NewHook: AccountResolver is required")
	}
	return &Hook{cfg: cfg}, nil
}

// Intercept implements jmap.SendInterceptor.
//
// Returns `intercepted=true` iff the hook fully handled the
// response. `intercepted=false` means "not for me, forward
// normally" — and the proxy's existing path runs.
func (h *Hook) Intercept(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte) (bool, error) {
	if r.Header.Get(HeaderOptIn) != "true" {
		return false, nil
	}
	jr, err := parseJMAPRequest(body)
	if err != nil {
		// A malformed body would have been rejected by Stalwart
		// anyway; let the upstream produce the canonical error.
		return false, nil
	}
	intent, ok := extractUndoIntent(jr)
	if !ok {
		// No EmailSubmission/set create in this batch — forward.
		return false, nil
	}

	tenantID := middleware.TenantIDFrom(ctx)
	kchatUserID := middleware.KChatUserIDFrom(ctx)
	if tenantID == "" || kchatUserID == "" {
		// Auth middleware should have populated these; if it
		// didn't, the proxy's own ServeHTTP already 500'd before
		// we got here — defensive fall-through to upstream.
		return false, nil
	}
	accountID, err := h.cfg.AccountResolver(ctx, tenantID, kchatUserID)
	if err != nil {
		return false, fmt.Errorf("undosend.Hook: resolve account: %w", err)
	}

	// Forward the *stripped* JMAP request to Stalwart so the
	// draft Email/set portion still mints the underlying message.
	strippedReq := intent.stripped
	stalwartResp, err := h.cfg.Forwarder.Dispatch(ctx, tenantID, kchatUserID, strippedReq)
	if err != nil {
		return false, fmt.Errorf("undosend.Hook: forward stripped: %w", err)
	}
	if methodErr := stalwartResp.FirstCallError(); methodErr != nil {
		// Stalwart rejected the Email/set portion. We don't
		// silently swallow this — propagate the original error
		// shape back to the client by writing the Stalwart
		// response verbatim.
		return writeJMAPResponse(w, stalwartResp, http.StatusOK)
	}

	// Resolve the back-reference: the original EmailSubmission/set
	// referenced `#<emailCreateKey>`; the worker needs the real
	// Stalwart id. Pull it out of the response createdIds map (RFC
	// 8620 §3.4).
	emailID, ok := resolveCreatedEmailID(stalwartResp, intent.emailCreateKey)
	if !ok {
		// The Email/set itself succeeded but the response shape is
		// unexpected — surface what we did get rather than guessing.
		return writeJMAPResponse(w, stalwartResp, http.StatusOK)
	}

	// Normalise the submission payload to the real emailId before
	// persisting; the worker now doesn't need to reason about
	// back-references.
	submissionPayload, err := normaliseSubmissionPayload(intent.submissionCreate, intent.emailCreateKey, emailID)
	if err != nil {
		return false, fmt.Errorf("undosend.Hook: normalise payload: %w", err)
	}

	ps, err := h.cfg.Service.Hold(ctx, HoldInput{
		TenantID:          tenantID,
		KChatUserID:       kchatUserID,
		StalwartAccountID: accountID,
		CreateID:          intent.submissionCreateKey,
		EmailID:           emailID,
		IdentityID:        intent.identityID,
		SubmissionPayload: submissionPayload,
	})
	if err != nil {
		return false, fmt.Errorf("undosend.Hook: hold: %w", err)
	}

	// Synthesise the EmailSubmission/set response so the JMAP
	// client believes the submission succeeded. The real
	// EmailSubmission entity gets minted by the worker after the
	// deadline; we expose the pending-send id via custom headers.
	merged := mergeWithSyntheticSubmission(stalwartResp, intent, ps.ID)
	w.Header().Set(HeaderPendingID, ps.ID)
	w.Header().Set(HeaderDeadline, fmt.Sprintf("%d", ps.DeadlineAt.Unix()))
	return writeJMAPResponse(w, merged, http.StatusOK)
}

// undoIntent captures everything the hook learns from a single
// JMAP request that opts in to undo-send.
type undoIntent struct {
	stripped              jmap.JmapRequest
	submissionMethodID    string
	submissionCreateKey   string
	emailCreateKey        string
	identityID            string
	submissionCreate      map[string]any
}

// extractUndoIntent walks the request, finds the first
// `EmailSubmission/set` with a non-empty `create` map, captures
// the createKey/identityId/back-reference, and returns a stripped
// JmapRequest with the EmailSubmission/set call removed.
//
// Returns ok=false when the request doesn't carry a
// `EmailSubmission/set` create — those requests are
// undo-send-irrelevant and must be forwarded verbatim.
func extractUndoIntent(req jmap.JmapRequest) (undoIntent, bool) {
	var intent undoIntent
	intent.stripped = jmap.JmapRequest{
		Using:      append([]string(nil), req.Using...),
		CreatedIds: copyStringMap(req.CreatedIds),
	}
	intent.stripped.MethodCalls = make([][]any, 0, len(req.MethodCalls))
	found := false
	for _, call := range req.MethodCalls {
		if len(call) != 3 {
			intent.stripped.MethodCalls = append(intent.stripped.MethodCalls, call)
			continue
		}
		name, _ := call[0].(string)
		args, _ := call[1].(map[string]any)
		id, _ := call[2].(string)
		if name != "EmailSubmission/set" || found {
			intent.stripped.MethodCalls = append(intent.stripped.MethodCalls, call)
			continue
		}
		create, _ := args["create"].(map[string]any)
		if len(create) == 0 {
			intent.stripped.MethodCalls = append(intent.stripped.MethodCalls, call)
			continue
		}
		// We only intercept the *first* create entry. JMAP allows
		// multiple per call but the React client always sends one.
		// If a future client batches, the extras fall through to
		// Stalwart on the next request.
		var subKey string
		var subVal map[string]any
		for k, v := range create {
			subKey = k
			subVal, _ = v.(map[string]any)
			break
		}
		if subVal == nil {
			intent.stripped.MethodCalls = append(intent.stripped.MethodCalls, call)
			continue
		}
		intent.submissionMethodID = id
		intent.submissionCreateKey = subKey
		intent.submissionCreate = subVal
		// Extract the back-reference to the Email/set create key.
		// JMAP shorthand: `"emailId": "#draft"` references the
		// `draft` key in a preceding Email/set call. The
		// extraction is best-effort; if the field is already a
		// concrete id we capture it directly.
		if ref, _ := subVal["emailId"].(string); strings.HasPrefix(ref, "#") {
			intent.emailCreateKey = strings.TrimPrefix(ref, "#")
		}
		if identity, _ := subVal["identityId"].(string); identity != "" {
			intent.identityID = identity
		}
		found = true
		// We DROP the EmailSubmission/set call from the stripped
		// request — Stalwart only mints the draft.
	}
	if !found {
		return undoIntent{}, false
	}
	return intent, true
}

// resolveCreatedEmailID extracts the real Stalwart id for the
// draft Email that was created in the stripped request's
// preceding Email/set call. JMAP places this in the method-call
// response args under `created[<key>].id`.
func resolveCreatedEmailID(resp *jmap.JmapResponse, emailCreateKey string) (string, bool) {
	if strings.TrimSpace(emailCreateKey) == "" {
		return "", false
	}
	for _, entry := range resp.MethodResponses {
		if len(entry) != 3 {
			continue
		}
		name, _ := entry[0].(string)
		if name != "Email/set" {
			continue
		}
		args, _ := entry[1].(map[string]any)
		created, _ := args["created"].(map[string]any)
		raw, _ := created[emailCreateKey].(map[string]any)
		id, ok := raw["id"].(string)
		if ok && id != "" {
			return id, true
		}
	}
	return "", false
}

// normaliseSubmissionPayload returns a JSON-encoded copy of
// `submissionCreate` with any back-reference replaced by the real
// Stalwart emailId.
func normaliseSubmissionPayload(submissionCreate map[string]any, emailCreateKey, emailID string) ([]byte, error) {
	clone := make(map[string]any, len(submissionCreate))
	for k, v := range submissionCreate {
		clone[k] = v
	}
	// Resolve the back-reference, if any.
	if ref, _ := clone["emailId"].(string); strings.HasPrefix(ref, "#") && strings.TrimPrefix(ref, "#") == emailCreateKey {
		clone["emailId"] = emailID
	} else if _, ok := clone["emailId"].(string); !ok {
		clone["emailId"] = emailID
	}
	return json.Marshal(clone)
}

// mergeWithSyntheticSubmission rebuilds a JmapResponse that has
// the original Email/set response untouched but inserts a fully
// synthetic EmailSubmission/set response so the JMAP client gets
// the shape it expects.
func mergeWithSyntheticSubmission(stalwartResp *jmap.JmapResponse, intent undoIntent, pendingSendID string) *jmap.JmapResponse {
	merged := &jmap.JmapResponse{
		SessionState:    stalwartResp.SessionState,
		CreatedIds:      copyStringMap(stalwartResp.CreatedIds),
		MethodResponses: make([][]any, 0, len(stalwartResp.MethodResponses)+1),
	}
	merged.MethodResponses = append(merged.MethodResponses, stalwartResp.MethodResponses...)
	syntheticArgs := map[string]any{}
	// Populate accountId from the Email/set response args.
	for _, entry := range stalwartResp.MethodResponses {
		if len(entry) != 3 {
			continue
		}
		name, _ := entry[0].(string)
		if name != "Email/set" {
			continue
		}
		args, _ := entry[1].(map[string]any)
		if acct, ok := args["accountId"].(string); ok {
			syntheticArgs["accountId"] = acct
		}
		break
	}
	syntheticArgs["created"] = map[string]any{
		intent.submissionCreateKey: map[string]any{
			"id":          pendingSendID, // proxy-issued; the real id appears after dispatch
			"sendAt":      nil,
			"undoStatus":  "pending",
		},
	}
	// Echo the JMAP method-call id so the client's matcher works.
	id := intent.submissionMethodID
	if id == "" {
		id = "1"
	}
	merged.MethodResponses = append(merged.MethodResponses, []any{
		"EmailSubmission/set",
		syntheticArgs,
		id,
	})
	return merged
}

func parseJMAPRequest(body []byte) (jmap.JmapRequest, error) {
	var jr jmap.JmapRequest
	if err := json.Unmarshal(body, &jr); err != nil {
		return jmap.JmapRequest{}, err
	}
	return jr, nil
}

func writeJMAPResponse(w http.ResponseWriter, resp *jmap.JmapResponse, status int) (bool, error) {
	buf, err := json.Marshal(resp)
	if err != nil {
		return false, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := io.Copy(w, bytes.NewReader(buf)); err != nil {
		return true, err
	}
	return true, nil
}

func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
