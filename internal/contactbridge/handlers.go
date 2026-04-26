package contactbridge

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers wires contact routes into the BFF mux.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers builds Handlers.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs the contact routes.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/contacts/{accountID}/addressbooks", authMW.Wrap(http.HandlerFunc(h.listAddressBooks)))
	mux.Handle("GET /api/v1/contacts/{accountID}/{addressBookID}", authMW.Wrap(http.HandlerFunc(h.listContacts)))
	mux.Handle("POST /api/v1/contacts/{accountID}/{addressBookID}", authMW.Wrap(http.HandlerFunc(h.createContact)))
	mux.Handle("GET /api/v1/contacts/{accountID}/{addressBookID}/{uid}", authMW.Wrap(http.HandlerFunc(h.getContact)))
	mux.Handle("PUT /api/v1/contacts/{accountID}/{addressBookID}/{uid}", authMW.Wrap(http.HandlerFunc(h.updateContact)))
	mux.Handle("DELETE /api/v1/contacts/{accountID}/{addressBookID}/{uid}", authMW.Wrap(http.HandlerFunc(h.deleteContact)))
}

func (h *Handlers) listAddressBooks(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.ListAddressBooks(r.Context(), r.PathValue("accountID"))
	h.writeResult(w, out, err)
}

func (h *Handlers) listContacts(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.GetContacts(r.Context(), r.PathValue("accountID"), r.PathValue("addressBookID"))
	h.writeResult(w, out, err)
}

func (h *Handlers) getContact(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.GetContact(r.Context(), r.PathValue("accountID"), r.PathValue("addressBookID"), r.PathValue("uid"))
	h.writeResult(w, out, err)
}

func (h *Handlers) createContact(w http.ResponseWriter, r *http.Request) {
	var d ContactDraft
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	uid, err := h.svc.CreateContact(r.Context(), r.PathValue("accountID"), r.PathValue("addressBookID"), d)
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"uid": uid})
}

func (h *Handlers) updateContact(w http.ResponseWriter, r *http.Request) {
	var d ContactDraft
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := h.svc.UpdateContact(r.Context(), r.PathValue("accountID"), r.PathValue("addressBookID"), r.PathValue("uid"), d); err != nil {
		h.respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) deleteContact(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteContact(r.Context(), r.PathValue("accountID"), r.PathValue("addressBookID"), r.PathValue("uid")); err != nil {
		h.respondError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) writeResult(w http.ResponseWriter, v any, err error) {
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *Handlers) respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
