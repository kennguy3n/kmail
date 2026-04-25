package vault

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ProtectedFolderHandlers exposes the protected-folder service.
type ProtectedFolderHandlers struct {
	svc    *ProtectedFolderService
	logger *log.Logger
}

// NewProtectedFolderHandlers returns Handlers.
func NewProtectedFolderHandlers(svc *ProtectedFolderService, logger *log.Logger) *ProtectedFolderHandlers {
	if logger == nil {
		logger = log.Default()
	}
	return &ProtectedFolderHandlers{svc: svc, logger: logger}
}

// Register installs the routes.
func (h *ProtectedFolderHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/protected-folders", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/protected-folders", authMW.Wrap(http.HandlerFunc(h.create)))
	mux.Handle("POST /api/v1/tenants/{id}/protected-folders/{folderId}/share", authMW.Wrap(http.HandlerFunc(h.share)))
	mux.Handle("POST /api/v1/tenants/{id}/protected-folders/{folderId}/unshare", authMW.Wrap(http.HandlerFunc(h.unshare)))
	mux.Handle("GET /api/v1/tenants/{id}/protected-folders/{folderId}/access", authMW.Wrap(http.HandlerFunc(h.access)))
	mux.Handle("GET /api/v1/tenants/{id}/protected-folders/{folderId}/access-log", authMW.Wrap(http.HandlerFunc(h.accessLog)))
}

func (h *ProtectedFolderHandlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	ownerID := r.URL.Query().Get("owner_id")
	out, err := h.svc.ListProtectedFolders(r.Context(), tenantID, ownerID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if out == nil {
		out = []ProtectedFolder{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *ProtectedFolderHandlers) create(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var f ProtectedFolder
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f.TenantID = tenantID
	out, err := h.svc.CreateProtectedFolder(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *ProtectedFolderHandlers) share(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	folderID := r.PathValue("folderId")
	var in struct {
		OwnerID    string `json:"owner_id"`
		GranteeID  string `json:"grantee_id"`
		Permission string `json:"permission"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.ShareFolder(r.Context(), tenantID, folderID, in.OwnerID, in.GranteeID, in.Permission)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *ProtectedFolderHandlers) unshare(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	folderID := r.PathValue("folderId")
	var in struct {
		OwnerID   string `json:"owner_id"`
		GranteeID string `json:"grantee_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := h.svc.UnshareFolder(r.Context(), tenantID, folderID, in.OwnerID, in.GranteeID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *ProtectedFolderHandlers) access(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	folderID := r.PathValue("folderId")
	out, err := h.svc.ListFolderAccess(r.Context(), tenantID, folderID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if out == nil {
		out = []FolderAccess{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *ProtectedFolderHandlers) accessLog(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	folderID := r.PathValue("folderId")
	out, err := h.svc.GetFolderAccessLog(r.Context(), tenantID, folderID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if out == nil {
		out = []AccessLogEntry{}
	}
	writeJSON(w, http.StatusOK, out)
}
