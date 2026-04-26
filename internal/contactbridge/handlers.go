package contactbridge

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers wires contact routes into the BFF mux.
type Handlers struct {
	svc    *Service
	gal    *GALService
	logger *log.Logger
}

// NewHandlers builds Handlers.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// WithGAL wires a GAL service so the `/gal` endpoints work.
func (h *Handlers) WithGAL(g *GALService) *Handlers {
	h.gal = g
	return h
}

// Register installs the contact routes.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/contacts/{accountID}/addressbooks", authMW.Wrap(http.HandlerFunc(h.listAddressBooks)))
	mux.Handle("GET /api/v1/contacts/{accountID}/{addressBookID}", authMW.Wrap(http.HandlerFunc(h.listContacts)))
	mux.Handle("POST /api/v1/contacts/{accountID}/{addressBookID}", authMW.Wrap(http.HandlerFunc(h.createContact)))
	mux.Handle("GET /api/v1/contacts/{accountID}/{addressBookID}/{uid}", authMW.Wrap(http.HandlerFunc(h.getContact)))
	mux.Handle("PUT /api/v1/contacts/{accountID}/{addressBookID}/{uid}", authMW.Wrap(http.HandlerFunc(h.updateContact)))
	mux.Handle("DELETE /api/v1/contacts/{accountID}/{addressBookID}/{uid}", authMW.Wrap(http.HandlerFunc(h.deleteContact)))
	// Phase 6: bulk vCard import / export.
	mux.Handle("POST /api/v1/contacts/{accountID}/{addressBookID}/import", authMW.Wrap(http.HandlerFunc(h.importVCard)))
	mux.Handle("GET /api/v1/contacts/{accountID}/{addressBookID}/export", authMW.Wrap(http.HandlerFunc(h.exportVCard)))
	// Phase 6: tenant-wide GAL.
	mux.Handle("GET /api/v1/contacts/gal", authMW.Wrap(http.HandlerFunc(h.listGAL)))
	mux.Handle("GET /api/v1/contacts/gal/search", authMW.Wrap(http.HandlerFunc(h.searchGAL)))
	mux.Handle("POST /api/v1/contacts/gal/sync", authMW.Wrap(http.HandlerFunc(h.syncGAL)))
}

func (h *Handlers) listGAL(w http.ResponseWriter, r *http.Request) {
	if h.gal == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "gal not configured"})
		return
	}
	tenantID := middleware.TenantIDFrom(r.Context())
	out, err := h.gal.List(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = []GALEntry{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) searchGAL(w http.ResponseWriter, r *http.Request) {
	if h.gal == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "gal not configured"})
		return
	}
	tenantID := middleware.TenantIDFrom(r.Context())
	out, err := h.gal.Search(r.Context(), tenantID, r.URL.Query().Get("q"), 25)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if out == nil {
		out = []GALEntry{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) syncGAL(w http.ResponseWriter, r *http.Request) {
	if h.gal == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "gal not configured"})
		return
	}
	tenantID := middleware.TenantIDFrom(r.Context())
	var in struct {
		Accounts []string `json:"accounts"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	written, err := h.gal.Sync(r.Context(), tenantID, in.Accounts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"upserted": written})
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

func (h *Handlers) importVCard(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	defer r.Body.Close()
	cards := splitMultiVCard(string(body))
	created := 0
	failed := 0
	for _, raw := range cards {
		c := ParseVCard(raw)
		if c == nil || c.FN == "" {
			failed++
			continue
		}
		draft := ContactDraft{
			FN: c.FN, Emails: c.Emails, Phones: c.Phones,
			Org: c.Org, Note: c.Note,
			PhotoURL: c.PhotoURL, Groups: c.Groups,
		}
		if _, err := h.svc.CreateContact(r.Context(), r.PathValue("accountID"), r.PathValue("addressBookID"), draft); err != nil {
			failed++
			continue
		}
		created++
	}
	writeJSON(w, http.StatusOK, map[string]int{"created": created, "failed": failed})
}

func (h *Handlers) exportVCard(w http.ResponseWriter, r *http.Request) {
	contacts, err := h.svc.GetContacts(r.Context(), r.PathValue("accountID"), r.PathValue("addressBookID"))
	if err != nil {
		h.respondError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\"contacts.vcf\"")
	for _, c := range contacts {
		if c.VCardRaw != "" {
			w.Write([]byte(c.VCardRaw))
			if !strings.HasSuffix(c.VCardRaw, "\r\n") {
				w.Write([]byte("\r\n"))
			}
			continue
		}
		w.Write([]byte(BuildVCard(ContactDraft{
			UID: c.UID, FN: c.FN, Emails: c.Emails, Phones: c.Phones,
			Org: c.Org, Note: c.Note,
			PhotoURL: c.PhotoURL, Groups: c.Groups,
		})))
	}
}

// splitMultiVCard splits a buffer that contains one or more
// `BEGIN:VCARD ... END:VCARD` blocks into the individual cards.
func splitMultiVCard(buf string) []string {
	const begin = "BEGIN:VCARD"
	var out []string
	for {
		i := strings.Index(buf, begin)
		if i < 0 {
			break
		}
		j := strings.Index(buf[i:], "END:VCARD")
		if j < 0 {
			break
		}
		end := i + j + len("END:VCARD")
		out = append(out, buf[i:end])
		buf = buf[end:]
	}
	return out
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
