package search

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// CutoverHandlers exposes the operator-facing cutover surface under
// `/api/v1/tenants/{id}/search/cutover`. It is a thin REST shell
// over CutoverService: the admin UI reads a tenant's cutover
// history and (optionally) triggers a manual cutover that runs the
// same export→reindex→validate→flip→mark dance as the auto-worker.
type CutoverHandlers struct {
	svc    *CutoverService
	logger *log.Logger
}

// NewCutoverHandlers binds the REST handlers to a CutoverService.
func NewCutoverHandlers(svc *CutoverService, logger *log.Logger) *CutoverHandlers {
	if logger == nil {
		logger = log.Default()
	}
	return &CutoverHandlers{svc: svc, logger: logger}
}

// Register installs the cutover routes. Both are tenant-scoped and
// wrapped in the OIDC middleware, mirroring the search Handlers.
func (h *CutoverHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/search/cutover", authMW.Wrap(http.HandlerFunc(h.listJobs)))
	mux.Handle("POST /api/v1/tenants/{id}/search/cutover", authMW.Wrap(http.HandlerFunc(h.initiate)))
}

// listJobs returns the tenant's cutover history across all targets.
func (h *CutoverHandlers) listJobs(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	jobs, err := h.svc.ListCutoverJobs(r.Context(), tenantID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	// Normalise nil to an empty slice so the client always sees a
	// JSON array rather than `null`.
	if jobs == nil {
		jobs = []CutoverJob{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

type initiateCutoverRequest struct {
	TargetBackend string `json:"target_backend"`
}

// initiate records operator intent and synchronously runs the
// cutover, returning the resulting job row. The synchronous model
// keeps the admin UX simple — the button blocks with a progress
// indicator and the response carries the terminal state — while the
// underlying store row stays consistent with the auto-worker (a
// failure leaves the tenant on source, a success flips the backend).
func (h *CutoverHandlers) initiate(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var req initiateCutoverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	job, err := h.svc.InitiateCutover(r.Context(), tenantID, req.TargetBackend)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	// If a cutover is already running for this (tenant, target),
	// UpsertPending leaves the in_progress row untouched. Short-
	// circuit with 409 instead of attempting a Claim that's
	// guaranteed to lose the race — and avoid the confusing
	// "started then immediately conflicted" UX.
	if job.State == CutoverInProgress {
		writeError(w, http.StatusConflict, ErrCutoverInProgress)
		return
	}
	actorID := middleware.KChatUserIDFrom(r.Context())
	if err := h.svc.ExecuteCutover(r.Context(), tenantID, req.TargetBackend, actorID); err != nil {
		// A validation/reindex failure is a real, expected
		// operator-facing outcome: the job row is now `failed` and
		// the tenant is still safely on source. We return just the
		// error status here; the admin UI refreshes the history table
		// (a GET) from its catch handler to pick up the now-`failed`
		// row, so the failed terminal state is never lost.
		status := statusFor(err)
		if errors.Is(err, ErrCutoverInProgress) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	job, err = h.svc.GetCutoverJob(r.Context(), tenantID, req.TargetBackend)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}
