package jmap

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// AttachmentHandlers exposes the attachment upload / link / revoke
// HTTP surface on the BFF.
type AttachmentHandlers struct {
	svc    *AttachmentService
	logger *log.Logger
}

// NewAttachmentHandlers returns AttachmentHandlers.
func NewAttachmentHandlers(svc *AttachmentService, logger *log.Logger) *AttachmentHandlers {
	if logger == nil {
		logger = log.Default()
	}
	return &AttachmentHandlers{svc: svc, logger: logger}
}

// Register mounts every attachment route onto the mux.
func (h *AttachmentHandlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/attachments/upload", authMW.Wrap(http.HandlerFunc(h.upload)))
	mux.Handle("GET /api/v1/attachments/{id}/link", authMW.Wrap(http.HandlerFunc(h.link)))
	mux.Handle("DELETE /api/v1/attachments/{id}", authMW.Wrap(http.HandlerFunc(h.revoke)))
}

// upload handles a multipart upload. The file field is named "file".
// The tenant is resolved from the OIDC tenant-id claim on the
// request context.
func (h *AttachmentHandlers) upload(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeAttachmentError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	// Hard cap at 100MB to avoid pathological uploads consuming
	// BFF memory. The SigV4 PUT streams, so this is only on the
	// multipart boundary parsing.
	if err := r.ParseMultipartForm(100 * 1024 * 1024); err != nil {
		writeAttachmentError(w, http.StatusBadRequest, err)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeAttachmentError(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()
	contentType := header.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	out, err := h.svc.UploadLargeAttachment(r.Context(), tenantID, header.Filename, contentType, file, header.Size)
	if err != nil {
		h.logger.Printf("attachment.upload: %v", err)
		writeAttachmentError(w, http.StatusBadGateway, err)
		return
	}
	writeAttachmentJSON(w, http.StatusCreated, out)
}

func (h *AttachmentHandlers) link(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeAttachmentError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	linkID := r.PathValue("id")
	out, err := h.svc.GetAttachmentLink(r.Context(), tenantID, linkID)
	if err != nil {
		writeAttachmentError(w, http.StatusNotFound, err)
		return
	}
	writeAttachmentJSON(w, http.StatusOK, out)
}

func (h *AttachmentHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeAttachmentError(w, http.StatusForbidden, errors.New("missing tenant context"))
		return
	}
	linkID := r.PathValue("id")
	if err := h.svc.RevokeAttachment(r.Context(), tenantID, linkID); err != nil {
		writeAttachmentError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeAttachmentJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAttachmentError(w http.ResponseWriter, status int, err error) {
	writeAttachmentJSON(w, status, map[string]string{"error": fmt.Sprint(err)})
}
