// Package billing — Stripe webhook stub.
//
// Phase 4 closes the "tenant-level billing integration" loop with a
// minimal Stripe webhook endpoint so the BFF can react to off-band
// payment events. The handler accepts the three event shapes the AR
// team flagged as MVP-critical (`payment_intent.succeeded`,
// `invoice.paid`, `customer.subscription.updated`) and updates the
// `billing_subscriptions` status accordingly.
//
// Signature verification is a stub: the handler accepts an
// `Stripe-Signature` header and re-checks it against
// `Config.StripeWebhookSecret` using HMAC-SHA256 when the secret is
// configured. Empty secret = dev mode = accept everything (parity
// with the OIDC dev-bypass token). Production deployments MUST set
// the secret via env.

package billing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// WebhookConfig wires the Stripe webhook handler.
type WebhookConfig struct {
	Lifecycle           *Lifecycle
	StripeWebhookSecret string
	Logger              *log.Logger
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// WebhookHandler is the Stripe webhook receiver.
type WebhookHandler struct {
	cfg WebhookConfig
}

// NewWebhookHandler returns a WebhookHandler with sensible defaults.
func NewWebhookHandler(cfg WebhookConfig) *WebhookHandler {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &WebhookHandler{cfg: cfg}
}

// Register mounts the webhook on the BFF mux. Intentionally
// unauthenticated — Stripe webhooks are signed, not bearer-authed.
func (h *WebhookHandler) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/billing/webhooks/stripe", http.HandlerFunc(h.serve))
}

type stripeEvent struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

func (h *WebhookHandler) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.verify(r, body); err != nil {
		h.cfg.Logger.Printf("billing.webhook: signature: %v", err)
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	var ev stripeEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.dispatch(r, ev); err != nil {
		h.cfg.Logger.Printf("billing.webhook %s: %v", ev.Type, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *WebhookHandler) dispatch(r *http.Request, ev stripeEvent) error {
	if h.cfg.Lifecycle == nil {
		return nil
	}
	switch ev.Type {
	case "payment_intent.succeeded", "invoice.paid":
		return h.markActive(r, ev)
	case "invoice.payment_failed":
		return h.markPastDue(r, ev)
	case "customer.subscription.updated", "customer.subscription.deleted":
		return h.applySubscriptionUpdate(r, ev)
	default:
		// Unknown event types are accepted to keep Stripe's retry
		// behavior happy — we'll add handlers as we need them.
		return nil
	}
}

type paymentObject struct {
	Metadata map[string]string `json:"metadata"`
}

func tenantIDFromObject(raw json.RawMessage) string {
	var obj paymentObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	return obj.Metadata["tenant_id"]
}

func (h *WebhookHandler) markActive(r *http.Request, ev stripeEvent) error {
	tenantID := tenantIDFromObject(ev.Data.Object)
	if tenantID == "" {
		return nil
	}
	pool := h.cfg.Lifecycle.svc.cfg.Pool
	if pool == nil {
		return nil
	}
	_, err := pool.Exec(r.Context(), `
		UPDATE billing_subscriptions SET status = 'active'
		WHERE tenant_id = $1::uuid
	`, tenantID)
	return err
}

func (h *WebhookHandler) markPastDue(r *http.Request, ev stripeEvent) error {
	tenantID := tenantIDFromObject(ev.Data.Object)
	if tenantID == "" {
		return nil
	}
	pool := h.cfg.Lifecycle.svc.cfg.Pool
	if pool == nil {
		return nil
	}
	_, err := pool.Exec(r.Context(), `
		UPDATE billing_subscriptions SET status = 'past_due'
		WHERE tenant_id = $1::uuid
	`, tenantID)
	return err
}

type subscriptionObject struct {
	ID                 string `json:"id"`
	Status             string `json:"status"`
	Metadata           map[string]string `json:"metadata"`
	CurrentPeriodStart int64  `json:"current_period_start"`
	CurrentPeriodEnd   int64  `json:"current_period_end"`
}

func (h *WebhookHandler) applySubscriptionUpdate(r *http.Request, ev stripeEvent) error {
	var sub subscriptionObject
	if err := json.Unmarshal(ev.Data.Object, &sub); err != nil {
		return err
	}
	tenantID := sub.Metadata["tenant_id"]
	if tenantID == "" {
		return nil
	}
	pool := h.cfg.Lifecycle.svc.cfg.Pool
	if pool == nil {
		return nil
	}
	status := sub.Status
	if status == "" {
		status = "active"
	}
	if !validSubscriptionStatus(status) {
		return fmt.Errorf("unknown stripe status %q", status)
	}
	periodStart := time.Unix(sub.CurrentPeriodStart, 0).UTC()
	periodEnd := time.Unix(sub.CurrentPeriodEnd, 0).UTC()
	_, err := pool.Exec(r.Context(), `
		UPDATE billing_subscriptions
		SET status = $2,
		    stripe_subscription_id = $3,
		    current_period_start = $4,
		    current_period_end = $5
		WHERE tenant_id = $1::uuid
	`, tenantID, status, sub.ID, periodStart, periodEnd)
	return err
}

func validSubscriptionStatus(s string) bool {
	switch s {
	case "active", "trialing":
		return true
	case "past_due", "unpaid":
		return true
	case "canceled", "cancelled", "incomplete", "incomplete_expired":
		return true
	}
	return false
}

// verify checks the Stripe-Signature header against the configured
// webhook secret using the documented HMAC-SHA256 derivation. Empty
// secret = dev mode = always accept (parity with the OIDC dev-bypass
// token). Production deployments MUST set the secret via env.
func (h *WebhookHandler) verify(r *http.Request, body []byte) error {
	if h.cfg.StripeWebhookSecret == "" {
		return nil
	}
	header := r.Header.Get("Stripe-Signature")
	if header == "" {
		return errors.New("missing Stripe-Signature header")
	}
	var t, v1 string
	for _, kv := range strings.Split(header, ",") {
		parts := strings.SplitN(strings.TrimSpace(kv), "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "t":
			t = parts[1]
		case "v1":
			v1 = parts[1]
		}
	}
	if t == "" || v1 == "" {
		return errors.New("malformed Stripe-Signature header")
	}
	mac := hmac.New(sha256.New, []byte(h.cfg.StripeWebhookSecret))
	mac.Write([]byte(t + "." + string(body)))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(v1)) {
		return errors.New("signature mismatch")
	}
	return nil
}
