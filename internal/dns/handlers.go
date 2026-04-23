package dns

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the DNS Onboarding Service over REST under
// `/api/v1/tenants/{id}/domains/{domainId}/...`. The handlers are
// thin: they validate tenant scope, delegate to Service, and
// marshal the result.
//
// The handlers live alongside the DNS service rather than in the
// Tenant Service package so the DNS wizard can evolve
// independently of tenant CRUD.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers returns DNS Handlers bound to the provided Service.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs every DNS route onto the provided mux. Each
// route is wrapped in the OIDC middleware so the handler code can
// assume an authenticated request context.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/tenants/{id}/domains/{domainId}/verify", authMW.Wrap(http.HandlerFunc(h.verifyDomain)))
	mux.Handle("GET /api/v1/tenants/{id}/domains/{domainId}/dns-records", authMW.Wrap(http.HandlerFunc(h.getDomainRecords)))
}

// verifyDomain runs the four DNS checks (MX / SPF / DKIM / DMARC)
// against the tenant's domain and persists the results.
func (h *Handlers) verifyDomain(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	domainID := r.PathValue("domainId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if h.svc == nil {
		writeError(w, http.StatusNotImplemented, errors.New("dns service not configured"))
		return
	}
	result, err := h.svc.VerifyDomain(r.Context(), tenantID, domainID)
	if err != nil {
		h.logger.Printf("verifyDomain: %v", err)
		writeError(w, statusForDNSError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// getDomainRecords returns the DNS records the tenant must publish
// for the domain. The tenant scope is enforced both in middleware
// and via the RLS-scoped domain lookup inside the service.
func (h *Handlers) getDomainRecords(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	domainID := r.PathValue("domainId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if h.svc == nil {
		writeError(w, http.StatusNotImplemented, errors.New("dns service not configured"))
		return
	}
	domain, err := h.svc.LookupDomainName(r.Context(), tenantID, domainID)
	if err != nil {
		h.logger.Printf("getDomainRecords: %v", err)
		writeError(w, statusForDNSError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, h.svc.GetExpectedRecords(domain))
}

// checkTenantScope enforces that the authenticated tenant matches
// the tenant ID in the URL path. This is the application-level
// companion to the RLS policy on `domains` — RLS would already
// block cross-tenant lookups, but failing at the handler gives a
// friendlier 403 than a Postgres error.
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

// statusForDNSError maps a Service error to the HTTP status the
// handlers should return.
func statusForDNSError(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
