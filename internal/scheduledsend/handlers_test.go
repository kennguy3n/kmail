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
	cancelArgs struct {
		tenantID string
		id       string
	}
}

func (f *fakeManager) ListByUser(_ context.Context, _, _ string) ([]ScheduledSend, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.rows, nil
}

func (f *fakeManager) Get(_ context.Context, _, _ string) (*ScheduledSend, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.getResult, nil
}

func (f *fakeManager) Cancel(_ context.Context, tenantID, id string) error {
	f.cancelArgs.tenantID = tenantID
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
