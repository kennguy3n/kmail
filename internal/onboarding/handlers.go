package onboarding

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers wires onboarding routes.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers builds Handlers.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs the onboarding routes.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/onboarding", authMW.Wrap(http.HandlerFunc(h.get)))
	mux.Handle("POST /api/v1/tenants/{id}/onboarding/{stepId}/skip", authMW.Wrap(http.HandlerFunc(h.skip)))
	mux.Handle("POST /api/v1/tenants/{id}/onboarding/{stepId}/unskip", authMW.Wrap(http.HandlerFunc(h.unskip)))
	mux.Handle("POST /api/v1/tenants/{id}/onboarding/reset", authMW.Wrap(http.HandlerFunc(h.reset)))
}

func (h *Handlers) reset(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := h.svc.ResetChecklist(r.Context(), tenantID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out, err := h.svc.GetChecklist(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) skip(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	stepID := r.PathValue("stepId")
	if err := h.svc.SkipStep(r.Context(), tenantID, stepID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) unskip(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	stepID := r.PathValue("stepId")
	if err := h.svc.UnskipStep(r.Context(), tenantID, stepID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
