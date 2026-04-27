package dns

import (
	"errors"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// DKIMHandlers exposes the DKIM rotation service over REST under
// `/api/v1/tenants/{id}/domains/{domainId}/dkim`.
type DKIMHandlers struct {
	svc    *DKIMRotationService
	logger *log.Logger
}

// NewDKIMHandlers returns DKIM Handlers bound to the provided
// service.
func NewDKIMHandlers(svc *DKIMRotationService, logger *log.Logger) *DKIMHandlers {
	if logger == nil {
		logger = log.Default()
	}
	return &DKIMHandlers{svc: svc, logger: logger}
}

// Register installs the DKIM routes.
func (h *DKIMHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/domains/{domainId}/dkim", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/domains/{domainId}/dkim", authMW.Wrap(http.HandlerFunc(h.rotate)))
	mux.Handle("POST /api/v1/tenants/{id}/domains/{domainId}/dkim/{keyId}/rotate", authMW.Wrap(http.HandlerFunc(h.rotate)))
	mux.Handle("DELETE /api/v1/tenants/{id}/domains/{domainId}/dkim/{keyId}", authMW.Wrap(http.HandlerFunc(h.revoke)))
}

func (h *DKIMHandlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	domainID := r.PathValue("domainId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	keys, err := h.svc.ListKeys(r.Context(), tenantID, domainID)
	if err != nil {
		writeError(w, statusForDKIM(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (h *DKIMHandlers) rotate(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	domainID := r.PathValue("domainId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	key, err := h.svc.RotateKey(r.Context(), tenantID, domainID)
	if err != nil {
		writeError(w, statusForDKIM(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, key)
}

func (h *DKIMHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	domainID := r.PathValue("domainId")
	keyID := r.PathValue("keyId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := h.svc.RevokeKey(r.Context(), tenantID, domainID, keyID); err != nil {
		writeError(w, statusForDKIM(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func statusForDKIM(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
