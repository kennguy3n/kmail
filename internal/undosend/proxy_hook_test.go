package undosend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

func contextWithIdentity(tenantID, kchatUserID string) context.Context {
	ctx := middleware.WithTenantID(context.Background(), tenantID)
	ctx = middleware.WithKChatUserID(ctx, kchatUserID)
	return ctx
}

func newJMAPRequest(t *testing.T, body []byte, ctx context.Context, optIn bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/jmap", bytes.NewReader(body))
	if optIn {
		r.Header.Set(HeaderOptIn, "true")
	}
	return r.WithContext(ctx)
}

type stubForwarder struct {
	called    int
	response  *jmap.JmapResponse
	err       error
	lastReq   jmap.JmapRequest
}

func (s *stubForwarder) Dispatch(_ context.Context, _, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	s.called++
	s.lastReq = req
	if s.err != nil {
		return nil, s.err
	}
	return s.response, nil
}

func buildSendBody(emailCreateKey, subCreateKey, identityID string) []byte {
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

func stalwartStrippedResponse(emailCreateKey, realEmailID string) *jmap.JmapResponse {
	return &jmap.JmapResponse{
		SessionState: "session-1",
		MethodResponses: [][]any{
			{"Email/set", map[string]any{
				"accountId": "acct-a",
				"created": map[string]any{
					emailCreateKey: map[string]any{"id": realEmailID},
				},
			}, "0"},
		},
	}
}

func newTestHook(t *testing.T, svc *Service, forwarder InternalSubmitter) *Hook {
	t.Helper()
	h, err := NewHook(HookConfig{
		Service:   svc,
		Forwarder: forwarder,
		AccountResolver: func(_ context.Context, _, _ string) (string, error) {
			return "acct-a", nil
		},
	})
	if err != nil {
		t.Fatalf("NewHook: %v", err)
	}
	return h
}

func TestIntercept_HappyPath(t *testing.T) {
	svc, _, _ := newTestService(t)
	fwd := &stubForwarder{response: stalwartStrippedResponse("draft", "REAL-email-1")}
	hook := newTestHook(t, svc, fwd)

	body := buildSendBody("draft", "submission", "ident-1")
	ctx := contextWithIdentity("tenant-a", "kchat-a")
	r := newJMAPRequest(t, body, ctx, true)
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err != nil {
		t.Fatalf("Intercept: %v", err)
	}
	if !intercepted {
		t.Fatalf("expected intercepted=true")
	}
	pendingID := w.Header().Get(HeaderPendingID)
	if pendingID == "" {
		t.Fatalf("missing %s header", HeaderPendingID)
	}
	deadline := w.Header().Get(HeaderDeadline)
	if deadline == "" {
		t.Fatalf("missing %s header", HeaderDeadline)
	}
	if _, err := strconv.ParseInt(deadline, 10, 64); err != nil {
		t.Fatalf("deadline header not unix int: %v", err)
	}

	// Forwarder must see a body with EmailSubmission/set stripped.
	if len(fwd.lastReq.MethodCalls) != 1 {
		t.Fatalf("stripped request should carry exactly 1 call, got %d", len(fwd.lastReq.MethodCalls))
	}
	if name, _ := fwd.lastReq.MethodCalls[0][0].(string); name != "Email/set" {
		t.Fatalf("stripped request first call = %s, want Email/set", name)
	}

	// Verify pending send persisted in Valkey.
	ps, err := svc.Get(ctx, pendingID)
	if err != nil {
		t.Fatalf("Get persisted: %v", err)
	}
	if ps.EmailID != "REAL-email-1" {
		t.Fatalf("EmailID = %q, want REAL-email-1", ps.EmailID)
	}
	if ps.IdentityID != "ident-1" {
		t.Fatalf("IdentityID = %q, want ident-1", ps.IdentityID)
	}

	// Verify the synthesised response body shape.
	respBytes, err := io.ReadAll(w.Result().Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var resp jmap.JmapResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.MethodResponses) != 2 {
		t.Fatalf("expected 2 method responses, got %d", len(resp.MethodResponses))
	}
	if resp.MethodResponses[1][0].(string) != "EmailSubmission/set" {
		t.Fatalf("second method response = %v, want EmailSubmission/set", resp.MethodResponses[1][0])
	}
}

func TestIntercept_OptOutFallsThrough(t *testing.T) {
	svc, _, _ := newTestService(t)
	fwd := &stubForwarder{}
	hook := newTestHook(t, svc, fwd)

	body := buildSendBody("draft", "submission", "ident-1")
	ctx := contextWithIdentity("tenant-a", "kchat-a")
	// No opt-in header.
	r := newJMAPRequest(t, body, ctx, false)
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err != nil {
		t.Fatalf("Intercept: %v", err)
	}
	if intercepted {
		t.Fatalf("expected fall-through without opt-in header")
	}
	if fwd.called != 0 {
		t.Fatalf("forwarder should not be called on fall-through")
	}
}

func TestIntercept_NoEmailSubmissionFallsThrough(t *testing.T) {
	svc, _, _ := newTestService(t)
	fwd := &stubForwarder{}
	hook := newTestHook(t, svc, fwd)

	body := []byte(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Email/get",{"accountId":"acct-a"},"0"]]}`)
	ctx := contextWithIdentity("tenant-a", "kchat-a")
	r := newJMAPRequest(t, body, ctx, true)
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err != nil {
		t.Fatalf("Intercept: %v", err)
	}
	if intercepted {
		t.Fatalf("read-only batch should fall through")
	}
}

func TestIntercept_StalwartErrorPropagated(t *testing.T) {
	svc, _, _ := newTestService(t)
	fwd := &stubForwarder{err: errors.New("connect refused")}
	hook := newTestHook(t, svc, fwd)

	body := buildSendBody("draft", "submission", "ident-1")
	ctx := contextWithIdentity("tenant-a", "kchat-a")
	r := newJMAPRequest(t, body, ctx, true)
	w := httptest.NewRecorder()

	intercepted, err := hook.Intercept(ctx, w, r, body)
	if err == nil {
		t.Fatalf("expected error to be propagated to proxy")
	}
	if intercepted {
		t.Fatalf("forwarder failure should not claim interception")
	}
}

func TestIntercept_DeadlineHeaderMatchesService(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	svc, _, _ := newTestService(t, func(c *Config) {
		c.Delay = 12 * time.Second
		c.NowFunc = func() time.Time { return now }
	})
	fwd := &stubForwarder{response: stalwartStrippedResponse("draft", "REAL-email-1")}
	hook := newTestHook(t, svc, fwd)

	body := buildSendBody("draft", "submission", "ident-1")
	ctx := contextWithIdentity("tenant-a", "kchat-a")
	r := newJMAPRequest(t, body, ctx, true)
	w := httptest.NewRecorder()

	if _, err := hook.Intercept(ctx, w, r, body); err != nil {
		t.Fatalf("Intercept: %v", err)
	}
	deadline, err := strconv.ParseInt(w.Header().Get(HeaderDeadline), 10, 64)
	if err != nil {
		t.Fatalf("ParseInt deadline: %v", err)
	}
	if deadline != now.Add(12*time.Second).Unix() {
		t.Fatalf("deadline = %d, want %d", deadline, now.Add(12*time.Second).Unix())
	}
}

func TestNormaliseSubmissionPayload_ResolvesBackReference(t *testing.T) {
	src := map[string]any{
		"emailId":    "#draft",
		"identityId": "ident-1",
	}
	out, err := normaliseSubmissionPayload(src, "draft", "REAL-1")
	if err != nil {
		t.Fatalf("normaliseSubmissionPayload: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["emailId"] != "REAL-1" {
		t.Fatalf("emailId not resolved: %v", got)
	}
}

func TestResolveCreatedEmailID_HappyPath(t *testing.T) {
	resp := stalwartStrippedResponse("draft", "REAL-1")
	got, ok := resolveCreatedEmailID(resp, "draft")
	if !ok || got != "REAL-1" {
		t.Fatalf("resolveCreatedEmailID = %q, %v; want REAL-1, true", got, ok)
	}
}

func TestResolveCreatedEmailID_MissingReturnsFalse(t *testing.T) {
	resp := &jmap.JmapResponse{MethodResponses: [][]any{}}
	got, ok := resolveCreatedEmailID(resp, "draft")
	if ok || got != "" {
		t.Fatalf("expected miss, got %q, %v", got, ok)
	}
}
