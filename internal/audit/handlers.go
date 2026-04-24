package audit

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Registrar is the auth-middleware shape every handler pulls in.
type Registrar interface {
	Wrap(h http.Handler) http.Handler
}

// Handlers mounts the audit-log HTTP surface on a std-lib mux.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers builds the handler facade.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register wires the audit routes onto the provided mux.
func (h *Handlers) Register(mux *http.ServeMux, auth Registrar) {
	mux.Handle("GET /api/v1/tenants/{tenantID}/audit-log", auth.Wrap(http.HandlerFunc(h.query)))
	mux.Handle("GET /api/v1/tenants/{tenantID}/audit-log/export", auth.Wrap(http.HandlerFunc(h.export)))
	mux.Handle("POST /api/v1/tenants/{tenantID}/audit-log/verify", auth.Wrap(http.HandlerFunc(h.verify)))
}

func (h *Handlers) query(w http.ResponseWriter, r *http.Request) {
	tenantID := h.tenantID(r)
	q := r.URL.Query()
	f := QueryFilters{
		Action:  q.Get("action"),
		ActorID: q.Get("actor"),
		// Client-side key is `resource_type` (see
		// web/src/api/admin.ts#AuditLogQuery). Accept `resource`
		// as a legacy alias for CLI callers.
		ResourceType: firstNonEmpty(q.Get("resource_type"), q.Get("resource")),
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			f.Offset = n
		}
	}
	if v := q.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = t
		}
	}
	if v := q.Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Until = t
		}
	}
	entries, err := h.svc.Query(r.Context(), tenantID, f)
	if err != nil {
		h.respondError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (h *Handlers) export(w http.ResponseWriter, r *http.Request) {
	tenantID := h.tenantID(r)
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	var since, until time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		since, _ = time.Parse(time.RFC3339, v)
	}
	if v := r.URL.Query().Get("until"); v != "" {
		until, _ = time.Parse(time.RFC3339, v)
	}
	data, err := h.svc.Export(r.Context(), tenantID, format, since, until)
	if err != nil {
		h.respondError(w, err)
		return
	}
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	_, _ = w.Write(data)
}

func (h *Handlers) verify(w http.ResponseWriter, r *http.Request) {
	tenantID := h.tenantID(r)
	err := h.svc.VerifyChain(r.Context(), tenantID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	case errors.Is(err, ErrChainBroken):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	default:
		h.respondError(w, err)
	}
}

func (h *Handlers) tenantID(r *http.Request) string {
	if id := r.PathValue("tenantID"); id != "" {
		return id
	}
	return middleware.TenantIDFrom(r.Context())
}

func (h *Handlers) respondError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalidInput):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		h.logger.Printf("audit: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
