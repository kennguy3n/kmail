package priority

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/smartfeatures"
)

func authed(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	ctx := middleware.WithKChatUserID(middleware.WithTenantID(context.Background(), "t1"), "u1")
	return r.WithContext(ctx)
}

func TestPriorityInboxHandler_Compute(t *testing.T) {
	src := &fakeSource{msgs: []smartfeatures.Message{
		msg("E1", "a@b.com", time.Now()),
	}}
	svc, _ := NewService(Config{Source: src})
	h := NewHandlers(svc, nil, nil)

	w := httptest.NewRecorder()
	h.priorityInbox(w, authed(http.MethodGet, "/api/v1/priority-inbox"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["cached"] != false {
		t.Fatalf("expected cached=false, got %v", out["cached"])
	}
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %v", out["items"])
	}
}

func TestPriorityInboxHandler_CachedNoStore(t *testing.T) {
	src := &fakeSource{}
	svc, _ := NewService(Config{Source: src})
	h := NewHandlers(svc, nil, nil)
	w := httptest.NewRecorder()
	h.priorityInbox(w, authed(http.MethodGet, "/api/v1/priority-inbox?cached=1"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestPriorityInboxHandler_CachedWithStore(t *testing.T) {
	ctx := context.Background()
	store, _ := NewStore(newTestRedis(t), time.Minute)
	_ = store.Save(ctx, "t1", "u1", []Scored{scored("E9", 88)})
	src := &fakeSource{}
	svc, _ := NewService(Config{Source: src, Store: store})
	h := NewHandlers(svc, store, nil)

	w := httptest.NewRecorder()
	h.priorityInbox(w, authed(http.MethodGet, "/api/v1/priority-inbox?cached=1"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["cached"] != true {
		t.Fatalf("expected cached=true")
	}
}

func TestPriorityInboxHandler_MissingAuth(t *testing.T) {
	src := &fakeSource{}
	svc, _ := NewService(Config{Source: src})
	h := NewHandlers(svc, nil, nil)
	w := httptest.NewRecorder()
	// No identity on context.
	h.priorityInbox(w, httptest.NewRequest(http.MethodGet, "/api/v1/priority-inbox", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
