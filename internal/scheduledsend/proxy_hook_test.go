package scheduledsend

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// fakeScheduler records every Schedule call and returns a row
// with a deterministic id so hook tests don't need a real DB.
type fakeScheduler struct {
	mu     sync.Mutex
	calls  int
	last   ScheduleInput
	id     string
	err    error
	sendAt time.Time
}

func (f *fakeScheduler) Schedule(_ context.Context, in ScheduleInput) (*ScheduledSend, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.last = in
	if f.err != nil {
		return nil, f.err
	}
	id := f.id
	if id == "" {
		id = uuid.NewString()
	}
	sendAt := f.sendAt
	if sendAt.IsZero() {
		sendAt = in.SendAt
	}
	return &ScheduledSend{
		ID:                id,
		TenantID:          in.TenantID,
		KChatUserID:       in.KChatUserID,
		StalwartAccountID: in.StalwartAccountID,
		EmailID:           in.EmailID,
		IdentityID:        in.IdentityID,
		SubmissionPayload: in.SubmissionPayload,
		SendAt:            sendAt,
		Status:            StatusPending,
	}, nil
}

type stubForwarder struct {
	called   int
	response *jmap.JmapResponse
	err      error
	lastReq  jmap.JmapRequest
}

func (s *stubForwarder) Dispatch(_ context.Context, _, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	s.called++
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.response, nil
}

func ctxWithIdentity(tenantID, kchatUserID string) context.Context {
	ctx := middleware.WithTenantID(context.Background(), tenantID)
	ctx = middleware.WithKChatUserID(ctx, kchatUserID)
	return ctx
}

func buildScheduleBody(emailCreateKey, subCreateKey, identityID string) []byte {
	body := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": []any{
			[]any{"Email/set", map[string]any{
				"accountId": "acct-a",
				"create": map[string]any{
					emailCreateKey: map[string]any{"subject": "hello"},
				},
			}, "0"},
			[]any{"EmailSubmission/set", map[string]any{
				"accountId": "acct-a",
				"create": map[string]any{
					subCreateKey: map[string]any{
						"emailId":    "#" + emailCreateKey,
						"identityId": identityID,
					},
				},
			}, "1"},
		},
	}
	buf, _ := json.Marshal(body)
	return buf
}

func emailSetCreatedResponse(emailCreateKey, realID string) *jmap.JmapResponse {
	return &jmap.JmapResponse{
		SessionState: "session-1",
		MethodResponses: [][]any{
			{"Email/set", map[string]any{
				"accountId": "acct-a",
				"created": map[string]any{
					emailCreateKey: map[string]any{"id": realID},
				},
			}, "0"},
		},
	}
}

func newJMAPRequest(t *testing.T, body []byte, ctx context.Context, scheduleAt string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/jmap", bytes.NewReader(body))
	if scheduleAt != "" {
		r.Header.Set(HeaderScheduleAt, scheduleAt)
	}
	return r.WithContext(ctx)
}

func newHookForTest(t *testing.T, sched scheduler, fwd InternalSubmitter) *Hook {
	t.Helper()
	h, err := newHookWithScheduler(sched, fwd, func(_ context.Context, _, _ string) (string, error) {
		return "acct-a", nil
	}, nil)
	if err != nil {
		t.Fatalf("newHookWithScheduler: %v", err)
	}
	return h
}

func TestIntercept_HappyPath(t *testing.T) {
	future := time.Now().Add(10 * time.Minute).Unix()
	sched := &fakeScheduler{id: "ss-1"}
	fwd := &stubForwarder{response: emailSetCreatedResponse("draft", "REAL-email-1")}
	hook := newHookForTest(t, sched, fwd)

	body := buildScheduleBody("draft", "submission", "ident-1")
	ctx := ctxWithIdentity("tenant-a", "kchat-a")
	r := newJMAPRequest(t, body, ctx, strconv.FormatInt(future, 10))
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err != nil {
		t.Fatalf("Intercept: %v", err)
	}
	if !intercepted {
		t.Fatalf("expected intercepted=true")
	}
	if got := w.Header().Get(HeaderScheduledID); got != "ss-1" {
		t.Fatalf("scheduled id header = %q, want ss-1", got)
	}
	if got := w.Header().Get(HeaderScheduledSendAt); got == "" {
		t.Fatalf("missing scheduled send-at header")
	}
	if sched.calls != 1 {
		t.Fatalf("expected 1 schedule call, got %d", sched.calls)
	}
	if sched.last.EmailID != "REAL-email-1" {
		t.Fatalf("EmailID = %q, want REAL-email-1", sched.last.EmailID)
	}
	if sched.last.IdentityID != "ident-1" {
		t.Fatalf("IdentityID = %q, want ident-1", sched.last.IdentityID)
	}

	// Forwarder should have received only the Email/set call.
	if len(fwd.lastReq.MethodCalls) != 1 {
		t.Fatalf("forwarder saw %d calls, want 1", len(fwd.lastReq.MethodCalls))
	}
	if name, _ := fwd.lastReq.MethodCalls[0][0].(string); name != "Email/set" {
		t.Fatalf("forwarder first call = %s, want Email/set", name)
	}

	// Response body should contain the merged EmailSubmission/set
	// synthetic response.
	respBody, err := io.ReadAll(w.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var jr jmap.JmapResponse
	if err := json.Unmarshal(respBody, &jr); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(jr.MethodResponses) != 2 {
		t.Fatalf("expected 2 method responses, got %d", len(jr.MethodResponses))
	}
	if name, _ := jr.MethodResponses[1][0].(string); name != "EmailSubmission/set" {
		t.Fatalf("second response = %q, want EmailSubmission/set", name)
	}
}

func TestIntercept_NoHeaderFallsThrough(t *testing.T) {
	sched := &fakeScheduler{}
	fwd := &stubForwarder{}
	hook := newHookForTest(t, sched, fwd)
	body := buildScheduleBody("draft", "submission", "ident-1")
	ctx := ctxWithIdentity("tenant-a", "kchat-a")
	r := newJMAPRequest(t, body, ctx, "")
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err != nil {
		t.Fatalf("Intercept: %v", err)
	}
	if intercepted {
		t.Fatalf("expected fall-through when header absent")
	}
	if sched.calls != 0 {
		t.Fatalf("Schedule should not have been called")
	}
}

func TestIntercept_MalformedHeaderReturns400(t *testing.T) {
	sched := &fakeScheduler{}
	fwd := &stubForwarder{}
	hook := newHookForTest(t, sched, fwd)
	body := buildScheduleBody("draft", "submission", "ident-1")
	ctx := ctxWithIdentity("tenant-a", "kchat-a")
	r := newJMAPRequest(t, body, ctx, "not-a-time")
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err != nil {
		t.Fatalf("Intercept: %v", err)
	}
	if !intercepted {
		t.Fatalf("expected hook to take ownership of the response on malformed header")
	}
	if w.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Result().StatusCode)
	}
}

func TestIntercept_NoEmailSubmissionFallsThrough(t *testing.T) {
	sched := &fakeScheduler{}
	fwd := &stubForwarder{}
	hook := newHookForTest(t, sched, fwd)
	body := []byte(`{"using":[],"methodCalls":[["Mailbox/get",{},"0"]]}`)
	ctx := ctxWithIdentity("tenant-a", "kchat-a")
	r := newJMAPRequest(t, body, ctx, strconv.FormatInt(time.Now().Add(10*time.Minute).Unix(), 10))
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err != nil {
		t.Fatalf("Intercept: %v", err)
	}
	if intercepted {
		t.Fatalf("expected fall-through when no EmailSubmission/set")
	}
}

func TestIntercept_RFC3339HeaderAccepted(t *testing.T) {
	sched := &fakeScheduler{id: "ss-rfc3339"}
	fwd := &stubForwarder{response: emailSetCreatedResponse("draft", "REAL-1")}
	hook := newHookForTest(t, sched, fwd)
	body := buildScheduleBody("draft", "submission", "ident-1")
	ctx := ctxWithIdentity("tenant-a", "kchat-a")
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	r := newJMAPRequest(t, body, ctx, future)
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err != nil {
		t.Fatalf("Intercept: %v", err)
	}
	if !intercepted {
		t.Fatalf("expected intercepted=true")
	}
	if sched.calls != 1 {
		t.Fatalf("expected 1 schedule call, got %d", sched.calls)
	}
}

func TestParseScheduleAt_Variants(t *testing.T) {
	t.Run("unix-seconds", func(t *testing.T) {
		raw := "1750000000"
		got, err := parseScheduleAt(raw)
		if err != nil {
			t.Fatalf("parseScheduleAt: %v", err)
		}
		if got.Unix() != 1_750_000_000 {
			t.Fatalf("unix = %d, want 1750000000", got.Unix())
		}
	})
	t.Run("rfc3339", func(t *testing.T) {
		raw := "2026-06-12T09:00:00Z"
		got, err := parseScheduleAt(raw)
		if err != nil {
			t.Fatalf("parseScheduleAt: %v", err)
		}
		if !got.Equal(time.Date(2026, 6, 12, 9, 0, 0, 0, time.UTC)) {
			t.Fatalf("got %v, want 2026-06-12T09:00:00Z", got)
		}
	})
	t.Run("garbage", func(t *testing.T) {
		_, err := parseScheduleAt("not-a-date")
		if err == nil {
			t.Fatalf("expected error")
		}
	})
	t.Run("negative-unix", func(t *testing.T) {
		_, err := parseScheduleAt("-1")
		if err == nil {
			t.Fatalf("expected error for negative unix seconds")
		}
	})
}

func TestNormaliseSubmissionPayload_ResolvesBackReference(t *testing.T) {
	src := map[string]any{
		"emailId":    "#draft",
		"identityId": "ident-1",
	}
	buf, err := normaliseSubmissionPayload(src, "draft", "real-id")
	if err != nil {
		t.Fatalf("normaliseSubmissionPayload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["emailId"] != "real-id" {
		t.Fatalf("emailId = %v, want real-id", out["emailId"])
	}
}

// TestIntercept_ScheduleFailureAfterDispatchReturnsInterceptedNotFalse
// pins the orphan-draft fix: once the stripped Email/set has been
// forwarded to Stalwart (draft minted), any subsequent error
// (Postgres scheduling failure, normalise failure, etc.) MUST
// return `(intercepted=true, ...)` so the proxy does NOT fall
// through to its ServeHTTP reverse-proxy path. Falling through
// would re-forward the FULL original body (Email/set +
// EmailSubmission/set) to Stalwart, creating a duplicate draft
// AND submitting it immediately — completely defeating the
// scheduled-send semantics.
func TestIntercept_ScheduleFailureAfterDispatchReturnsInterceptedNotFalse(t *testing.T) {
	future := time.Now().Add(10 * time.Minute).Unix()
	sched := &fakeScheduler{err: errSchedulerBoom}
	fwd := &stubForwarder{response: emailSetCreatedResponse("draft", "REAL-email-1")}
	hook := newHookForTest(t, sched, fwd)

	body := buildScheduleBody("draft", "submission", "ident-1")
	ctx := ctxWithIdentity("tenant-a", "kchat-a")
	r := newJMAPRequest(t, body, ctx, strconv.FormatInt(future, 10))
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err != nil {
		t.Fatalf("Intercept returned err=%v, want nil (orphan-draft fix surfaces the half-committed Stalwart response instead of bubbling)", err)
	}
	if !intercepted {
		t.Fatalf("intercepted=false on scheduler failure after Dispatch — proxy would fall through and create a duplicate draft + immediate submission. This is the exact regression the fix pins.")
	}
	// Forwarder must have been called exactly once (the stripped
	// Email/set). If the proxy fell through, the full body would
	// then reach Stalwart via the reverse-proxy path; the hook
	// test cannot observe that directly, but `intercepted=true`
	// is the contract the proxy honors.
	if fwd.called != 1 {
		t.Fatalf("forwarder called %d times, want 1 (stripped Email/set)", fwd.called)
	}
	// Response should be the half-committed Stalwart response —
	// the client sees the draft was saved but no
	// EmailSubmission/set response, signaling scheduling failed.
	respBody, err := io.ReadAll(w.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var jr jmap.JmapResponse
	if err := json.Unmarshal(respBody, &jr); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(jr.MethodResponses) != 1 {
		t.Fatalf("expected 1 method response (Email/set only — scheduling failed), got %d", len(jr.MethodResponses))
	}
	if name, _ := jr.MethodResponses[0][0].(string); name != "Email/set" {
		t.Fatalf("only response method = %q, want Email/set", name)
	}
}

var errSchedulerBoom = &scheduleBoomErr{}

type scheduleBoomErr struct{}

func (*scheduleBoomErr) Error() string { return "simulated scheduler failure" }

func TestResolveCreatedEmailID_HappyPath(t *testing.T) {
	resp := emailSetCreatedResponse("draft", "REAL-1")
	got, ok := resolveCreatedEmailID(resp, "draft")
	if !ok || got != "REAL-1" {
		t.Fatalf("got (%q, %v), want (REAL-1, true)", got, ok)
	}
}
