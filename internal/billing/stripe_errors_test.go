package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStripeDoErrorPaths exercises the do() error handling: a 4xx with
// a structured Stripe error body, a 4xx with a plain body, and a decode
// failure on a 2xx response.
func TestStripeDoErrorPaths(t *testing.T) {
	t.Run("structured api error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"resource_missing","message":"No such customer"}}`))
		}))
		defer srv.Close()
		c := NewStripeClient("sk_test")
		c.BaseURL = srv.URL
		_, err := c.CreateCustomer(context.Background(), "x@example.com", nil)
		if err == nil || !strings.Contains(err.Error(), "No such customer") {
			t.Fatalf("expected structured error, got %v", err)
		}
	})

	t.Run("plain error body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("upstream boom"))
		}))
		defer srv.Close()
		c := NewStripeClient("sk_test")
		c.BaseURL = srv.URL
		_, err := c.CreateSubscription(context.Background(), SubscriptionRequest{Customer: "cus", PriceID: "price"})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("expected plain error, got %v", err)
		}
	})

	t.Run("decode failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("not json"))
		}))
		defer srv.Close()
		c := NewStripeClient("sk_test")
		c.BaseURL = srv.URL
		_, err := c.CreateCustomer(context.Background(), "x@example.com", nil)
		if err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("expected decode error, got %v", err)
		}
	})
}

func TestCreatePortalSession(t *testing.T) {
	c := newPortalMockStripe(t)
	out, err := c.CreatePortalSession(context.Background(), "cus_x", "https://return.test")
	if err != nil {
		t.Fatalf("CreatePortalSession: %v", err)
	}
	if out.URL == "" {
		t.Error("empty portal URL")
	}

	// Empty customer is rejected before any HTTP call.
	if _, err := c.CreatePortalSession(context.Background(), "", ""); err == nil {
		t.Error("empty customer should error")
	}

	// Unconfigured client returns ErrStripeUnconfigured.
	if _, err := NewStripeClient("").CreatePortalSession(context.Background(), "cus", ""); err == nil {
		t.Error("unconfigured should error")
	}
}
