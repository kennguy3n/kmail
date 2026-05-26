package scheduledsend

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// fakeManager implements the handlers' `manager` interface.
type fakeManager struct {
	rows       []ScheduledSend
	getResult  *ScheduledSend
	getErr     error
	listErr    error
	cancelErr  error
	getArgs    struct {
		tenantID    string
		kchatUserID string
		id          string
	}
	cancelArgs struct {
		tenantID    string
		kchatUserID string
		id          string
	}
}

func (f *fakeManager) ListByUser(_ context.Context, _, _ string) ([]ScheduledSend, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeManager) Get(_ context.Context, tenantID, kchatUserID, id string) (*ScheduledSend, error) {
	f.getArgs.tenantID = tenantID
	f.getArgs.kchatUserID = kchatUserID
	f.getArgs.id = id
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

func (f *fakeManager) Cancel(_ context.Context, tenantID, kchatUserID, id string) error {
	f.cancelArgs.tenantID = tenantID
	f.cancelArgs.kchatUserID = kchatUserID
	f.cancelArgs.id = id
	return f.cancelErr
}

func handlerRequest(method, target string, body string) *http.Request {
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

func newRouterForTest(m manager) *http.ServeMux {
	mux := http.NewServeMux()
	h := newHandlersWithManager(m)
	mux.HandleFunc("GET /api/v1/scheduled-sends", h.list)
	mux.HandleFunc("GET /api/v1/scheduled-sends/{id}", h.get)
	mux.HandleFunc("DELETE /api/v1/scheduled-sends/{id}", h.cancel)
	return mux
}

func TestList_HappyPath(t *testing.T) {
	sentAt := time.Now().UTC()
	fm := &fakeManager{
		rows: []ScheduledSend{
			{
				ID:       "ss-1",
				TenantID: "tenant-a",
				Status:   StatusPending,
				EmailID:  "email-1",
				SendAt:   time.Now().Add(time.Hour),
				CreatedAt: time.Now(),
			},
			{
				ID:       "ss-2",
				TenantID: "tenant-a",
				Status:   StatusSent,
				EmailID:  "email-2",
				SendAt:   time.Now().Add(-time.Hour),
				SentAt:   &sentAt,
				CreatedAt: time.Now(),
			},
		},
	}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("GET", "/api/v1/scheduled-sends", "")
	router.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body struct {
		ScheduledSends []responsePayload `json:"scheduled_sends"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.ScheduledSends) != 2 {
		t.Fatalf("got %d rows, want 2", len(body.ScheduledSends))
	}
	if body.ScheduledSends[1].SentAt == nil {
		t.Fatalf("expected SentAt on the sent row")
	}
}

func TestGet_NotFoundReturns404(t *testing.T) {
	fm := &fakeManager{getErr: ErrNotFound}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("GET", "/api/v1/scheduled-sends/missing", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestGet_TenantMismatchReturns404(t *testing.T) {
	fm := &fakeManager{getErr: ErrTenantMismatch}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("GET", "/api/v1/scheduled-sends/cross-tenant", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (collapsed from mismatch)", w.Code)
	}
}

func TestCancel_HappyPath(t *testing.T) {
	fm := &fakeManager{}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/scheduled-sends/ss-1", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if fm.cancelArgs.id != "ss-1" {
		t.Fatalf("cancel id = %q, want ss-1", fm.cancelArgs.id)
	}
	if fm.cancelArgs.tenantID != "tenant-a" {
		t.Fatalf("cancel tenant = %q, want tenant-a", fm.cancelArgs.tenantID)
	}
	if fm.cancelArgs.kchatUserID != "kchat-a" {
		t.Fatalf("cancel kchat_user_id = %q, want kchat-a", fm.cancelArgs.kchatUserID)
	}
}

// TestCancel_PerUserScopedAtHandlerLayer pins the per-user authz
// fence at the handler layer. The handler MUST plumb
// kchat_user_id from the request context through to Service.Cancel
// so a peer in the tenant cannot cancel by guessing the UUID. A
// regression that drops the userID arg would silently degrade the
// authz model to tenant-only.
func TestCancel_PerUserScopedAtHandlerLayer(t *testing.T) {
	fm := &fakeManager{}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/scheduled-sends/ss-x", "")
	router.ServeHTTP(w, r)
	if fm.cancelArgs.kchatUserID == "" {
		t.Fatalf("handler did not pass kchat_user_id to Service.Cancel; per-user authz fence missing")
	}
	if fm.cancelArgs.kchatUserID != "kchat-a" {
		t.Fatalf("cancel kchat_user_id = %q, want kchat-a", fm.cancelArgs.kchatUserID)
	}
}

// TestGet_PerUserScopedAtHandlerLayer mirrors
// TestCancel_PerUserScopedAtHandlerLayer for the read path.
func TestGet_PerUserScopedAtHandlerLayer(t *testing.T) {
	fm := &fakeManager{getResult: &ScheduledSend{ID: "ss-x", TenantID: "tenant-a", Status: StatusPending}}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("GET", "/api/v1/scheduled-sends/ss-x", "")
	router.ServeHTTP(w, r)
	if fm.getArgs.kchatUserID == "" {
		t.Fatalf("handler did not pass kchat_user_id to Service.Get; per-user authz fence missing")
	}
	if fm.getArgs.kchatUserID != "kchat-a" {
		t.Fatalf("get kchat_user_id = %q, want kchat-a", fm.getArgs.kchatUserID)
	}
}

func TestCancel_AlreadyDispatchedReturns410(t *testing.T) {
	fm := &fakeManager{cancelErr: ErrAlreadyDispatched}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/scheduled-sends/ss-1", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410", w.Code)
	}
}

func TestCancel_AlreadyCancelledIdempotent(t *testing.T) {
	fm := &fakeManager{cancelErr: ErrAlreadyCancelled}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/scheduled-sends/ss-1", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (idempotent), got %d", w.Code, w.Code)
	}
}

func TestCancel_NotFoundReturns404(t *testing.T) {
	fm := &fakeManager{cancelErr: ErrNotFound}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("DELETE", "/api/v1/scheduled-sends/missing", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestList_PropagatesInternalErrorAsJSON500(t *testing.T) {
	fm := &fakeManager{listErr: errors.New("db down")}
	router := newRouterForTest(fm)
	w := httptest.NewRecorder()
	r := handlerRequest("GET", "/api/v1/scheduled-sends", "")
	router.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "internal error") {
		t.Fatalf("body = %q, want 'internal error' fragment", w.Body.String())
	}
}
