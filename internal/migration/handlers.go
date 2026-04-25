package migration

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers is the HTTP surface for the Migration Orchestrator. All
// routes are tenant-scoped via the OIDC middleware; the handlers
// read the authenticated tenant out of the request context (they
// never accept a tenant ID in the URL or body).
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers wires a Service into an http.Handler group.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs every migration route on the provided mux. The
// mux is the Go 1.22+ `http.ServeMux`, which understands method +
// path patterns natively.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/migrations",
		authMW.Wrap(http.HandlerFunc(h.createJob)))
	mux.Handle("POST /api/v1/migrations/test-connection",
		authMW.Wrap(http.HandlerFunc(h.testConnection)))
	mux.Handle("GET /api/v1/migrations",
		authMW.Wrap(http.HandlerFunc(h.listJobs)))
	mux.Handle("GET /api/v1/migrations/{jobId}",
		authMW.Wrap(http.HandlerFunc(h.getJob)))
	mux.Handle("DELETE /api/v1/migrations/{jobId}",
		authMW.Wrap(http.HandlerFunc(h.cancelJob)))
	mux.Handle("POST /api/v1/migrations/{jobId}/pause",
		authMW.Wrap(http.HandlerFunc(h.pauseJob)))
	mux.Handle("POST /api/v1/migrations/{jobId}/resume",
		authMW.Wrap(http.HandlerFunc(h.resumeJob)))
}

func (h *Handlers) pauseJob(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	jobID := r.PathValue("jobId")
	if err := h.svc.PauseJob(r.Context(), tenantID, jobID); err != nil {
		h.logger.Printf("pauseJob: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) resumeJob(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	jobID := r.PathValue("jobId")
	if err := h.svc.ResumeJob(r.Context(), tenantID, jobID); err != nil {
		h.logger.Printf("resumeJob: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// createJob validates + inserts a new job, then immediately calls
// StartJob so the caller does not need a second round-trip to
// actually kick off the import. Returns 202 with the persisted
// job row (password omitted via the `json:"-"` tag on the struct).
func (h *Handlers) createJob(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	var in CreateJobInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	job, err := h.svc.CreateJob(r.Context(), tenantID, in)
	if err != nil {
		h.logger.Printf("createJob: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	if err := h.svc.StartJob(r.Context(), tenantID, job.ID); err != nil {
		// The row is already persisted; surface the start failure
		// but leave the row in `pending` so the operator can retry
		// via DELETE + re-create.
		h.logger.Printf("createJob: start failed for %s: %v", job.ID, err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (h *Handlers) listJobs(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	jobs, err := h.svc.ListJobs(r.Context(), tenantID)
	if err != nil {
		h.logger.Printf("listJobs: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	if jobs == nil {
		jobs = []*MigrationJob{}
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (h *Handlers) getJob(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	jobID := r.PathValue("jobId")
	job, err := h.svc.GetJob(r.Context(), tenantID, jobID)
	if err != nil {
		h.logger.Printf("getJob: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// testConnection runs a lightweight IMAP LOGIN against the supplied
// host/port/credentials and surfaces the result so the migration
// wizard can validate input before posting createJob. The handler is
// tenant-scoped via OIDC but does not record any persistent state —
// it is a pure side-channel probe.
func (h *Handlers) testConnection(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	var in TestConnectionInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := h.svc.TestConnection(r.Context(), in); err != nil {
		// Surface the error message but as a 200 so the UI can
		// render a red banner without a network-error toast: the
		// HTTP layer worked fine, the IMAP login did not.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handlers) cancelJob(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	jobID := r.PathValue("jobId")
	if err := h.svc.CancelJob(r.Context(), tenantID, jobID); err != nil {
		h.logger.Printf("cancelJob: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------
// JSON / error helpers. Intentionally a copy of the tenant / dns
// helpers so this package has no dependency on either of them.
// ------------------------------------------------------------------

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// statusForServiceError maps package-level errors to HTTP status
// codes. Kept in sync with the equivalent helpers in
// internal/tenant and internal/dns.
func statusForServiceError(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}
