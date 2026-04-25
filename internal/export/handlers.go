package export

import (
	"encoding/json"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers wires the export endpoints.
type Handlers struct {
	svc *Service
}

// NewHandlers returns Handlers.
func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// Register installs the routes.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/exports", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/exports", authMW.Wrap(http.HandlerFunc(h.create)))
	mux.Handle("GET /api/v1/tenants/{id}/exports/{jobId}", authMW.Wrap(http.HandlerFunc(h.get)))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out, err := h.svc.ListExportJobs(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = []Job{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var in struct {
		RequesterID string `json:"requester_id"`
		Format      string `json:"format"`
		Scope       string `json:"scope"`
		ScopeRef    string `json:"scope_ref"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	out, err := h.svc.CreateExportJob(r.Context(), tenantID, in.RequesterID, in.Format, in.Scope, in.ScopeRef)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	id := r.PathValue("jobId")
	out, err := h.svc.GetExportJob(r.Context(), tenantID, id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
