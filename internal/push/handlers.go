package push

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the Push Service over REST.
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

// Register mounts every push route on the provided mux.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/push/subscribe",
		authMW.Wrap(http.HandlerFunc(h.subscribe)))
	mux.Handle("DELETE /api/v1/push/subscriptions/{id}",
		authMW.Wrap(http.HandlerFunc(h.unsubscribe)))
	mux.Handle("GET /api/v1/push/subscriptions",
		authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("GET /api/v1/push/preferences",
		authMW.Wrap(http.HandlerFunc(h.getPrefs)))
	mux.Handle("PUT /api/v1/push/preferences",
		authMW.Wrap(http.HandlerFunc(h.setPrefs)))
}

func (h *Handlers) subscribe(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := identify(r, w)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var sub Subscription
	if err := json.Unmarshal(body, &sub); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.Subscribe(r.Context(), tenantID, userID, sub)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) unsubscribe(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := identify(r, w)
	if !ok {
		return
	}
	if err := h.svc.Unsubscribe(r.Context(), tenantID, userID, r.PathValue("id")); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := identify(r, w)
	if !ok {
		return
	}
	out, err := h.svc.ListSubscriptions(r.Context(), tenantID, userID)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": out})
}

func (h *Handlers) getPrefs(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := identify(r, w)
	if !ok {
		return
	}
	prefs, err := h.svc.GetPreferences(r.Context(), tenantID, userID)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, prefs)
}

func (h *Handlers) setPrefs(w http.ResponseWriter, r *http.Request) {
	tenantID, userID, ok := identify(r, w)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var prefs NotificationPreference
	if err := json.Unmarshal(body, &prefs); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.UpdatePreferences(r.Context(), tenantID, userID, prefs)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// identify extracts the tenant + Stalwart account ID from the
// OIDC middleware. Returns (tenantID, userID, true) on success or
// writes an error response and returns ok=false.
func identify(r *http.Request, w http.ResponseWriter) (string, string, bool) {
	tenantID := middleware.TenantIDFrom(r.Context())
	if tenantID == "" {
		writeErr(w, http.StatusForbidden, errors.New("missing tenant context"))
		return "", "", false
	}
	userID := middleware.StalwartAccountIDFrom(r.Context())
	if userID == "" {
		userID = middleware.KChatUserIDFrom(r.Context())
	}
	if userID == "" {
		writeErr(w, http.StatusForbidden, errors.New("missing user context"))
		return "", "", false
	}
	return tenantID, userID, true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return http.StatusBadRequest
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}
