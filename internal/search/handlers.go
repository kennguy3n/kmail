package search

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the search admin surface over REST under
// `/api/v1/tenants/{id}/search/...`.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers returns search Handlers bound to the provided Service.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs every route onto the provided mux. All routes
// are wrapped in the OIDC middleware so handlers can assume an
// authenticated request context.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/search/backend", authMW.Wrap(http.HandlerFunc(h.getBackend)))
	mux.Handle("PUT /api/v1/tenants/{id}/search/backend", authMW.Wrap(http.HandlerFunc(h.putBackend)))
	mux.Handle("POST /api/v1/tenants/{id}/search/reindex", authMW.Wrap(http.HandlerFunc(h.reindex)))
}

func (h *Handlers) getBackend(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	backend, err := h.svc.GetBackend(r.Context(), tenantID)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"backend": backend})
}

type backendRequest struct {
	Backend string `json:"backend"`
}

func (h *Handlers) putBackend(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var req backendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := h.svc.SetBackend(r.Context(), tenantID, req.Backend); err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"backend": req.Backend})
}

func (h *Handlers) reindex(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	// Phase 7 ships the orchestration handler with an empty
	// message slice — the production reindex pulls messages out
	// of Stalwart in batches via the JMAP proxy. The handler is
	// idempotent: it deletes the existing index even if no
	// messages are passed in so a stale index can be cleared.
	if err := h.svc.Reindex(r.Context(), tenantID, nil); err != nil {
		h.logger.Printf("reindex %s: %v", tenantID, err)
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "queued"})
}

// helpers shared with other admin handlers.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
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

// checkTenantScope mirrors the helpers in the other admin
// packages. Cross-tenant access is blocked at both the RLS and
// HTTP layers.
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
