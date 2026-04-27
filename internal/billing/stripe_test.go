package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newMockStripe(t *testing.T) (*StripeClient, *httptest.Server, *[]string) {
	t.Helper()
	calls := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/customers":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "cus_test", "email": r.Form.Get("email")})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/subscriptions":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "sub_test", "customer": r.Form.Get("customer"), "status": "active"})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/"), "status": "active"})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/subscriptions/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/v1/subscriptions/"), "status": "canceled"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewStripeClient("sk_test_xxx")
	c.BaseURL = srv.URL
	return c, srv, &calls
}

func TestStripeCreateCustomerAndSubscription(t *testing.T) {
	c, _, calls := newMockStripe(t)
	cust, err := c.CreateCustomer(context.Background(), "billing@example.com", map[string]string{"kmail_tenant_id": "t-1"})
	if err != nil {
		t.Fatalf("CreateCustomer: %v", err)
	}
	if cust.ID != "cus_test" {
		t.Fatalf("customer ID = %q", cust.ID)
	}
	sub, err := c.CreateSubscription(context.Background(), SubscriptionRequest{
		Customer: cust.ID,
		PriceID:  "price_pro",
		Quantity: 1,
		Metadata: map[string]string{"kmail_tenant_id": "t-1"},
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if sub.ID != "sub_test" {
		t.Fatalf("subscription ID = %q", sub.ID)
	}
	if got, want := len(*calls), 2; got != want {
		t.Fatalf("calls = %v, want %d", *calls, want)
	}
}

func TestStripeUpdateSubscriptionMetadataOnly(t *testing.T) {
	c, _, _ := newMockStripe(t)
	if _, err := c.UpdateSubscription(context.Background(), "sub_x", SubscriptionRequest{
		Metadata: map[string]string{"kmail_plan": "pro"},
	}); err != nil {
		t.Fatalf("UpdateSubscription: %v", err)
	}
}

func TestStripeUpdateSubscriptionRequiresItemIDForPriceChanges(t *testing.T) {
	c, _, _ := newMockStripe(t)
	_, err := c.UpdateSubscription(context.Background(), "sub_x", SubscriptionRequest{PriceID: "price_pro"})
	if err == nil || !strings.Contains(err.Error(), "ItemID") {
		t.Fatalf("expected ItemID-required error, got %v", err)
	}
}

func TestStripeCancelSubscription(t *testing.T) {
	c, _, _ := newMockStripe(t)
	sub, err := c.CancelSubscription(context.Background(), "sub_x")
	if err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}
	if sub.Status != "canceled" {
		t.Fatalf("status = %q", sub.Status)
	}
}

func TestStripeUnconfiguredReturnsErr(t *testing.T) {
	c := NewStripeClient("")
	if _, err := c.CreateCustomer(context.Background(), "x@example.com", nil); err == nil {
		t.Fatal("expected ErrStripeUnconfigured")
	}
	if _, err := c.CreateSubscription(context.Background(), SubscriptionRequest{}); err == nil {
		t.Fatal("expected ErrStripeUnconfigured")
	}
	if _, err := c.UpdateSubscription(context.Background(), "x", SubscriptionRequest{}); err == nil {
		t.Fatal("expected ErrStripeUnconfigured")
	}
	if _, err := c.CancelSubscription(context.Background(), "x"); err == nil {
		t.Fatal("expected ErrStripeUnconfigured")
	}
}
