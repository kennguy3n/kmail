package billing

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeSignupCompleter records the checkout session ids passed to it.
type fakeSignupCompleter struct {
	mu       sync.Mutex
	sessions []string
	err      error
}

func (f *fakeSignupCompleter) CompleteCheckoutSignup(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, sessionID)
	return f.err
}

// TestDispatch_CheckoutCompleted_ProvisionsSignup verifies the new
// checkout.session.completed branch forwards the Checkout Session id
// to the SignupCompleter. It runs through the full serve() path with
// an empty webhook secret (dev-mode accept) so it also covers routing
// and the 204 response.
func TestDispatch_CheckoutCompleted_ProvisionsSignup(t *testing.T) {
	completer := &fakeSignupCompleter{}
	h := NewWebhookHandler(WebhookConfig{})
	h.SetSignupCompleter(completer)

	body := `{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":"cs_test_123"}}}`
	r := httptest.NewRequest("POST", "/api/v1/billing/webhooks/stripe", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.serve(rr, r)

	if rr.Code != 204 {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	completer.mu.Lock()
	defer completer.mu.Unlock()
	if len(completer.sessions) != 1 || completer.sessions[0] != "cs_test_123" {
		t.Fatalf("completer sessions = %v, want [cs_test_123]", completer.sessions)
	}
}

// TestDispatch_CheckoutCompleted_NoCompleter ensures the event is
// accepted (not 500) when no SignupCompleter is wired — keeps Stripe's
// retry behavior happy on deployments without self-service signup.
func TestDispatch_CheckoutCompleted_NoCompleter(t *testing.T) {
	h := NewWebhookHandler(WebhookConfig{})
	body := `{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":"cs_test_123"}}}`
	r := httptest.NewRequest("POST", "/api/v1/billing/webhooks/stripe", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.serve(rr, r)
	if rr.Code != 204 {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

// TestDispatch_CheckoutCompleted_EmptyID is a no-op (the event carries
// no session id to act on) and must not invoke the completer.
func TestDispatch_CheckoutCompleted_EmptyID(t *testing.T) {
	completer := &fakeSignupCompleter{}
	h := NewWebhookHandler(WebhookConfig{})
	h.SetSignupCompleter(completer)

	body := `{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":""}}}`
	r := httptest.NewRequest("POST", "/api/v1/billing/webhooks/stripe", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.serve(rr, r)
	if rr.Code != 204 {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
	if len(completer.sessions) != 0 {
		t.Fatalf("completer called with %v, want no calls", completer.sessions)
	}
}

// TestDispatch_CheckoutCompleted_CompleterError surfaces a provisioning
// error as a 500 so Stripe retries the webhook.
func TestDispatch_CheckoutCompleted_CompleterError(t *testing.T) {
	completer := &fakeSignupCompleter{err: errors.New("provision failed")}
	h := NewWebhookHandler(WebhookConfig{})
	h.SetSignupCompleter(completer)

	body := `{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":"cs_test_err"}}}`
	r := httptest.NewRequest("POST", "/api/v1/billing/webhooks/stripe", strings.NewReader(body))
	rr := httptest.NewRecorder()
	h.serve(rr, r)
	if rr.Code != 500 {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}
