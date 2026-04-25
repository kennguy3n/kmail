package vault

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// VaultHandlers exposes the vault service over
// `/api/v1/tenants/{id}/vault/folders`.
type VaultHandlers struct {
	svc    *VaultService
	logger *log.Logger
}

// NewVaultHandlers returns Handlers.
func NewVaultHandlers(svc *VaultService, logger *log.Logger) *VaultHandlers {
	if logger == nil {
		logger = log.Default()
	}
	return &VaultHandlers{svc: svc, logger: logger}
}

// Register installs the routes.
func (h *VaultHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/tenants/{id}/vault/folders", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/tenants/{id}/vault/folders", authMW.Wrap(http.HandlerFunc(h.create)))
	mux.Handle("GET /api/v1/tenants/{id}/vault/folders/{folderId}", authMW.Wrap(http.HandlerFunc(h.get)))
	mux.Handle("DELETE /api/v1/tenants/{id}/vault/folders/{folderId}", authMW.Wrap(http.HandlerFunc(h.delete)))
	mux.Handle("PUT /api/v1/tenants/{id}/vault/folders/{folderId}/encryption-meta", authMW.Wrap(http.HandlerFunc(h.setMeta)))
}

func (h *VaultHandlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	userID := r.URL.Query().Get("user_id")
	out, err := h.svc.ListVaultFolders(r.Context(), tenantID, userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if out == nil {
		out = []Folder{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *VaultHandlers) create(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var f Folder
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	f.TenantID = tenantID
	out, err := h.svc.CreateVaultFolder(r.Context(), f)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *VaultHandlers) get(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	folderID := r.PathValue("folderId")
	out, err := h.svc.GetVaultFolder(r.Context(), tenantID, folderID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *VaultHandlers) delete(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	folderID := r.PathValue("folderId")
	if err := h.svc.DeleteVaultFolder(r.Context(), tenantID, folderID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *VaultHandlers) setMeta(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	folderID := r.PathValue("folderId")
	var in struct {
		WrappedDEK   []byte `json:"wrapped_dek"`
		KeyAlgorithm string `json:"key_algorithm"`
		Nonce        []byte `json:"nonce"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.SetFolderEncryptionMeta(r.Context(), tenantID, folderID, in.WrappedDEK, in.KeyAlgorithm, in.Nonce)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
