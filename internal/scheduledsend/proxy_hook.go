package scheduledsend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// HeaderScheduleAt opts in to the Scheduled Send hold path. The
// value is the desired dispatch time, encoded as either RFC3339
// (`2026-06-12T09:00:00Z`) or unix-seconds (`1750000000`). Unix
// seconds keep the wire payload smaller for the React client
// while RFC3339 is more debuggable for cURL / Postman callers.
const HeaderScheduleAt = "X-KMail-Schedule-At"

// Response headers stamped on the synthesised JMAP response so
// the React client can build the "scheduled for X" toast.
const (
	HeaderScheduledID   = "X-KMail-Scheduled-Send-Id"
	HeaderScheduledSendAt = "X-KMail-Scheduled-Send-At"
)

// HookConfig configures the proxy hook.
//
// `Service` persists the held submission to Postgres.
// `Forwarder` is the inner JMAP client used to forward the
// stripped request (`EmailSubmission/set` removed, `Email/set`
// retained) so Stalwart still mints the draft. In production
// this is the proxy's own internal client; tests inject a fake.
type HookConfig struct {
	Service         *Service
	Forwarder       InternalSubmitter
	AccountResolver func(ctx context.Context, tenantID, kchatUserID string) (string, error)
}

// scheduler is the slice of Service the hook depends on. Tests
// inject an in-memory fake so the hook can be exercised without
// a real Postgres pool.
type scheduler interface {
	Schedule(ctx context.Context, in ScheduleInput) (*ScheduledSend, error)
}

// Hook implements `jmap.SendInterceptor`.
type Hook struct {
	scheduler       scheduler
	forwarder       InternalSubmitter
	accountResolver func(ctx context.Context, tenantID, kchatUserID string) (string, error)
}

// NewHook validates HookConfig and returns a *Hook ready to be
// passed to `Proxy.SetSendInterceptor`. Returning an error rather
// than panicking is important here because main.go composes both
// `undosend.Hook` and `scheduledsend.Hook` and chains them; a
// nil-pointer surprise would only surface on a real send.
func NewHook(cfg HookConfig) (*Hook, error) {
	if cfg.Service == nil {
		return nil, errors.New("scheduledsend.NewHook: Service is required")
	}
	return newHookWithScheduler(cfg.Service, cfg.Forwarder, cfg.AccountResolver)
}

// newHookWithScheduler is the test seam: lets tests substitute a
// fake scheduler for the real `*Service`.
func newHookWithScheduler(s scheduler, forwarder InternalSubmitter, resolver func(ctx context.Context, tenantID, kchatUserID string) (string, error)) (*Hook, error) {
	if s == nil {
		return nil, errors.New("scheduledsend.NewHook: scheduler is required")
	}
	if forwarder == nil {
		return nil, errors.New("scheduledsend.NewHook: Forwarder is required")
	}
	if resolver == nil {
		return nil, errors.New("scheduledsend.NewHook: AccountResolver is required")
	}
	return &Hook{scheduler: s, forwarder: forwarder, accountResolver: resolver}, nil
}

// Intercept implements `jmap.SendInterceptor`.
//
// Returns intercepted=true iff the hook fully handled the
// response (i.e. wrote the wire body itself). Returns
// intercepted=false to mean "not for me; let the proxy continue
// the normal forward path".
func (h *Hook) Intercept(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte) (bool, error) {
	scheduleAtRaw := strings.TrimSpace(r.Header.Get(HeaderScheduleAt))
	if scheduleAtRaw == "" {
		return false, nil
	}
	sendAt, err := parseScheduleAt(scheduleAtRaw)
	if err != nil {
		// A malformed header is a client bug; respond with 400
		// instead of silently dispatching now (which would be
		// surprising — the user clicked "Send tomorrow" and got
		// "Sent immediately").
		http.Error(w, fmt.Sprintf("scheduledsend: malformed %s header: %v", HeaderScheduleAt, err), http.StatusBadRequest)
		return true, nil
	}

	jr, err := parseJMAPRequest(body)
	if err != nil {
		// Malformed body — let Stalwart produce the canonical
		// error rather than guessing.
		return false, nil
	}
	intent, ok := extractScheduleIntent(jr)
	if !ok {
		// Header was set but the batch doesn't carry an
		// EmailSubmission/set create. Forward verbatim — this
		// can happen if a probing client sends the header on a
		// non-submission JMAP call (e.g. Mailbox/get).
		return false, nil
	}

	tenantID := middleware.TenantIDFrom(ctx)
	kchatUserID := middleware.KChatUserIDFrom(ctx)
	if tenantID == "" || kchatUserID == "" {
		return false, nil
	}
	accountID, err := h.accountResolver(ctx, tenantID, kchatUserID)
	if err != nil {
		return false, fmt.Errorf("scheduledsend.Hook: resolve account: %w", err)
	}

	stalwartResp, err := h.forwarder.Dispatch(ctx, tenantID, kchatUserID, intent.stripped)
	if err != nil {
		return false, fmt.Errorf("scheduledsend.Hook: forward stripped: %w", err)
	}
	if methodErr := stalwartResp.FirstCallError(); methodErr != nil {
		// Email/set failed upstream — write Stalwart's own
		// error response so the client sees the canonical
		// failure shape.
		return writeJMAPResponse(w, stalwartResp, http.StatusOK)
	}

	emailID, ok := resolveCreatedEmailID(stalwartResp, intent.emailCreateKey)
	if !ok {
		// Email/set succeeded but the response shape is
		// unexpected — surface what we got rather than guessing.
		return writeJMAPResponse(w, stalwartResp, http.StatusOK)
	}

	submissionPayload, err := normaliseSubmissionPayload(intent.submissionCreate, intent.emailCreateKey, emailID)
	if err != nil {
		return false, fmt.Errorf("scheduledsend.Hook: normalise payload: %w", err)
	}
	identityID := intent.identityID
	ss, err := h.scheduler.Schedule(ctx, ScheduleInput{
		TenantID:          tenantID,
		KChatUserID:       kchatUserID,
		StalwartAccountID: accountID,
		EmailID:           emailID,
		IdentityID:        identityID,
		SubmissionPayload: submissionPayload,
		SendAt:            sendAt,
	})
	if err != nil {
		if errors.Is(err, ErrInvalidSchedule) {
			// Client asked to schedule outside the allowed
			// horizon (too soon / too far). 400 is the only
			// safe answer.
			http.Error(w, fmt.Sprintf("scheduledsend: %v", err), http.StatusBadRequest)
			return true, nil
		}
		return false, fmt.Errorf("scheduledsend.Hook: schedule: %w", err)
	}

	merged := mergeWithSyntheticSubmission(stalwartResp, intent, ss.ID, ss.SendAt)
	w.Header().Set(HeaderScheduledID, ss.ID)
	w.Header().Set(HeaderScheduledSendAt, fmt.Sprintf("%d", ss.SendAt.Unix()))
	return writeJMAPResponse(w, merged, http.StatusOK)
}

// parseScheduleAt accepts either an RFC3339 timestamp or a
// unix-seconds integer. The dual format is intentional: the
// React client sends unix-seconds (smaller payload, no timezone
// ambiguity); cURL / Postman users find RFC3339 easier to write
// by hand.
func parseScheduleAt(raw string) (time.Time, error) {
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}, fmt.Errorf("non-positive unix seconds: %d", n)
		}
		return time.Unix(n, 0).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("not RFC3339 or unix-seconds: %w", err)
	}
	return t.UTC(), nil
}

// scheduleIntent captures everything the hook learns from a single
// JMAP request that opts in to scheduled send.
type scheduleIntent struct {
	stripped            jmap.JmapRequest
	submissionMethodID  string
	submissionCreateKey string
	emailCreateKey      string
	identityID          string
	submissionCreate    map[string]any
}

// extractScheduleIntent walks `req.MethodCalls`, finds the first
// `EmailSubmission/set` with a non-empty `create` map, captures
// the createKey / identityId / back-reference, and returns a
// stripped JmapRequest with that submission call removed.
func extractScheduleIntent(req jmap.JmapRequest) (scheduleIntent, bool) {
	var intent scheduleIntent
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
		if ref, _ := subVal["emailId"].(string); strings.HasPrefix(ref, "#") {
			intent.emailCreateKey = strings.TrimPrefix(ref, "#")
		}
		if identity, _ := subVal["identityId"].(string); identity != "" {
			intent.identityID = identity
		}
		found = true
	}
	if !found {
		return scheduleIntent{}, false
	}
	return intent, true
}

// resolveCreatedEmailID extracts the real Stalwart id for the
// draft Email that was created in the preceding Email/set call.
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
	if ref, _ := clone["emailId"].(string); strings.HasPrefix(ref, "#") && strings.TrimPrefix(ref, "#") == emailCreateKey {
		clone["emailId"] = emailID
	} else if _, ok := clone["emailId"].(string); !ok {
		clone["emailId"] = emailID
	}
	return json.Marshal(clone)
}

// mergeWithSyntheticSubmission keeps the Email/set response from
// Stalwart and appends a synthetic EmailSubmission/set response
// so the JMAP client sees the shape it expects. The synthetic
// `id` is the scheduled-send row id; the real EmailSubmission id
// appears only after the worker dispatches.
func mergeWithSyntheticSubmission(stalwartResp *jmap.JmapResponse, intent scheduleIntent, scheduledID string, sendAt time.Time) *jmap.JmapResponse {
	merged := &jmap.JmapResponse{
		SessionState:    stalwartResp.SessionState,
		CreatedIds:      copyStringMap(stalwartResp.CreatedIds),
		MethodResponses: make([][]any, 0, len(stalwartResp.MethodResponses)+1),
	}
	merged.MethodResponses = append(merged.MethodResponses, stalwartResp.MethodResponses...)
	syntheticArgs := map[string]any{}
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
			"id":         scheduledID,
			"sendAt":     sendAt.UTC().Format(time.RFC3339),
			"undoStatus": "pending",
		},
	}
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
