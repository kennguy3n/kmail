package snooze

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// fakeManager implements the handlers' manager interface.
type fakeManager struct {
	mu           sync.Mutex
	rows         []Snooze
	snoozeResult *Snooze
	snoozeErr    error
	getResult    *Snooze
	// getResultSecond, when non-nil, is returned on the SECOND
	// and subsequent Get calls. Models the wakeNow TOCTOU window
	// where the worker has moved the row to a terminal state
	// between the handler's first Get and its post-applyMove
	// re-read.
	getResultSecond *Snooze
	getCalls        int
	getErr          error
	listErr         error
	cancelErr       error
	created         []SnoozeInput
	// getArgs / cancelArgs capture the (tenant, kchat_user, id)
	// triple the handler passed in. The per-user authz tests
	// pin these so a refactor that drops kchatUserID can't slip
	// silently into Service-call shape.
	getArgs struct {
		tenantID    string
		kchatUserID string
		id          string
	}
	cancelArgs struct {
		tenantID    string
		kchatUserID string
		id          string
	}
	cancels []string
}

func (f *fakeManager) Snooze(_ context.Context, in SnoozeInput) (*Snooze, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, in)
	if f.snoozeErr != nil {
		return nil, f.snoozeErr
	}
	if f.snoozeResult != nil {
		return f.snoozeResult, nil
	}
	now := time.Now().UTC()
	return &Snooze{
		ID:                 "s-1",
		TenantID:           in.TenantID,
		KChatUserID:        in.KChatUserID,
		StalwartAccountID:  in.StalwartAccountID,
		EmailID:            in.EmailID,
		OriginalMailboxIDs: in.OriginalMailboxIDs,
		SnoozedMailboxID:   in.SnoozedMailboxID,
		SnoozeUntil:        in.SnoozeUntil,
		MarkUnreadOnWake:   in.MarkUnreadOnWake,
		Status:             StatusSnoozed,
		CreatedAt:          now,
		UpdatedAt:          now,
		NextRetryAt:        in.SnoozeUntil,
	}, nil
}

func (f *fakeManager) Get(_ context.Context, tenantID, kchatUserID, id string) (*Snooze, error) {
	f.mu.Lock()
	f.getArgs.tenantID = tenantID
	f.getArgs.kchatUserID = kchatUserID
	f.getArgs.id = id
	f.getCalls++
	calls := f.getCalls
	f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if calls >= 2 && f.getResultSecond != nil {
		return f.getResultSecond, nil
	}
	return f.getResult, nil
}

func (f *fakeManager) ListByUser(_ context.Context, _, _ string) ([]Snooze, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeManager) Cancel(_ context.Context, tenantID, kchatUserID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelArgs.tenantID = tenantID
	f.cancelArgs.kchatUserID = kchatUserID
	f.cancelArgs.id = id
	f.cancels = append(f.cancels, id)
	return f.cancelErr
}

// fakeDispatcher implements the handlers' dispatcher interface.
type fakeDispatcher struct {
	mu       sync.Mutex
	calls    int
	requests []jmap.JmapRequest
	resp     *jmap.JmapResponse
	err      error
	resolveErr error
}

func (f *fakeDispatcher) ResolveAccountID(_ context.Context, _, _ string) (string, error) {
	if f.resolveErr != nil {
		return "", f.resolveErr
	}
	return "acct-a", nil
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	if f.resp != nil {
		return f.resp, nil
	}
	return okHandlerResp(), nil
}

func okHandlerResp() *jmap.JmapResponse {
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"Email/set", map[string]any{"accountId": "acct-a", "updated": map[string]any{"email-1": nil}}, "snz"},
		},
	}
}

func handlerRequest(method, target, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	ctx := middleware.WithTenantID(r.Context(), "tenant-a")
	ctx = middleware.WithKChatUserID(ctx, "kchat-a")
	return r.WithContext(ctx)
}

// testNow is the fixed clock the handler test seam injects via
// newHandlersWith. All test fixtures that produce a SnoozeUntil
// must anchor relative to testNow (not wall-clock time.Now())
// so the handler's horizon-check sees a value within
// [MinSnoozeHorizon, MaxSnoozeHorizon] of its injected clock.
var testNow = time.Unix(1_700_000_000, 0)

func newRouterForTest(m manager, d dispatcher) *http.ServeMux {
	mux := http.NewServeMux()
	h := newHandlersWith(m, d, func() time.Time { return testNow })
	mux.HandleFunc("POST /api/v1/snooze", h.create)
	mux.HandleFunc("GET /api/v1/snoozed", h.list)
	mux.HandleFunc("GET /api/v1/snoozed/{id}", h.get)
	mux.HandleFunc("DELETE /api/v1/snoozed/{id}", h.wakeNow)
	return mux
}

func makeCreateBody() string {
	until := testNow.Add(2 * time.Hour).UTC().Format(time.RFC3339)
	body := map[string]any{
		"email_id":             "email-1",
		"original_mailbox_ids": map[string]bool{"mb-inbox": true},
		"snoozed_mailbox_id":   "mb-snoozed",
		"snooze_until":         until,
	}
	b, _ := json.Marshal(body)
	return string(b)
}

func TestCreate_HappyPathMovesAndPersists(t *testing.T) {
	fm := &fakeManager{}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)
	w := httptest.NewRecorder()
	r := handlerRequest("POST", "/api/v1/snooze", makeCreateBody())
	router.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s, want 201", w.Code, w.Body.String())
	}
	if fd.calls != 1 {
		t.Fatalf("expected exactly 1 dispatch (move-to-snoozed), got %d", fd.calls)
	}
	if len(fm.created) != 1 {
		t.Fatalf("expected exactly 1 service.Snooze call, got %d", len(fm.created))
	}
	in := fm.created[0]
	if in.SnoozedMailboxID != "mb-snoozed" {
		t.Errorf("SnoozedMailboxID = %q, want mb-snoozed", in.SnoozedMailboxID)
	}
	if !in.MarkUnreadOnWake {
		t.Errorf("MarkUnreadOnWake should default true")
	}
	// Verify the patch shape on dispatch: snoozed mailbox added,
	// inbox dropped, $seen untouched.
	args, _ := fd.requests[0].MethodCalls[0][1].(map[string]any)
	update, _ := args["update"].(map[string]any)
	patch, _ := update["email-1"].(map[string]any)
	if patch["mailboxIds/mb-snoozed"] != any(true) {
		t.Errorf("expected mb-snoozed added on move-in, got %v", patch["mailboxIds/mb-snoozed"])
	}
	if patch["mailboxIds/mb-inbox"] != any(nil) {
		t.Errorf("expected mb-inbox dropped on move-in, got %v", patch["mailboxIds/mb-inbox"])
	}
	if _, has := patch["keywords/$seen"]; has {
		t.Errorf("move-in patch must not touch $seen, got %v", patch["keywords/$seen"])
	}
}

func TestCreate_RejectsSnoozedMailboxInOriginals(t *testing.T) {
	fm := &fakeManager{}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	bodyMap := map[string]any{
		"email_id":             "email-1",
		"original_mailbox_ids": map[string]bool{"mb-snoozed": true, "mb-inbox": true},
		"snoozed_mailbox_id":   "mb-snoozed",
		"snooze_until":         testNow.Add(time.Hour).UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(bodyMap)

	w := httptest.NewRecorder()
	r := handlerRequest("POST", "/api/v1/snooze", string(b))
	router.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when snoozed mailbox is in originals", w.Code)
	}
	if fd.calls != 0 {
		t.Fatalf("expected no dispatch when validation rejects, got %d", fd.calls)
	}
}

func TestCreate_StalwartRefusalReturns502(t *testing.T) {
	fm := &fakeManager{}
	// The Stalwart error text must NOT appear in the response
	// body — leaking it would expose internal infrastructure
	// detail (server names, mailbox ids that didn't resolve,
	// etc.) to the client.
	fd := &fakeDispatcher{err: errors.New("internal-host-42:9123 EOF")}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("POST", "/api/v1/snooze", makeCreateBody())
	router.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 when stalwart move fails", w.Code)
	}
	if len(fm.created) != 0 {
		t.Fatalf("must NOT persist row when JMAP move fails: %d rows created", len(fm.created))
	}
	if strings.Contains(w.Body.String(), "internal-host-42") {
		t.Fatalf("response body must NOT leak Stalwart error text; got %s", w.Body.String())
	}
}

func TestCreate_DuplicateSnoozeReturns409(t *testing.T) {
	fm := &fakeManager{snoozeErr: ErrAlreadySnoozed}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("POST", "/api/v1/snooze", makeCreateBody())
	router.ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for duplicate active snooze", w.Code)
	}
}

// TestCreate_PersistFailureRevertsByRestoringOriginalsAndDroppingSnoozed
// pins the BUG_0001 fix: when DB persistence fails after the
// move-to-snoozed succeeded, the best-effort revert dispatch
// must produce the WAKE patch (add originals + drop snoozed)
// — NOT a no-op patch that only re-asserts the snoozed mailbox
// and leaves the email orphaned in the Snoozed folder.
func TestCreate_PersistFailureRevertsByRestoringOriginalsAndDroppingSnoozed(t *testing.T) {
	fm := &fakeManager{snoozeErr: errors.New("postgres exploded")}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("POST", "/api/v1/snooze", makeCreateBody())
	router.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when persist fails", w.Code)
	}
	// Two dispatches expected: 1) move-to-snoozed (the forward
	// patch), 2) revert (the wake-shape patch).
	if fd.calls != 2 {
		t.Fatalf("expected 2 dispatches (forward + revert), got %d", fd.calls)
	}
	// Inspect the SECOND dispatch — must be a true wake patch.
	args, _ := fd.requests[1].MethodCalls[0][1].(map[string]any)
	update, _ := args["update"].(map[string]any)
	patch, _ := update["email-1"].(map[string]any)
	if patch["mailboxIds/mb-inbox"] != any(true) {
		t.Errorf("revert must ADD original mb-inbox back, got %v", patch["mailboxIds/mb-inbox"])
	}
	if v, has := patch["mailboxIds/mb-snoozed"]; !has || v != any(nil) {
		t.Errorf("revert must DROP mb-snoozed (set to nil), got %v (has=%v)", v, has)
	}
	// Revert is restoring pre-snooze state, NOT waking the user
	// (the snoozed-folder UI was never shown), so $seen must be
	// left untouched.
	if _, has := patch["keywords/$seen"]; has {
		t.Errorf("revert must NOT touch keywords/$seen; got %v", patch["keywords/$seen"])
	}
}

// TestCreate_HorizonTooShortFastFailsBeforeDispatch pins the
// ANALYSIS_0002 fix: an out-of-horizon snooze_until must 400
// before the JMAP forward-move dispatches, so a clearly-invalid
// request doesn't burn two Stalwart round-trips (move + revert).
func TestCreate_HorizonTooShortFastFailsBeforeDispatch(t *testing.T) {
	fm := &fakeManager{}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	bodyMap := map[string]any{
		"email_id":             "email-1",
		"original_mailbox_ids": map[string]bool{"mb-inbox": true},
		"snoozed_mailbox_id":   "mb-snoozed",
		// 30s in the future — below MinSnoozeHorizon (1m).
		"snooze_until": testNow.Add(30 * time.Second).UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(bodyMap)
	w := httptest.NewRecorder()
	r := handlerRequest("POST", "/api/v1/snooze", string(b))
	router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400 for sub-min horizon", w.Code, w.Body.String())
	}
	if fd.calls != 0 {
		t.Fatalf("expected ZERO dispatches when horizon validation rejects up-front, got %d", fd.calls)
	}
	if len(fm.created) != 0 {
		t.Fatalf("expected ZERO service.Snooze calls when horizon validation rejects up-front, got %d", len(fm.created))
	}
}

// TestCreate_HorizonTooFarFastFailsBeforeDispatch is the upper-
// bound twin of TestCreate_HorizonTooShortFastFailsBeforeDispatch.
func TestCreate_HorizonTooFarFastFailsBeforeDispatch(t *testing.T) {
	fm := &fakeManager{}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	bodyMap := map[string]any{
		"email_id":             "email-1",
		"original_mailbox_ids": map[string]bool{"mb-inbox": true},
		"snoozed_mailbox_id":   "mb-snoozed",
		// 366 days out — beyond MaxSnoozeHorizon (365d).
		"snooze_until": testNow.Add(366 * 24 * time.Hour).UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(bodyMap)
	w := httptest.NewRecorder()
	r := handlerRequest("POST", "/api/v1/snooze", string(b))
	router.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400 for above-max horizon", w.Code, w.Body.String())
	}
	if fd.calls != 0 {
		t.Fatalf("expected ZERO dispatches when horizon validation rejects up-front, got %d", fd.calls)
	}
}

// TestCreate_DuplicateSnoozeSkipsRevert pins the ANALYSIS_0001
// fix: when Snooze() returns ErrAlreadySnoozed, the prior snooze
// is already in the Snoozed folder, so the forward JMAP patch
// was a no-op. A revert here would undo the FIRST snooze's JMAP
// state. The handler must skip the revert dispatch entirely.
func TestCreate_DuplicateSnoozeSkipsRevert(t *testing.T) {
	fm := &fakeManager{snoozeErr: ErrAlreadySnoozed}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("POST", "/api/v1/snooze", makeCreateBody())
	router.ServeHTTP(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for duplicate active snooze", w.Code)
	}
	// One dispatch expected: the forward move-to-snoozed. The
	// revert MUST NOT fire (would undo the first snooze).
	if fd.calls != 1 {
		t.Fatalf("expected exactly 1 dispatch (forward only, no revert), got %d", fd.calls)
	}
}

// TestWakeNow_AlreadyAwakeReturns200 pins the BUG_0002 fix: if
// the worker beat the user's DELETE (ErrAlreadyAwake), the email
// is already at its target location and the operation is
// fully-successful. Returning 500 in this case would surface a
// spurious error for a no-op cancel.
func TestWakeNow_AlreadyAwakeReturns200(t *testing.T) {
	row := &Snooze{
		ID: "s-1", TenantID: "tenant-a", KChatUserID: "kchat-a",
		StalwartAccountID:  "acct-a",
		EmailID:            "email-1",
		Status:             StatusSnoozed,
		OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
		SnoozedMailboxID:   "mb-snoozed",
	}
	fm := &fakeManager{getResult: row, cancelErr: ErrAlreadyAwake}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/snoozed/s-1", "")
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 when worker already woke", w.Code, w.Body.String())
	}
}

// TestWakeNow_AlreadyCancelledReturns200 is the sibling case to
// TestWakeNow_AlreadyAwakeReturns200: when another user-driven
// cancel already landed, the row is terminal+successful and the
// repeat request should idempotently return 200.
func TestWakeNow_AlreadyCancelledReturns200(t *testing.T) {
	row := &Snooze{
		ID: "s-1", TenantID: "tenant-a", KChatUserID: "kchat-a",
		StalwartAccountID:  "acct-a",
		EmailID:            "email-1",
		Status:             StatusSnoozed,
		OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
		SnoozedMailboxID:   "mb-snoozed",
	}
	fm := &fakeManager{getResult: row, cancelErr: ErrAlreadyCancelled}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/snoozed/s-1", "")
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 on idempotent cancel", w.Code, w.Body.String())
	}
}

func TestCreate_InvalidJSONReturns400(t *testing.T) {
	fm := &fakeManager{}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("POST", "/api/v1/snooze", "{not valid json")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for invalid json", w.Code)
	}
}

func TestList_HappyPath(t *testing.T) {
	fm := &fakeManager{
		rows: []Snooze{
			{ID: "s-1", TenantID: "tenant-a", Status: StatusSnoozed, EmailID: "email-1", SnoozedMailboxID: "mb-snoozed", SnoozeUntil: time.Now().Add(time.Hour), CreatedAt: time.Now()},
			{ID: "s-2", TenantID: "tenant-a", Status: StatusUnsnoozed, EmailID: "email-2", SnoozedMailboxID: "mb-snoozed", SnoozeUntil: time.Now().Add(-time.Hour), CreatedAt: time.Now()},
		},
	}
	router := newRouterForTest(fm, &fakeDispatcher{})

	w := httptest.NewRecorder()
	r := handlerRequest("GET", "/api/v1/snoozed", "")
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		Snoozes []responsePayload `json:"snoozes"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Snoozes) != 2 {
		t.Fatalf("got %d rows, want 2", len(body.Snoozes))
	}
}

func TestGet_NotFoundReturns404(t *testing.T) {
	fm := &fakeManager{getErr: ErrNotFound}
	router := newRouterForTest(fm, &fakeDispatcher{})
	w := httptest.NewRecorder()
	r := handlerRequest("GET", "/api/v1/snoozed/missing", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestWakeNow_HappyPath(t *testing.T) {
	fm := &fakeManager{
		getResult: &Snooze{
			ID:                 "s-1",
			TenantID:           "tenant-a",
			KChatUserID:        "kchat-a",
			StalwartAccountID:  "acct-a",
			EmailID:            "email-1",
			OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
			SnoozedMailboxID:   "mb-snoozed",
			Status:             StatusSnoozed,
		},
	}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/snoozed/s-1", "")
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	if fd.calls != 1 {
		t.Fatalf("expected 1 dispatch (move-back), got %d", fd.calls)
	}
	if len(fm.cancels) != 1 || fm.cancels[0] != "s-1" {
		t.Fatalf("expected service.Cancel(s-1), got %v", fm.cancels)
	}
	// Verify the patch shape: originals restored, snoozed dropped.
	// The fixture row has MarkUnreadOnWake=false (zero value), so
	// `keywords/$seen` MUST NOT be present in the patch. The
	// happy-path test for MarkUnreadOnWake=true lives in
	// TestWakeNow_HonoursMarkUnreadOnWake below.
	args, _ := fd.requests[0].MethodCalls[0][1].(map[string]any)
	update, _ := args["update"].(map[string]any)
	patch, _ := update["email-1"].(map[string]any)
	if patch["mailboxIds/mb-inbox"] != any(true) {
		t.Errorf("expected mb-inbox restored on wake, got %v", patch["mailboxIds/mb-inbox"])
	}
	if patch["mailboxIds/mb-snoozed"] != any(nil) {
		t.Errorf("expected mb-snoozed dropped on wake, got %v", patch["mailboxIds/mb-snoozed"])
	}
	if _, has := patch["keywords/$seen"]; has {
		t.Errorf("expected NO $seen keyword change when MarkUnreadOnWake=false; got %v", patch["keywords/$seen"])
	}
}

// TestWakeNow_HonoursMarkUnreadOnWake pins the BUG_pr-review-job-cba…0001
// fix: the DELETE /api/v1/snoozed/{id} early-wake path must
// emit the SAME JMAP patch the worker would have emitted at the
// scheduled wake-time. The worker clears `keywords/$seen` via
// buildWakePatch when MarkUnreadOnWake=true (worker.go:256-259);
// the handler must honour the same flag so an early wake resurfaces
// the email as new — otherwise the user sees the snoozed email
// back in their inbox but still marked as read, defeating the
// whole point of snooze.
func TestWakeNow_HonoursMarkUnreadOnWake(t *testing.T) {
	fm := &fakeManager{
		getResult: &Snooze{
			ID:                 "s-1",
			TenantID:           "tenant-a",
			KChatUserID:        "kchat-a",
			StalwartAccountID:  "acct-a",
			EmailID:            "email-1",
			OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
			SnoozedMailboxID:   "mb-snoozed",
			Status:             StatusSnoozed,
			MarkUnreadOnWake:   true,
		},
	}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/snoozed/s-1", "")
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	args, _ := fd.requests[0].MethodCalls[0][1].(map[string]any)
	update, _ := args["update"].(map[string]any)
	patch, _ := update["email-1"].(map[string]any)
	if patch["mailboxIds/mb-inbox"] != any(true) {
		t.Errorf("expected mb-inbox restored on wake, got %v", patch["mailboxIds/mb-inbox"])
	}
	if patch["mailboxIds/mb-snoozed"] != any(nil) {
		t.Errorf("expected mb-snoozed dropped on wake, got %v", patch["mailboxIds/mb-snoozed"])
	}
	// The load-bearing assertion: $seen MUST be cleared so the
	// email resurfaces as new.
	v, has := patch["keywords/$seen"]
	if !has {
		t.Errorf("expected keywords/$seen=nil in patch when MarkUnreadOnWake=true; missing entirely")
	}
	if v != any(nil) {
		t.Errorf("expected keywords/$seen=nil, got %v", v)
	}
}

func TestWakeNow_AlreadyTerminalIsIdempotent(t *testing.T) {
	fm := &fakeManager{
		getResult: &Snooze{
			ID:                 "s-1",
			TenantID:           "tenant-a",
			KChatUserID:        "kchat-a",
			StalwartAccountID:  "acct-a",
			EmailID:            "email-1",
			OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
			SnoozedMailboxID:   "mb-snoozed",
			Status:             StatusUnsnoozed,
		},
	}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)

	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/snoozed/s-1", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 idempotent", w.Code)
	}
	if fd.calls != 0 {
		t.Fatalf("expected NO dispatch when already terminal, got %d", fd.calls)
	}
}

func TestWakeNow_NotFoundReturns404(t *testing.T) {
	fm := &fakeManager{getErr: ErrNotFound}
	fd := &fakeDispatcher{}
	router := newRouterForTest(fm, fd)
	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/snoozed/missing", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestWakeNow_StalwartRefusalReturns502(t *testing.T) {
	fm := &fakeManager{
		getResult: &Snooze{
			ID:                 "s-1",
			TenantID:           "tenant-a",
			KChatUserID:        "kchat-a",
			StalwartAccountID:  "acct-a",
			EmailID:            "email-1",
			OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
			SnoozedMailboxID:   "mb-snoozed",
			Status:             StatusSnoozed,
		},
	}
	// Stalwart error text must NOT appear in the response —
	// same hardening as TestCreate_StalwartRefusalReturns502.
	fd := &fakeDispatcher{err: errors.New("internal-host-42:9123 connect refused")}
	router := newRouterForTest(fm, fd)
	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/snoozed/s-1", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if len(fm.cancels) != 0 {
		t.Fatalf("must NOT cancel row when JMAP wake failed: %v", fm.cancels)
	}
	if strings.Contains(w.Body.String(), "internal-host-42") {
		t.Fatalf("response body must NOT leak Stalwart error text; got %s", w.Body.String())
	}
}

// TestWakeNow_StalwartRefusalAfterWorkerWokeReturns200 pins the
// Round 5 TOCTOU fix: between the handler's initial Get and the
// JMAP applyMove, the worker may have claimed the row,
// dispatched its own wake patch, and marked the row unsnoozed.
// Stalwart may then reject the handler's redundant applyMove
// (notUpdated for stale mailboxIds), but the email is correctly
// at its target location — returning 502 would surface a
// spurious error for a fully-successful wake. The handler must
// re-read the row; if the worker won the race (status !=
// snoozed), return 200.
func TestWakeNow_StalwartRefusalAfterWorkerWokeReturns200(t *testing.T) {
	first := &Snooze{
		ID:                 "s-1",
		TenantID:           "tenant-a",
		KChatUserID:        "kchat-a",
		StalwartAccountID:  "acct-a",
		EmailID:            "email-1",
		OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
		SnoozedMailboxID:   "mb-snoozed",
		Status:             StatusSnoozed,
	}
	// Models the worker landing markUnsnoozed between the
	// handler's first and second Get.
	second := *first
	second.Status = StatusUnsnoozed
	fm := &fakeManager{
		getResult:       first,
		getResultSecond: &second,
	}
	fd := &fakeDispatcher{err: errors.New("internal-host-42:9123 stalwart notUpdated stale mailbox")}
	router := newRouterForTest(fm, fd)
	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/snoozed/s-1", "")
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200 when worker won the wake race", w.Code, w.Body.String())
	}
	if len(fm.cancels) != 0 {
		t.Fatalf("must NOT call Cancel when worker already woke; got %v", fm.cancels)
	}
	if strings.Contains(w.Body.String(), "internal-host-42") {
		t.Fatalf("200-after-race response body must NOT leak Stalwart error text; got %s", w.Body.String())
	}
}

// TestGet_PerUserScopedAtHandlerLayer pins the per-user authz
// fence at the handler entry point: the handler must extract
// `kchat_user_id` from the auth context and pass it to
// `Service.Get`. Without this regression guard, a refactor of
// the handler that drops the second arg (or hard-codes "") would
// compile and pass every other test, but silently re-open the
// cross-user UUID-guessing hole the Service layer's belt closes.
func TestGet_PerUserScopedAtHandlerLayer(t *testing.T) {
	fm := &fakeManager{
		getResult: &Snooze{
			ID:          "s-1",
			TenantID:    "tenant-a",
			KChatUserID: "kchat-a",
			Status:      StatusSnoozed,
		},
	}
	router := newRouterForTest(fm, &fakeDispatcher{})
	w := httptest.NewRecorder()
	r := handlerRequest("GET", "/api/v1/snoozed/s-1", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	if fm.getArgs.tenantID != "tenant-a" {
		t.Errorf("handler must pass tenantID=tenant-a to Service.Get, got %q", fm.getArgs.tenantID)
	}
	if fm.getArgs.kchatUserID != "kchat-a" {
		t.Errorf("handler must pass kchatUserID=kchat-a to Service.Get, got %q", fm.getArgs.kchatUserID)
	}
	if fm.getArgs.id != "s-1" {
		t.Errorf("handler must pass id=s-1 to Service.Get, got %q", fm.getArgs.id)
	}
}

// TestWakeNow_PerUserScopedAtHandlerLayer — same as above for the
// DELETE path. Both Get (to read the row) AND Cancel (to flip
// terminal state) must carry kchatUserID.
func TestWakeNow_PerUserScopedAtHandlerLayer(t *testing.T) {
	fm := &fakeManager{
		getResult: &Snooze{
			ID:                 "s-1",
			TenantID:           "tenant-a",
			KChatUserID:        "kchat-a",
			StalwartAccountID:  "acct-a",
			EmailID:            "email-1",
			OriginalMailboxIDs: json.RawMessage(`{"mb-inbox":true}`),
			SnoozedMailboxID:   "mb-snoozed",
			Status:             StatusSnoozed,
		},
	}
	router := newRouterForTest(fm, &fakeDispatcher{})
	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/snoozed/s-1", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	if fm.getArgs.kchatUserID != "kchat-a" {
		t.Errorf("wakeNow must pass kchatUserID=kchat-a to Service.Get, got %q", fm.getArgs.kchatUserID)
	}
	if fm.cancelArgs.kchatUserID != "kchat-a" {
		t.Errorf("wakeNow must pass kchatUserID=kchat-a to Service.Cancel, got %q", fm.cancelArgs.kchatUserID)
	}
	if fm.cancelArgs.tenantID != "tenant-a" {
		t.Errorf("wakeNow must pass tenantID=tenant-a to Service.Cancel, got %q", fm.cancelArgs.tenantID)
	}
	if fm.cancelArgs.id != "s-1" {
		t.Errorf("wakeNow must pass id=s-1 to Service.Cancel, got %q", fm.cancelArgs.id)
	}
}
