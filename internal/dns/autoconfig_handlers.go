// Package dns — autoconfig / autodiscover HTTP handlers.
//
// Public, unauthenticated endpoints. Mozilla autoconfig (used by
// Thunderbird) and Microsoft Outlook autodiscover both work by
// the client guessing where to fetch the XML based on the user's
// email domain — the most common path is `<domain>/mail/config-v1.1.xml`
// or `autoconfig.<domain>/mail/config-v1.1.xml`. KMail's DNS
// wizard publishes the matching DNS records; this file owns the
// HTTP side.
package dns

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// AutoconfigHandlers wraps the AutoconfigService for HTTP.
type AutoconfigHandlers struct {
	svc    *AutoconfigService
	logger *log.Logger
}

// NewAutoconfigHandlers builds the handlers struct.
func NewAutoconfigHandlers(svc *AutoconfigService, logger *log.Logger) *AutoconfigHandlers {
	if logger == nil {
		logger = log.Default()
	}
	return &AutoconfigHandlers{svc: svc, logger: logger}
}

// Register binds the autoconfig / autodiscover endpoints. These
// are intentionally registered without the OIDC middleware — the
// whole point is that Thunderbird / Outlook fetch them
// pre-authentication.
func (h *AutoconfigHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /mail/config-v1.1.xml", h.mozillaAutoconfig)
	mux.HandleFunc("GET /.well-known/autoconfig/mail/config-v1.1.xml", h.mozillaAutoconfig)
	mux.HandleFunc("POST /autodiscover/autodiscover.xml", h.outlookAutodiscover)
	// Outlook also probes the lowercase path on some clients.
	mux.HandleFunc("POST /Autodiscover/Autodiscover.xml", h.outlookAutodiscover)
}

// mozillaAutoconfig answers Thunderbird's GET. The `emailaddress`
// query parameter carries the user's email; we extract the
// domain, look it up, and reply.
func (h *AutoconfigHandlers) mozillaAutoconfig(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("emailaddress"))
	if email == "" {
		http.Error(w, "emailaddress query parameter required", http.StatusBadRequest)
		return
	}
	settings, err := h.svc.SettingsForEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, ErrUnknownDomain) {
			http.Error(w, "domain not registered", http.StatusNotFound)
			return
		}
		h.logger.Printf("autoconfig.mozilla: %v", err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	body, err := MozillaXML(email, *settings)
	if err != nil {
		h.logger.Printf("autoconfig.mozilla: render: %v", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(CacheControlMaxAge.Seconds())))
	_, _ = w.Write(body)
}

// outlookAutodiscover answers Outlook's POST.
func (h *AutoconfigHandlers) outlookAutodiscover(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()
	var req OutlookAutodiscoverRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid autodiscover request", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(req.Request.EMailAddress)
	if email == "" {
		http.Error(w, "EMailAddress required", http.StatusBadRequest)
		return
	}
	settings, err := h.svc.SettingsForEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, ErrUnknownDomain) {
			http.Error(w, "domain not registered", http.StatusNotFound)
			return
		}
		h.logger.Printf("autoconfig.outlook: %v", err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	xmlBody, err := OutlookXML(email, *settings)
	if err != nil {
		h.logger.Printf("autoconfig.outlook: render: %v", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(CacheControlMaxAge.Seconds())))
	_, _ = w.Write(xmlBody)
}
