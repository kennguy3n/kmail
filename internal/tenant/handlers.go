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
//
// DNS verification and DNS records routes live in
// `internal/dns/handlers.go` so the DNS wizard can evolve
// independently of tenant CRUD.
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
//
// Both PUT and PATCH are accepted for the partial-update endpoints
// because the input types carry pointer fields — omitted fields
// are left unchanged, which is the semantic HTTP clients expect
// from PATCH but which we also keep on PUT for backward
// compatibility with earlier callers.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/tenants", authMW.Wrap(http.HandlerFunc(h.createTenant)))
	mux.Handle("GET /api/v1/tenants", authMW.Wrap(http.HandlerFunc(h.listTenants)))
	mux.Handle("GET /api/v1/tenants/{id}", authMW.Wrap(http.HandlerFunc(h.getTenant)))
	mux.Handle("PUT /api/v1/tenants/{id}", authMW.Wrap(http.HandlerFunc(h.updateTenant)))
	mux.Handle("PATCH /api/v1/tenants/{id}", authMW.Wrap(http.HandlerFunc(h.updateTenant)))
	mux.Handle("DELETE /api/v1/tenants/{id}", authMW.Wrap(http.HandlerFunc(h.deleteTenant)))
	mux.Handle("POST /api/v1/tenants/{id}/users", authMW.Wrap(http.HandlerFunc(h.createUser)))
	mux.Handle("GET /api/v1/tenants/{id}/users", authMW.Wrap(http.HandlerFunc(h.listUsers)))
	mux.Handle("GET /api/v1/tenants/{id}/users/{userId}", authMW.Wrap(http.HandlerFunc(h.getUser)))
	mux.Handle("PUT /api/v1/tenants/{id}/users/{userId}", authMW.Wrap(http.HandlerFunc(h.updateUser)))
	mux.Handle("PATCH /api/v1/tenants/{id}/users/{userId}", authMW.Wrap(http.HandlerFunc(h.updateUser)))
	mux.Handle("DELETE /api/v1/tenants/{id}/users/{userId}", authMW.Wrap(http.HandlerFunc(h.deleteUser)))
	mux.Handle("POST /api/v1/tenants/{id}/domains", authMW.Wrap(http.HandlerFunc(h.createDomain)))
	mux.Handle("GET /api/v1/tenants/{id}/domains", authMW.Wrap(http.HandlerFunc(h.listDomains)))
	mux.Handle("POST /api/v1/tenants/{id}/shared-inboxes", authMW.Wrap(http.HandlerFunc(h.createSharedInbox)))
	mux.Handle("GET /api/v1/tenants/{id}/shared-inboxes", authMW.Wrap(http.HandlerFunc(h.listSharedInboxes)))
	mux.Handle("POST /api/v1/tenants/{id}/shared-inboxes/{inboxId}/members", authMW.Wrap(http.HandlerFunc(h.addSharedInboxMember)))
	mux.Handle("DELETE /api/v1/tenants/{id}/shared-inboxes/{inboxId}/members/{userId}", authMW.Wrap(http.HandlerFunc(h.removeSharedInboxMember)))
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
		writeError(w, statusForServiceError(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (h *Handlers) listTenants(w http.ResponseWriter, r *http.Request) {
	tenants, err := h.svc.ListTenants(r.Context())
	if err != nil {
		h.logger.Printf("listTenants: %v", err)
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if tenants == nil {
		tenants = []Tenant{}
	}
	writeJSON(w, http.StatusOK, tenants)
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

func (h *Handlers) updateTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in UpdateTenantInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	t, err := h.svc.UpdateTenant(r.Context(), id, in)
	if err != nil {
		h.logger.Printf("updateTenant: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (h *Handlers) deleteTenant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.svc.DeleteTenant(r.Context(), id); err != nil {
		h.logger.Printf("deleteTenant: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		writeError(w, statusForServiceError(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (h *Handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	users, err := h.svc.ListUsers(r.Context(), tenantID)
	if err != nil {
		h.logger.Printf("listUsers: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	if users == nil {
		users = []User{}
	}
	writeJSON(w, http.StatusOK, users)
}

func (h *Handlers) getUser(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	userID := r.PathValue("userId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	u, err := h.svc.GetUser(r.Context(), tenantID, userID)
	if err != nil {
		h.logger.Printf("getUser: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (h *Handlers) updateUser(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	userID := r.PathValue("userId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in UpdateUserInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u, err := h.svc.UpdateUser(r.Context(), tenantID, userID, in)
	if err != nil {
		h.logger.Printf("updateUser: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (h *Handlers) deleteUser(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	userID := r.PathValue("userId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := h.svc.DeleteUser(r.Context(), tenantID, userID); err != nil {
		h.logger.Printf("deleteUser: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		writeError(w, statusForServiceError(err), err)
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
		writeError(w, statusForServiceError(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, si)
}

func (h *Handlers) listSharedInboxes(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	inboxes, err := h.svc.ListSharedInboxes(r.Context(), tenantID)
	if err != nil {
		h.logger.Printf("listSharedInboxes: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	if inboxes == nil {
		inboxes = []SharedInbox{}
	}
	writeJSON(w, http.StatusOK, inboxes)
}

func (h *Handlers) addSharedInboxMember(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	inboxID := r.PathValue("inboxId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	var in AddSharedInboxMemberInput
	if err := decodeJSON(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	member, err := h.svc.AddSharedInboxMember(r.Context(), tenantID, inboxID, in.UserID, in.Role)
	if err != nil {
		h.logger.Printf("addSharedInboxMember: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, member)
}

func (h *Handlers) removeSharedInboxMember(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	inboxID := r.PathValue("inboxId")
	userID := r.PathValue("userId")
	if err := checkTenantScope(r, tenantID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := h.svc.RemoveSharedInboxMember(r.Context(), tenantID, inboxID, userID); err != nil {
		h.logger.Printf("removeSharedInboxMember: %v", err)
		writeError(w, statusForServiceError(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// statusForServiceError maps a Service error to the HTTP status the
// mutation handlers should return. Validation failures surface as
// 400 via the ErrInvalidInput sentinel; everything else (Postgres
// outages, pool exhaustion, constraint violations, unexpected RLS
// failures) surfaces as 500 so clients don't retry a transient
// infra failure forever.
func statusForServiceError(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
