package tenant

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the Tenant Service over REST under
// `/api/v1/tenants`. The handlers are thin: they decode JSON,
// delegate to Service, and marshal the result.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers returns a Handlers bound to the provided Service.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs every tenant route onto the provided mux. Each
// route is wrapped in the OIDC middleware so the handler code can
// assume an authenticated request context. The mux is the Go 1.22+
// `http.ServeMux`, which supports method+path patterns natively.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/tenants", authMW.Wrap(http.HandlerFunc(h.createTenant)))
	mux.Handle("GET /api/v1/tenants/{id}", authMW.Wrap(http.HandlerFunc(h.getTenant)))
	mux.Handle("POST /api/v1/tenants/{id}/users", authMW.Wrap(http.HandlerFunc(h.createUser)))
	mux.Handle("POST /api/v1/tenants/{id}/domains", authMW.Wrap(http.HandlerFunc(h.createDomain)))
	mux.Handle("GET /api/v1/tenants/{id}/domains", authMW.Wrap(http.HandlerFunc(h.listDomains)))
	mux.Handle("POST /api/v1/tenants/{id}/shared-inboxes", authMW.Wrap(http.HandlerFunc(h.createSharedInbox)))
}

func (h *Handlers) createTenant(w http.ResponseWriter, r *http.Request) {
	var in CreateTenantInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t, err := h.svc.CreateTenant(r.Context(), in)
	if err != nil {
		h.logger.Printf("createTenant: %v", err)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *Handlers) getTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := h.svc.GetTenant(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if err != nil {
		h.logger.Printf("getTenant: %v", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *Handlers) createUser(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in CreateUserInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := h.svc.CreateUser(r.Context(), tenantID, in)
	if err != nil {
		h.logger.Printf("createUser: %v", err)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (h *Handlers) createDomain(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in CreateDomainInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	d, err := h.svc.CreateDomain(r.Context(), tenantID, in)
	if err != nil {
		h.logger.Printf("createDomain: %v", err)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, d)
}

func (h *Handlers) listDomains(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	domains, err := h.svc.ListDomains(r.Context(), tenantID)
	if err != nil {
		h.logger.Printf("listDomains: %v", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if domains == nil {
		domains = []Domain{}
	}
	writeJSON(w, http.StatusOK, domains)
}

func (h *Handlers) createSharedInbox(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in CreateSharedInboxInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	si, err := h.svc.CreateSharedInbox(r.Context(), tenantID, in)
	if err != nil {
		h.logger.Printf("createSharedInbox: %v", err)
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, si)
}

// checkTenantScope enforces that the authenticated tenant matches
// the tenant ID in the URL path. This is the application-level
// companion to the RLS policy on every tenant-scoped table — the
// RLS policy would already block a cross-tenant write, but failing
// at the handler gives a friendlier 403 than a Postgres error.
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

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
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
