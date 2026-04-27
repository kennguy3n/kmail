package sieve

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the Sieve service over REST under
// `/api/v1/tenants/{id}/sieve-rules`.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers returns Handlers bound to the provided service.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs every Sieve route onto the provided mux.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/sieve-rules", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/sieve-rules", authMW.Wrap(http.HandlerFunc(h.create)))
	mux.Handle("PUT /api/v1/tenants/{id}/sieve-rules/{ruleId}", authMW.Wrap(http.HandlerFunc(h.update)))
	mux.Handle("DELETE /api/v1/tenants/{id}/sieve-rules/{ruleId}", authMW.Wrap(http.HandlerFunc(h.delete)))
	mux.Handle("POST /api/v1/tenants/{id}/sieve-rules/validate", authMW.Wrap(http.HandlerFunc(h.validate)))
	mux.Handle("POST /api/v1/tenants/{id}/sieve-rules/deploy", authMW.Wrap(http.HandlerFunc(h.deploy)))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	rules, err := h.svc.ListRules(r.Context(), tenantID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in Rule
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.TenantID = tenantID
	out, err := h.svc.CreateRule(r.Context(), in)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) update(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	ruleID := r.PathValue("ruleId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in Rule
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	in.TenantID = tenantID
	in.ID = ruleID
	out, err := h.svc.UpdateRule(r.Context(), in)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	ruleID := r.PathValue("ruleId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := h.svc.DeleteRule(r.Context(), tenantID, ruleID); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) validate(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var req struct {
		Script string `json:"script"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := h.svc.ValidateScript(req.Script); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": true})
}

func (h *Handlers) deploy(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := h.svc.DeployRules(r.Context(), tenantID); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "deployed"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
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
