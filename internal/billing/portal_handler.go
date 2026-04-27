package billing

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// PortalHandlers wires the `/billing/portal` endpoint that mints a
// Stripe Customer Portal session for the calling tenant. Lives in
// its own file so the existing `handlers.go` Service-only surface
// stays uncluttered.
type PortalHandlers struct {
	pool   *pgxpool.Pool
	stripe *StripeClient
	logger *log.Logger
}

// NewPortalHandlers returns a PortalHandlers bound to the given
// pool and Stripe client. The pool is consulted for the tenant's
// `stripe_customer_id`; if that column is empty the handler
// returns a 404 so the UI knows to fall through to the existing
// stub-mode billing surface.
func NewPortalHandlers(pool *pgxpool.Pool, stripe *StripeClient, logger *log.Logger) *PortalHandlers {
	if logger == nil {
		logger = log.Default()
	}
	return &PortalHandlers{pool: pool, stripe: stripe, logger: logger}
}

// Register installs the portal route. The caller passes the same
// authMW used for the rest of the billing surface.
func (h *PortalHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/tenants/{id}/billing/portal", authMW.Wrap(http.HandlerFunc(h.create)))
}

type portalRequest struct {
	ReturnURL string `json:"return_url"`
}

func (h *PortalHandlers) create(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var req portalRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if h.stripe == nil || !h.stripe.Configured() {
		writeError(w, http.StatusServiceUnavailable, ErrStripeUnconfigured)
		return
	}
	customer, err := h.lookupCustomer(r.Context(), tenantID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if customer == "" {
		writeError(w, http.StatusNotFound, errors.New("billing: tenant has no stripe customer"))
		return
	}
	session, err := h.stripe.CreatePortalSession(r.Context(), customer, req.ReturnURL)
	if err != nil {
		h.logger.Printf("portal session create for tenant=%s: %v", tenantID, err)
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// lookupCustomer reads `billing_subscriptions.stripe_customer_id`
// for the tenant. Returns "" when no row exists.
func (h *PortalHandlers) lookupCustomer(ctx context.Context, tenantID string) (string, error) {
	if h.pool == nil {
		return "", nil
	}
	var customer string
	err := pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
			SELECT COALESCE(stripe_customer_id, '')
			  FROM billing_subscriptions
			 WHERE tenant_id = $1::uuid
			 ORDER BY created_at DESC LIMIT 1`, tenantID)
		return row.Scan(&customer)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return customer, nil
}
