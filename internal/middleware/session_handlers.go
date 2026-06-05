package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// SessionHandlers serves the user-facing session API:
//
//	GET  /api/v1/sessions          list the caller's active sessions
//	POST /api/v1/sessions/revoke   revoke a session by id (or all others)
//
// Both are registered behind the same OIDC auth wrapper as the rest
// of the API, so the caller's identity comes from the request
// context. A user may only list and revoke their OWN sessions — the
// userKey is derived from the authenticated context, never from the
// request body, so one user cannot enumerate or revoke another's
// sessions.
type SessionHandlers struct {
	mgr *SessionManager
}

// NewSessionHandlers builds the handlers around a manager.
func NewSessionHandlers(mgr *SessionManager) *SessionHandlers {
	return &SessionHandlers{mgr: mgr}
}

// Register mounts the routes, applying `wrap` (the OIDC auth
// middleware) to each.
func (h *SessionHandlers) Register(mux *http.ServeMux, wrap func(http.Handler) http.Handler) {
	mux.Handle("GET /api/v1/sessions", wrap(http.HandlerFunc(h.list)))
	mux.Handle("POST /api/v1/sessions/revoke", wrap(http.HandlerFunc(h.revoke)))
}

type sessionListItem struct {
	SessionInfo
	Current bool `json:"current"`
}

type sessionListResponse struct {
	Sessions      []sessionListItem `json:"sessions"`
	MaxConcurrent int               `json:"max_concurrent"`
	IdleTimeoutS  int               `json:"idle_timeout_seconds"`
}

func (h *SessionHandlers) storeReady(w http.ResponseWriter) bool {
	if h.mgr == nil || h.mgr.cfg.Store == nil {
		http.Error(w, "session management not configured", http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *SessionHandlers) list(w http.ResponseWriter, r *http.Request) {
	if !h.storeReady(w) {
		return
	}
	tenantID := TenantIDFrom(r.Context())
	userID := KChatUserIDFrom(r.Context())
	if tenantID == "" || userID == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	now := time.Now()
	sessions, err := h.mgr.cfg.Store.List(r.Context(), userKey(tenantID, userID), h.mgr.cfg.IdleTimeout, now)
	if err != nil {
		h.mgr.logf("session: list failed: %v", err)
		http.Error(w, "failed to list sessions", http.StatusBadGateway)
		return
	}
	current := SessionIDFromRequest(r)
	items := make([]sessionListItem, 0, len(sessions))
	for _, s := range sessions {
		items = append(items, sessionListItem{SessionInfo: s, Current: s.ID == current})
	}
	writeJSON(w, http.StatusOK, sessionListResponse{
		Sessions:      items,
		MaxConcurrent: h.mgr.cfg.MaxConcurrent,
		IdleTimeoutS:  int(h.mgr.cfg.IdleTimeout / time.Second),
	})
}

type revokeRequest struct {
	// SessionID is the session to revoke. Mutually exclusive with
	// AllOthers.
	SessionID string `json:"session_id"`
	// AllOthers revokes every session for the caller EXCEPT the one
	// the request is made from (a "sign out everywhere else").
	AllOthers bool `json:"all_others"`
}

type revokeResponse struct {
	Revoked []string `json:"revoked"`
}

func (h *SessionHandlers) revoke(w http.ResponseWriter, r *http.Request) {
	if !h.storeReady(w) {
		return
	}
	tenantID := TenantIDFrom(r.Context())
	userID := KChatUserIDFrom(r.Context())
	if tenantID == "" || userID == "" {
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}
	var req revokeRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.SessionID == "" && !req.AllOthers {
		http.Error(w, "session_id or all_others required", http.StatusBadRequest)
		return
	}
	uk := userKey(tenantID, userID)
	now := time.Now()
	current := SessionIDFromRequest(r)
	ttl := h.mgr.cfg.RevokeTTL

	var toRevoke []string
	if req.AllOthers {
		sessions, err := h.mgr.cfg.Store.List(r.Context(), uk, h.mgr.cfg.IdleTimeout, now)
		if err != nil {
			h.mgr.logf("session: list during revoke-all failed: %v", err)
			http.Error(w, "failed to enumerate sessions", http.StatusBadGateway)
			return
		}
		for _, s := range sessions {
			if s.ID != current {
				toRevoke = append(toRevoke, s.ID)
			}
		}
	} else {
		// Single-session revoke. The store enforces ownership: it
		// only revokes (and writes a tombstone) when the id is in
		// THIS user's set, returning ErrSessionNotFound otherwise.
		// So a caller cannot revoke or tombstone another user's
		// session even by guessing its id.
		toRevoke = []string{req.SessionID}
	}

	revoked := make([]string, 0, len(toRevoke))
	for _, sid := range toRevoke {
		switch err := h.mgr.cfg.Store.Revoke(r.Context(), uk, sid, ttl, now); {
		case err == nil:
			revoked = append(revoked, sid)
		case errors.Is(err, ErrSessionNotFound):
			// Not the caller's session (or already gone): skip it
			// rather than 502 the whole request or tombstone an
			// id we do not own.
			continue
		default:
			h.mgr.logf("session: revoke %s failed: %v", sid, err)
			http.Error(w, "failed to revoke session", http.StatusBadGateway)
			return
		}
	}
	// A single-session revoke that matched nothing the caller owns is
	// a 404 — distinct from a successful "signed out everywhere else"
	// that legitimately had no other sessions to revoke.
	if !req.AllOthers && len(revoked) == 0 {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, revokeResponse{Revoked: revoked})
}
