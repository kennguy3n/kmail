package billing

import "testing"

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
