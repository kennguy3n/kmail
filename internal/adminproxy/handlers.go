package adminproxy

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the admin proxy HTTP surface.
type Handlers struct {
	svc            *AdminProxyService
	logger         *log.Logger
	defaultStalwart string
}

// NewHandlers builds Handlers.
func NewHandlers(svc *AdminProxyService, logger *log.Logger, defaultStalwart string) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger, defaultStalwart: defaultStalwart}
}

// Register installs the proxy routes on the mux.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/admin/proxy/{tenantId}/access", authMW.Wrap(http.HandlerFunc(h.requestAccess)))
	mux.Handle("GET /api/v1/admin/proxy/{tenantId}/sessions", authMW.Wrap(http.HandlerFunc(h.listSessions)))
	mux.Handle("POST /api/v1/admin/proxy/{tenantId}/sessions/{sessionId}/revoke", authMW.Wrap(http.HandlerFunc(h.revokeSession)))
	// The wildcard route is the actual proxy. The match pattern
	// below catches everything under
	// /api/v1/admin/proxy/{tenantId}/{approvalId}/...
	mux.Handle("/api/v1/admin/proxy/", authMW.Wrap(http.HandlerFunc(h.proxy)))
}

func (h *Handlers) requestAccess(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenantId")
	adminUserID := middleware.KChatUserIDFrom(r.Context())
	if adminUserID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	var in struct {
		Reason string `json:"reason"`
		Scope  string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req, err := h.svc.RequestAccess(r.Context(), tenantID, adminUserID, in.Reason, in.Scope)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, req)
}

func (h *Handlers) listSessions(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenantId")
	sessions, err := h.svc.ListSessions(r.Context(), tenantID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if sessions == nil {
		sessions = []AccessSession{}
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (h *Handlers) revokeSession(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("tenantId")
	sessionID := r.PathValue("sessionId")
	if err := h.svc.RevokeSession(r.Context(), tenantID, sessionID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// proxy is the wildcard handler. The path shape is
// `/api/v1/admin/proxy/{tenantId}/{approvalId}/{rest...}`. We
// split on the first three segments past `/api/v1/admin/proxy/`
// and forward the remainder to Stalwart.
func (h *Handlers) proxy(w http.ResponseWriter, r *http.Request) {
	const prefix = "/api/v1/admin/proxy/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	tenantID := parts[0]
	approvalID := parts[1]
	tail := ""
	if len(parts) == 3 {
		tail = "/" + parts[2]
	}

	adminUserID := middleware.KChatUserIDFrom(r.Context())
	if adminUserID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}

	sess, err := h.svc.EnsureSession(r.Context(), tenantID, approvalID, adminUserID)
	switch {
	case errors.Is(err, ErrNotApproved):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "approval pending or rejected"})
		return
	case errors.Is(err, ErrSessionExpired):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "session expired"})
		return
	case errors.Is(err, ErrSessionRevoked):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "session revoked"})
		return
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	target, err := h.svc.ResolveShard(r.Context(), tenantID, h.defaultStalwart)
	if err != nil || target == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "no stalwart shard"})
		return
	}
	u, err := url.Parse(target)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad stalwart url"})
		return
	}

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	rev := httputil.NewSingleHostReverseProxy(u)
	rev.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, e error) {
		h.logger.Printf("admin proxy: forwarding error %v", e)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": e.Error()})
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = tail
	r2.URL.RawPath = ""
	r2.Host = u.Host
	rev.ServeHTTP(rec, r2)
	h.svc.LogProxyAccess(r.Context(), tenantID, adminUserID, r, sess.ID, sess.Scope, rec.status)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// drainBody is a small helper for tests that intercept the body
// without forwarding.
func drainBody(r io.Reader) string {
	if r == nil {
		return ""
	}
	var sb strings.Builder
	_, _ = io.Copy(&sb, r)
	return sb.String()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
