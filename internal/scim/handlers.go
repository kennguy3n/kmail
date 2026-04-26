package scim

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers is the SCIM HTTP surface. Two distinct surfaces live
// here:
//
//   * `/scim/v2/Users` and `/scim/v2/Groups` — the SCIM 2.0
//     provisioning endpoints, authenticated via SCIM bearer token
//     (`scim_tokens`). The OIDC middleware does NOT wrap these
//     routes; the IdP-issued bearer is hashed and matched against
//     the per-tenant token table directly.
//
//   * `/api/v1/tenants/{id}/scim/tokens` — the admin API for
//     generating, listing, and revoking SCIM tokens. Wrapped by
//     the standard OIDC middleware so the existing tenant-admin
//     UI can manage tokens.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers returns Handlers.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs all SCIM routes on the mux.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	// SCIM-bearer-token-authenticated provisioning surface.
	mux.Handle("GET /scim/v2/Users", h.scimAuth(http.HandlerFunc(h.listUsers)))
	mux.Handle("POST /scim/v2/Users", h.scimAuth(http.HandlerFunc(h.createUser)))
	mux.Handle("GET /scim/v2/Users/{id}", h.scimAuth(http.HandlerFunc(h.getUser)))
	mux.Handle("PATCH /scim/v2/Users/{id}", h.scimAuth(http.HandlerFunc(h.patchUser)))
	mux.Handle("PUT /scim/v2/Users/{id}", h.scimAuth(http.HandlerFunc(h.patchUser)))
	mux.Handle("DELETE /scim/v2/Users/{id}", h.scimAuth(http.HandlerFunc(h.deleteUser)))
	mux.Handle("GET /scim/v2/Groups", h.scimAuth(http.HandlerFunc(h.listGroups)))
	mux.Handle("POST /scim/v2/Groups", h.scimAuth(http.HandlerFunc(h.createGroup)))
	mux.Handle("GET /scim/v2/Groups/{id}", h.scimAuth(http.HandlerFunc(h.getGroup)))
	mux.Handle("PATCH /scim/v2/Groups/{id}", h.scimAuth(http.HandlerFunc(h.patchGroup)))
	mux.Handle("DELETE /scim/v2/Groups/{id}", h.scimAuth(http.HandlerFunc(h.deleteGroup)))

	// Admin-side token management — wrapped by OIDC.
	mux.Handle("GET /api/v1/tenants/{id}/scim/tokens", authMW.Wrap(http.HandlerFunc(h.listTokens)))
	mux.Handle("POST /api/v1/tenants/{id}/scim/tokens", authMW.Wrap(http.HandlerFunc(h.generateToken)))
	mux.Handle("DELETE /api/v1/tenants/{id}/scim/tokens/{tokenId}", authMW.Wrap(http.HandlerFunc(h.revokeToken)))
}

// scimAuth gates SCIM-protocol routes via the per-tenant
// `scim_tokens` table. On success the resolved tenant ID is
// stashed in the request context so handlers can read it without
// re-authenticating.
type scimCtxKey struct{}

func (h *Handlers) scimAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			h.scimError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
		tenantID, err := h.svc.ResolveTenantFromToken(r.Context(), token)
		if err != nil {
			h.scimError(w, http.StatusUnauthorized, "invalid scim token")
			return
		}
		ctx := context.WithValue(r.Context(), scimCtxKey{}, tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func tenantFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(scimCtxKey{}).(string); ok {
		return v
	}
	return ""
}

func (h *Handlers) scimError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Error{
		Schemas: []string{SchemaError},
		Status:  strconv.Itoa(status),
		Detail:  detail,
	})
}

func (h *Handlers) writeSCIM(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------
// Users
// ---------------------------------------------------------------

func (h *Handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	startIndex, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
	if startIndex == 0 {
		startIndex = 1
	}
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	if count == 0 {
		count = 100
	}
	users, total, err := h.svc.ListUsers(r.Context(), tenantID, startIndex, count)
	if err != nil {
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := make([]any, 0, len(users))
	for _, u := range users {
		res = append(res, u)
	}
	h.writeSCIM(w, http.StatusOK, ListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: total,
		StartIndex:   startIndex,
		ItemsPerPage: len(res),
		Resources:    res,
	})
}

func (h *Handlers) getUser(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	u, err := h.svc.GetUser(r.Context(), tenantID, r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		h.scimError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeSCIM(w, http.StatusOK, u)
}

func (h *Handlers) createUser(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	var in User
	in.Active = true
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.scimError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := h.svc.CreateUser(r.Context(), tenantID, in)
	switch {
	case errors.Is(err, ErrConflict):
		h.scimError(w, http.StatusConflict, "user already exists")
		return
	case errors.Is(err, ErrInvalidInput):
		h.scimError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeSCIM(w, http.StatusCreated, out)
}

func (h *Handlers) patchUser(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	var req PatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.scimError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := h.svc.PatchUser(r.Context(), tenantID, r.PathValue("id"), req)
	if errors.Is(err, ErrNotFound) {
		h.scimError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeSCIM(w, http.StatusOK, u)
}

func (h *Handlers) deleteUser(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	if err := h.svc.DeleteUser(r.Context(), tenantID, r.PathValue("id")); err != nil {
		if errors.Is(err, ErrNotFound) {
			h.scimError(w, http.StatusNotFound, "user not found")
			return
		}
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------
// Groups
// ---------------------------------------------------------------

func (h *Handlers) listGroups(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	startIndex, _ := strconv.Atoi(r.URL.Query().Get("startIndex"))
	if startIndex == 0 {
		startIndex = 1
	}
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	if count == 0 {
		count = 100
	}
	groups, total, err := h.svc.ListGroups(r.Context(), tenantID, startIndex, count)
	if err != nil {
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := make([]any, 0, len(groups))
	for _, g := range groups {
		res = append(res, g)
	}
	h.writeSCIM(w, http.StatusOK, ListResponse{
		Schemas:      []string{SchemaListResponse},
		TotalResults: total,
		StartIndex:   startIndex,
		ItemsPerPage: len(res),
		Resources:    res,
	})
}

func (h *Handlers) getGroup(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	g, err := h.svc.GetGroup(r.Context(), tenantID, r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		h.scimError(w, http.StatusNotFound, "group not found")
		return
	}
	if err != nil {
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeSCIM(w, http.StatusOK, g)
}

func (h *Handlers) createGroup(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	var in Group
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		h.scimError(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := h.svc.CreateGroup(r.Context(), tenantID, in)
	switch {
	case errors.Is(err, ErrConflict):
		h.scimError(w, http.StatusConflict, "group already exists")
		return
	case errors.Is(err, ErrInvalidInput):
		h.scimError(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeSCIM(w, http.StatusCreated, out)
}

func (h *Handlers) patchGroup(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	var req PatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.scimError(w, http.StatusBadRequest, err.Error())
		return
	}
	g, err := h.svc.PatchGroup(r.Context(), tenantID, r.PathValue("id"), req)
	if err != nil {
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.writeSCIM(w, http.StatusOK, g)
}

func (h *Handlers) deleteGroup(w http.ResponseWriter, r *http.Request) {
	tenantID := tenantFromCtx(r.Context())
	if err := h.svc.DeleteGroup(r.Context(), tenantID, r.PathValue("id")); err != nil {
		h.scimError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------
// Token management (admin API)
// ---------------------------------------------------------------

func (h *Handlers) listTokens(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	tokens, err := h.svc.ListTokens(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if tokens == nil {
		tokens = []ScimToken{}
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (h *Handlers) generateToken(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var in struct {
		Description string `json:"description"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	t, plain, err := h.svc.GenerateToken(r.Context(), tenantID, in.Description)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token": plain,
		"meta":  t,
	})
}

func (h *Handlers) revokeToken(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	tokenID := r.PathValue("tokenId")
	if err := h.svc.RevokeToken(r.Context(), tenantID, tokenID); err != nil {
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
