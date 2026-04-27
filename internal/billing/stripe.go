// Package billing — Stripe REST client.
//
// The Phase 4 webhook handler in `webhook.go` reacted to inbound
// Stripe events. Phase 7 closes the loop in the outbound
// direction: a `StripeClient` that the BFF calls to create /
// cancel / update subscriptions and to mint Stripe Customer
// Portal sessions for self-service billing changes.
//
// We intentionally avoid the official `stripe-go` SDK and build a
// thin REST shim instead, mirroring the existing webhook code's
// approach. Reasons: (1) keeps the Go module graph small, (2)
// matches the existing pattern of hand-written upstream clients
// (DNS, KChat), (3) makes the wire calls easy to mock in unit
// tests with `httptest.NewServer`.
//
// The client is gated by `Config.StripeSecretKey`. When that env
// var is empty the client returns `ErrStripeUnconfigured` and
// the BFF falls back to the existing stub-mode billing surface.
package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrStripeUnconfigured is returned by every StripeClient method
// when the secret key is empty. Callers translate it into the
// existing "billing in stub mode" branch.
var ErrStripeUnconfigured = errors.New("stripe: not configured")

// StripeClient talks to api.stripe.com over REST.
type StripeClient struct {
	APIKey     string
	BaseURL    string
	HTTPClient *http.Client
}

// NewStripeClient returns a client wired to api.stripe.com (or
// the override BaseURL for tests). When apiKey is empty every
// method returns ErrStripeUnconfigured.
func NewStripeClient(apiKey string) *StripeClient {
	return &StripeClient{
		APIKey:     apiKey,
		BaseURL:    "https://api.stripe.com",
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Configured returns true if APIKey is set.
func (c *StripeClient) Configured() bool { return c != nil && c.APIKey != "" }

// SubscriptionRequest is the input shape for CreateSubscription /
// UpdateSubscription. Only the fields KMail uses are surfaced.
type SubscriptionRequest struct {
	Customer string
	PriceID  string
	Quantity int
	Metadata map[string]string
}

// SubscriptionResult is the trimmed shape returned by Stripe.
type SubscriptionResult struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
	Status   string `json:"status"`
	Created  int64  `json:"created"`
}

// PortalSessionResult is the trimmed shape from
// `/v1/billing_portal/sessions`.
type PortalSessionResult struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// CreateSubscription POSTs to /v1/subscriptions.
func (c *StripeClient) CreateSubscription(ctx context.Context, req SubscriptionRequest) (*SubscriptionResult, error) {
	if !c.Configured() {
		return nil, ErrStripeUnconfigured
	}
	form := url.Values{}
	form.Set("customer", req.Customer)
	form.Set("items[0][price]", req.PriceID)
	if req.Quantity > 0 {
		form.Set("items[0][quantity]", fmt.Sprintf("%d", req.Quantity))
	}
	for k, v := range req.Metadata {
		form.Set("metadata["+k+"]", v)
	}
	var out SubscriptionResult
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions", form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateSubscription POSTs to /v1/subscriptions/:id with the
// fields it can find in req.
func (c *StripeClient) UpdateSubscription(ctx context.Context, id string, req SubscriptionRequest) (*SubscriptionResult, error) {
	if !c.Configured() {
		return nil, ErrStripeUnconfigured
	}
	if id == "" {
		return nil, errors.New("stripe: subscription id required")
	}
	form := url.Values{}
	if req.PriceID != "" {
		form.Set("items[0][price]", req.PriceID)
	}
	if req.Quantity > 0 {
		form.Set("items[0][quantity]", fmt.Sprintf("%d", req.Quantity))
	}
	for k, v := range req.Metadata {
		form.Set("metadata["+k+"]", v)
	}
	var out SubscriptionResult
	if err := c.do(ctx, http.MethodPost, "/v1/subscriptions/"+url.PathEscape(id), form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelSubscription DELETEs /v1/subscriptions/:id.
func (c *StripeClient) CancelSubscription(ctx context.Context, id string) (*SubscriptionResult, error) {
	if !c.Configured() {
		return nil, ErrStripeUnconfigured
	}
	if id == "" {
		return nil, errors.New("stripe: subscription id required")
	}
	var out SubscriptionResult
	if err := c.do(ctx, http.MethodDelete, "/v1/subscriptions/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreatePortalSession POSTs to /v1/billing_portal/sessions and
// returns the customer-facing URL the UI should redirect to.
func (c *StripeClient) CreatePortalSession(ctx context.Context, customer, returnURL string) (*PortalSessionResult, error) {
	if !c.Configured() {
		return nil, ErrStripeUnconfigured
	}
	if customer == "" {
		return nil, errors.New("stripe: customer required")
	}
	form := url.Values{}
	form.Set("customer", customer)
	if returnURL != "" {
		form.Set("return_url", returnURL)
	}
	var out PortalSessionResult
	if err := c.do(ctx, http.MethodPost, "/v1/billing_portal/sessions", form, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// do executes a single request against the Stripe API. For POST /
// PUT / DELETE with form data we use `application/x-www-form-urlencoded`
// matching Stripe's idiomatic shape (Stripe accepts a body on
// DELETE for fields like `cancel_at_period_end`). GETs pass form
// as a query string.
func (c *StripeClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	endpoint := strings.TrimRight(c.BaseURL, "/") + path
	var body io.Reader
	if form != nil && (method == http.MethodPost || method == http.MethodPut || method == http.MethodDelete) {
		body = strings.NewReader(form.Encode())
	} else if form != nil {
		endpoint += "?" + form.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.APIKey, "")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: do: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode >= 400 {
		var apiErr struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(respBody, &apiErr)
		if apiErr.Error.Message != "" {
			return fmt.Errorf("stripe: %d %s/%s: %s", resp.StatusCode, apiErr.Error.Type, apiErr.Error.Code, apiErr.Error.Message)
		}
		return fmt.Errorf("stripe: %d %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("stripe: decode: %w", err)
		}
	}
	return nil
}
