package billing

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the Billing / Quota Service over REST under
// `/api/v1/tenants/{id}/billing`.
type Handlers struct {
	svc       *Service
	lifecycle *Lifecycle
	logger    *log.Logger
}

// NewHandlers returns Handlers bound to the provided Service.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// WithLifecycle returns a copy of the Handlers wired to the given
// lifecycle helper. Required for the proration-preview and
// billing-history endpoints.
func (h *Handlers) WithLifecycle(lc *Lifecycle) *Handlers {
	cp := *h
	cp.lifecycle = lc
	return &cp
}

// Register installs every billing route onto the provided mux.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/billing", authMW.Wrap(http.HandlerFunc(h.summary)))
	mux.Handle("GET /api/v1/tenants/{id}/billing/usage", authMW.Wrap(http.HandlerFunc(h.usage)))
	mux.Handle("PATCH /api/v1/tenants/{id}/billing", authMW.Wrap(http.HandlerFunc(h.updateLimits)))
	mux.Handle("PATCH /api/v1/tenants/{id}/billing/plan", authMW.Wrap(http.HandlerFunc(h.changePlan)))
	mux.Handle("POST /api/v1/tenants/{id}/billing/invoice", authMW.Wrap(http.HandlerFunc(h.invoice)))
	mux.Handle("GET /api/v1/tenants/{id}/billing/proration-preview", authMW.Wrap(http.HandlerFunc(h.prorationPreview)))
	mux.Handle("GET /api/v1/tenants/{id}/billing/history", authMW.Wrap(http.HandlerFunc(h.history)))
}

func (h *Handlers) prorationPreview(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	plan := r.URL.Query().Get("plan")
	if err := ValidatePlan(plan); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if h.lifecycle == nil {
		writeJSON(w, http.StatusOK, map[string]any{"proration_cents": 0})
		return
	}
	cents, err := h.lifecycle.ProrationPreview(r.Context(), tenantID, plan)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id":       tenantID,
		"new_plan":        plan,
		"proration_cents": cents,
	})
}

func (h *Handlers) history(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if h.lifecycle == nil {
		writeJSON(w, http.StatusOK, []BillingHistoryEntry{})
		return
	}
	out, err := h.lifecycle.ListBillingHistory(r.Context(), tenantID, 50)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ChangePlanInput is the request body for PATCH .../billing/plan.
type ChangePlanInput struct {
	Plan string `json:"plan"`
}

func (h *Handlers) changePlan(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in ChangePlanInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// Capture old plan before the mutation so the lifecycle hook
	// can prorate. ChangePlan is idempotent on (oldPlan == newPlan)
	// so re-reading the plan column does not race the UPDATE.
	prev, _ := h.svc.Summary(r.Context(), tenantID)
	sum, err := h.svc.ChangePlan(r.Context(), tenantID, in.Plan)
	if err != nil {
		h.logger.Printf("billing.changePlan: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	if h.lifecycle != nil && prev != nil && prev.Plan != "" {
		if err := h.lifecycle.OnPlanChanged(r.Context(), tenantID, prev.Plan, in.Plan); err != nil {
			h.logger.Printf("billing.lifecycle.OnPlanChanged: %v", err)
		}
	}
	writeJSON(w, http.StatusOK, sum)
}

func (h *Handlers) summary(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	sum, err := h.svc.Summary(r.Context(), tenantID)
	if err != nil {
		h.logger.Printf("billing.summary: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (h *Handlers) usage(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	q, err := h.svc.GetQuota(r.Context(), tenantID)
	if errors.Is(err, ErrNotFound) {
		// Newly provisioned tenants have no quota row yet. Return
		// a zero-valued shell so the admin UI can still render the
		// "unprovisioned" state without a toast error.
		writeJSON(w, http.StatusOK, Quota{TenantID: tenantID})
		return
	}
	if err != nil {
		h.logger.Printf("billing.usage: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (h *Handlers) updateLimits(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in UpdateQuotaLimitsInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	q, err := h.svc.UpdateQuotaLimits(r.Context(), tenantID, in)
	if err != nil {
		h.logger.Printf("billing.updateLimits: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, q)
}

func (h *Handlers) invoice(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	cents, err := h.svc.CalculateInvoice(r.Context(), tenantID)
	if err != nil {
		h.logger.Printf("billing.invoice: %v", err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenant_id":     tenantID,
		"amount_cents":  cents,
		"currency":      "USD",
	})
}

// statusFor translates a Service error into the HTTP status the
// mutation handlers should return.
func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrQuotaExceeded):
		return http.StatusPaymentRequired
	default:
		return http.StatusInternalServerError
	}
}

func checkTenantScope(r *http.Request, pathTenantID string) error {
	ctxTenantID := middleware.TenantIDFrom(r.Context())
	if ctxTenantID == "" {
		return errors.New("missing tenant context")
	}
	if ctxTenantID != pathTenantID {
		return errors.New("cross-tenant access forbidden")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
