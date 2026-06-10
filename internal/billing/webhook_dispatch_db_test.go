package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// validV1 computes the HMAC-SHA256 v1 signature over `<ts>.<body>`
// for a string-form timestamp (used to exercise the non-numeric
// timestamp branch where the HMAC still matches).
func validV1(secret, ts string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return hex.EncodeToString(mac.Sum(nil))
}

// stubSignupCompleter records CompleteCheckoutSignup invocations.
type stubSignupCompleter struct {
	called  int
	lastID  string
	failErr error
}

func (s *stubSignupCompleter) CompleteCheckoutSignup(_ context.Context, id string) error {
	s.called++
	s.lastID = id
	return s.failErr
}

// serveWebhook posts a signed event body through the full serve()
// path and returns the response recorder.
func serveWebhook(h *WebhookHandler, secret string, ts int64, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/v1/billing/webhooks/stripe", strings.NewReader(body))
	if secret != "" {
		r.Header.Set("Stripe-Signature", signStripe(secret, ts, []byte(body)))
	}
	rr := httptest.NewRecorder()
	h.serve(rr, r)
	return rr
}

func currentSubStatus(t *testing.T, lc *Lifecycle, tenant string) SubscriptionStatus {
	t.Helper()
	sub, err := lc.GetSubscription(context.Background(), tenant)
	if err != nil {
		t.Fatalf("GetSubscription: %v", err)
	}
	return sub.Status
}

func TestWebhookDispatchLifecycleDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()
	lc := NewLifecycle(svc, nil)
	if err := lc.OnTenantCreated(ctx, tenant, PlanCore); err != nil {
		t.Fatalf("OnTenantCreated: %v", err)
	}

	now := time.Unix(1_700_000_000, 0)
	h := NewWebhookHandler(WebhookConfig{
		Lifecycle: lc,
		Now:       func() time.Time { return now },
	})
	ts := now.Unix()

	// invoice.payment_failed → past_due.
	body := fmt.Sprintf(`{"id":"evt_pf","type":"invoice.payment_failed","data":{"object":{"metadata":{"tenant_id":%q}}}}`, tenant)
	if rr := serveWebhook(h, "", ts, body); rr.Code != http.StatusNoContent {
		t.Fatalf("payment_failed code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := currentSubStatus(t, lc, tenant); got != SubscriptionPastDue {
		t.Errorf("after payment_failed status=%s want past_due", got)
	}

	// payment_intent.succeeded → active again.
	body = fmt.Sprintf(`{"id":"evt_pi","type":"payment_intent.succeeded","data":{"object":{"metadata":{"tenant_id":%q}}}}`, tenant)
	if rr := serveWebhook(h, "", ts, body); rr.Code != http.StatusNoContent {
		t.Fatalf("payment_intent code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := currentSubStatus(t, lc, tenant); got != SubscriptionActive {
		t.Errorf("after payment_intent status=%s want active", got)
	}

	// customer.subscription.updated → canceled maps to cancelled.
	body = fmt.Sprintf(`{"id":"evt_su","type":"customer.subscription.updated","data":{"object":{"id":"sub_123","status":"canceled","metadata":{"tenant_id":%q},"current_period_start":%d,"current_period_end":%d}}}`,
		tenant, now.Unix(), now.Add(720*time.Hour).Unix())
	if rr := serveWebhook(h, "", ts, body); rr.Code != http.StatusNoContent {
		t.Fatalf("subscription.updated code=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := currentSubStatus(t, lc, tenant); got != SubscriptionCancelled {
		t.Errorf("after subscription.updated status=%s want cancelled", got)
	}

	// Unknown event type is accepted (204) so Stripe stops retrying.
	if rr := serveWebhook(h, "", ts, `{"id":"evt_x","type":"charge.refunded","data":{"object":{}}}`); rr.Code != http.StatusNoContent {
		t.Errorf("unknown event code=%d", rr.Code)
	}

	// Missing tenant_id metadata is a no-op (still 204).
	if rr := serveWebhook(h, "", ts, `{"id":"evt_no","type":"invoice.paid","data":{"object":{"metadata":{}}}}`); rr.Code != http.StatusNoContent {
		t.Errorf("no-tenant event code=%d", rr.Code)
	}

	// Invalid subscription status → 500 (handler surfaces the error).
	body = fmt.Sprintf(`{"id":"evt_bad","type":"customer.subscription.updated","data":{"object":{"status":"bogus","metadata":{"tenant_id":%q}}}}`, tenant)
	if rr := serveWebhook(h, "", ts, body); rr.Code != http.StatusInternalServerError {
		t.Errorf("bogus status code=%d want 500", rr.Code)
	}
}

func TestWebhookCompleteSignupDB(t *testing.T) {
	stub := &stubSignupCompleter{}
	now := time.Unix(1_700_000_000, 0)
	h := NewWebhookHandler(WebhookConfig{
		SignupCompleter: stub,
		Now:             func() time.Time { return now },
	})
	ts := now.Unix()

	body := `{"id":"evt_cs","type":"checkout.session.completed","data":{"object":{"id":"cs_test_123"}}}`
	if rr := serveWebhook(h, "", ts, body); rr.Code != http.StatusNoContent {
		t.Fatalf("checkout.completed code=%d body=%s", rr.Code, rr.Body.String())
	}
	if stub.called != 1 || stub.lastID != "cs_test_123" {
		t.Errorf("signup completer: called=%d id=%q", stub.called, stub.lastID)
	}

	// Empty session id is a no-op.
	stub.called = 0
	body = `{"id":"evt_cs2","type":"checkout.session.completed","data":{"object":{"id":""}}}`
	if rr := serveWebhook(h, "", ts, body); rr.Code != http.StatusNoContent {
		t.Fatalf("empty session code=%d", rr.Code)
	}
	if stub.called != 0 {
		t.Errorf("empty session should not call completer, called=%d", stub.called)
	}

	// With no completer wired, the event is accepted and ignored.
	hNoComp := NewWebhookHandler(WebhookConfig{Now: func() time.Time { return now }})
	if rr := serveWebhook(hNoComp, "", ts, `{"id":"e","type":"checkout.session.completed","data":{"object":{"id":"cs"}}}`); rr.Code != http.StatusNoContent {
		t.Errorf("nil completer code=%d", rr.Code)
	}
}

func TestWebhookServeErrors(t *testing.T) {
	secret := "whsec_err"
	now := time.Unix(1_700_000_000, 0)
	h := NewWebhookHandler(WebhookConfig{
		StripeWebhookSecret: secret,
		Now:                 func() time.Time { return now },
	})
	ts := now.Unix()

	// Valid signature but malformed JSON → 400.
	bad := "not json"
	r := httptest.NewRequest("POST", "/", strings.NewReader(bad))
	r.Header.Set("Stripe-Signature", signStripe(secret, ts, []byte(bad)))
	rr := httptest.NewRecorder()
	h.serve(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("malformed json code=%d want 400", rr.Code)
	}

	// Wrong signature → 400.
	body := `{"id":"e","type":"invoice.paid","data":{"object":{}}}`
	r = httptest.NewRequest("POST", "/", strings.NewReader(body))
	r.Header.Set("Stripe-Signature", "t=1,v1=deadbeef")
	rr = httptest.NewRecorder()
	h.serve(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad sig code=%d want 400", rr.Code)
	}

	// Missing signature header → 400.
	r = httptest.NewRequest("POST", "/", strings.NewReader(body))
	rr = httptest.NewRecorder()
	h.serve(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing sig code=%d want 400", rr.Code)
	}
}

func TestVerifyMalformedHeaders(t *testing.T) {
	secret := "whsec_x"
	now := time.Unix(1_700_000_000, 0)
	h := NewWebhookHandler(WebhookConfig{StripeWebhookSecret: secret, Now: func() time.Time { return now }})
	body := []byte(`{}`)

	mk := func(sig string) *http.Request {
		r := httptest.NewRequest("POST", "/", nil)
		r.Header.Set("Stripe-Signature", sig)
		return r
	}

	if err := h.verify(mk(""), body); err == nil {
		t.Error("empty header should fail")
	}
	if err := h.verify(mk("garbage-without-equals"), body); err == nil {
		t.Error("header without t/v1 should fail")
	}
	if err := h.verify(mk("t=notanumber,v1="+validV1(secret, "notanumber", body)), body); err == nil {
		t.Error("non-numeric timestamp should fail")
	}

	// Dev mode (empty secret) accepts anything.
	hDev := NewWebhookHandler(WebhookConfig{})
	if err := hDev.verify(httptest.NewRequest("POST", "/", nil), body); err != nil {
		t.Errorf("dev-mode verify: %v", err)
	}
}

func TestValidSubscriptionStatus(t *testing.T) {
	for _, s := range []string{"active", "trialing", "past_due", "unpaid", "canceled", "cancelled", "incomplete", "incomplete_expired"} {
		if !validSubscriptionStatus(s) {
			t.Errorf("validSubscriptionStatus(%q)=false want true", s)
		}
	}
	for _, s := range []string{"", "bogus", "deleted"} {
		if validSubscriptionStatus(s) {
			t.Errorf("validSubscriptionStatus(%q)=true want false", s)
		}
	}
}
