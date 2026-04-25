package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMapStripeStatus locks in the translation between Stripe's
// status vocabulary and the three-value enum the
// `billing_subscriptions.status` CHECK constraint allows. Drift
// here would re-introduce the infinite Stripe-retry loop fixed in
// PR #21.
func TestMapStripeStatus(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"active", "active"},
		{"trialing", "active"},
		{"past_due", "past_due"},
		{"unpaid", "past_due"},
		{"incomplete", "past_due"},
		{"incomplete_expired", "past_due"},
		{"canceled", "cancelled"},
		{"cancelled", "cancelled"},
		{"", "active"},
	}
	for _, c := range cases {
		if got := mapStripeStatus(c.in); got != c.want {
			t.Errorf("mapStripeStatus(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// signStripe returns a Stripe-Signature header value computed over
// `t.body` using `secret`, mimicking the Stripe SDK derivation.
func signStripe(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", ts, string(body))))
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// TestVerify_RejectsReplayedSignature ensures a signed payload
// captured outside the 5-minute replay window is rejected even
// though the HMAC still matches. Without this check an attacker
// who captures a valid `payment_intent.succeeded` could re-trigger
// it indefinitely.
func TestVerify_RejectsReplayedSignature(t *testing.T) {
	secret := "whsec_test"
	body := []byte(`{"id":"evt_1","type":"invoice.paid"}`)
	now := time.Now()
	h := NewWebhookHandler(WebhookConfig{
		StripeWebhookSecret: secret,
		Now:                 func() time.Time { return now },
	})

	// Fresh signature — accepted.
	r := httptest.NewRequest("POST", "/api/v1/billing/webhooks/stripe", strings.NewReader(""))
	r.Header.Set("Stripe-Signature", signStripe(secret, now.Unix(), body))
	if err := h.verify(r, body); err != nil {
		t.Fatalf("fresh signature: %v", err)
	}

	// Stale signature — rejected.
	stale := now.Add(-10 * time.Minute).Unix()
	r2 := httptest.NewRequest("POST", "/api/v1/billing/webhooks/stripe", strings.NewReader(""))
	r2.Header.Set("Stripe-Signature", signStripe(secret, stale, body))
	if err := h.verify(r2, body); err == nil {
		t.Fatal("stale signature accepted, want rejection")
	}

	// Future-dated signature beyond the window — also rejected
	// (clock-skew tolerance is symmetric).
	future := now.Add(10 * time.Minute).Unix()
	r3 := httptest.NewRequest("POST", "/api/v1/billing/webhooks/stripe", strings.NewReader(""))
	r3.Header.Set("Stripe-Signature", signStripe(secret, future, body))
	if err := h.verify(r3, body); err == nil {
		t.Fatal("future signature accepted, want rejection")
	}
}
