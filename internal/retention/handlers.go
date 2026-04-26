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
	worker *Worker
}

// NewHandlers returns Handlers.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// WithWorker wires the running worker so the status endpoint can
// report dry-run mode and the enforcement snapshot.
func (h *Handlers) WithWorker(w *Worker) *Handlers {
	h.worker = w
	return h
}

// Register installs the routes.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/retention", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/retention", authMW.Wrap(http.HandlerFunc(h.create)))
	mux.Handle("PUT /api/v1/tenants/{id}/retention/{policyId}", authMW.Wrap(http.HandlerFunc(h.update)))
	mux.Handle("DELETE /api/v1/tenants/{id}/retention/{policyId}", authMW.Wrap(http.HandlerFunc(h.delete)))
	mux.Handle("GET /api/v1/tenants/{id}/retention/status", authMW.Wrap(http.HandlerFunc(h.status)))
}

func (h *Handlers) status(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	out := map[string]any{
		"dry_run":         true,
		"recent_runs":     []any{},
		"emails_deleted":  int64(0),
		"emails_archived": int64(0),
		"errors":          int64(0),
	}
	if h.worker != nil {
		snap := h.worker.Snapshot()
		out["dry_run"] = snap.DryRun
		out["last_evaluated_at"] = snap.LastEvaluated
		out["emails_deleted"] = snap.EmailsDeleted
		out["emails_archived"] = snap.EmailsArchived
		out["errors"] = snap.Errors
	}
	runs, err := h.svc.RecentEnforcementRuns(r.Context(), tenantID, 20)
	if err == nil {
		out["recent_runs"] = runs
	}
	writeJSON(w, http.StatusOK, out)
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
