package retention

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers wires the retention admin endpoints.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers returns Handlers.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs the routes.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/retention", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/retention", authMW.Wrap(http.HandlerFunc(h.create)))
	mux.Handle("PUT /api/v1/tenants/{id}/retention/{policyId}", authMW.Wrap(http.HandlerFunc(h.update)))
	mux.Handle("DELETE /api/v1/tenants/{id}/retention/{policyId}", authMW.Wrap(http.HandlerFunc(h.delete)))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out, err := h.svc.ListPolicies(r.Context(), tenantID)
	if err != nil {
		h.fail(w, http.StatusInternalServerError, err)
		return
	}
	if out == nil {
		out = []Policy{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var p Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		h.fail(w, http.StatusBadRequest, err)
		return
	}
	p.TenantID = tenantID
	out, err := h.svc.CreatePolicy(r.Context(), p)
	if err != nil {
		h.fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) update(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var p Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		h.fail(w, http.StatusBadRequest, err)
		return
	}
	p.TenantID = tenantID
	p.ID = r.PathValue("policyId")
	out, err := h.svc.UpdatePolicy(r.Context(), p)
	if err != nil {
		h.fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	id := r.PathValue("policyId")
	if err := h.svc.DeletePolicy(r.Context(), tenantID, id); err != nil {
		h.fail(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) fail(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
