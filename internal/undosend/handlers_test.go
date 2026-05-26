package undosend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestHandlers(t *testing.T) (*Handlers, *Service) {
	t.Helper()
	svc, _, _ := newTestService(t)
	return &Handlers{svc: svc}, svc
}

func newAuthedRequest(method, path, tenantID string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	ctx := contextWithIdentity(tenantID, "kchat-a")
	return r.WithContext(ctx)
}

func TestCancelHandler_HappyPath(t *testing.T) {
	h, svc := newTestHandlers(t)
	ps, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	r := newAuthedRequest(http.MethodPost, "/api/v1/send/"+ps.ID+"/cancel", "tenant-a")
	r.SetPathValue("id", ps.ID)
	w := httptest.NewRecorder()
	h.cancel(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["cancelled"] != true {
		t.Fatalf("cancelled = %v", out["cancelled"])
	}
}

func TestCancelHandler_AlreadySentIs410(t *testing.T) {
	h, _ := newTestHandlers(t)
	r := newAuthedRequest(http.MethodPost, "/api/v1/send/missing/cancel", "tenant-a")
	r.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	h.cancel(w, r)
	if w.Code != http.StatusGone {
		t.Fatalf("status = %d, want 410; body=%s", w.Code, w.Body.String())
	}
}

func TestCancelHandler_CrossTenantIs404(t *testing.T) {
	h, svc := newTestHandlers(t)
	ps, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	r := newAuthedRequest(http.MethodPost, "/api/v1/send/"+ps.ID+"/cancel", "tenant-OTHER")
	r.SetPathValue("id", ps.ID)
	w := httptest.NewRecorder()
	h.cancel(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

func TestStatusHandler_HappyPath(t *testing.T) {
	h, svc := newTestHandlers(t)
	ps, err := svc.Hold(context.Background(), HoldInput{
		TenantID:          "tenant-a",
		KChatUserID:       "kchat-a",
		StalwartAccountID: "acct-a",
		EmailID:           "email-1",
		CreateID:          "submission",
		SubmissionPayload: []byte(`{"emailId":"email-1"}`),
	})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	r := newAuthedRequest(http.MethodGet, "/api/v1/send/"+ps.ID, "tenant-a")
	r.SetPathValue("id", ps.ID)
	w := httptest.NewRecorder()
	h.status(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"email_id":"email-1"`) {
		t.Fatalf("body missing email_id: %s", w.Body.String())
	}
}

func TestStatusHandler_MissingReturnsSent(t *testing.T) {
	h, _ := newTestHandlers(t)
	r := newAuthedRequest(http.MethodGet, "/api/v1/send/missing", "tenant-a")
	r.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	h.status(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"sent"`) {
		t.Fatalf("expected sent status: %s", w.Body.String())
	}
}
