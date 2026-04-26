package confidentialsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Handlers exposes the confidential-send service over HTTP.
type Handlers struct {
	svc            *Service
	valkey         *redis.Client
	logger         *log.Logger
	maxAttempts    int
	attemptWindow  time.Duration
}

// NewHandlers returns Handlers. `valkey` is optional; when nil
// the public-portal rate limiter degrades to allowing every
// attempt (matches the dev-without-Valkey posture).
func NewHandlers(svc *Service, valkey *redis.Client, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{
		svc:           svc,
		valkey:        valkey,
		logger:        logger,
		maxAttempts:   5,
		attemptWindow: 15 * time.Minute,
	}
}

// Register installs the routes. The public portal route is
// intentionally registered without the auth middleware — token +
// password are the only authentication factors there.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/tenants/{id}/confidential-send", authMW.Wrap(http.HandlerFunc(h.create)))
	mux.Handle("GET /api/v1/tenants/{id}/confidential-send", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("DELETE /api/v1/tenants/{id}/confidential-send/{linkId}", authMW.Wrap(http.HandlerFunc(h.revoke)))
	// Public portal — no auth.
	mux.HandleFunc("GET /api/v1/secure/{token}", h.portal)
	mux.HandleFunc("POST /api/v1/secure/{token}", h.portal)
	// Phase 6: MLS key-derivation endpoints. The Compose flow
	// calls /mls/status to decide between MLS and link-based send,
	// then /mls/wrap to obtain a per-recipient wrapping key.
	mux.Handle("GET /api/v1/tenants/{id}/confidential-send/mls/status", authMW.Wrap(http.HandlerFunc(h.mlsStatus)))
	mux.Handle("POST /api/v1/tenants/{id}/confidential-send/mls/wrap", authMW.Wrap(http.HandlerFunc(h.mlsWrap)))
	mux.Handle("POST /api/v1/tenants/{id}/confidential-send/{linkId}/mls/rekey", authMW.Wrap(http.HandlerFunc(h.mlsRekey)))
}

func (h *Handlers) mlsStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": h.svc.MLSEnabled()})
}

func (h *Handlers) mlsWrap(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SenderLeafKey       string `json:"sender_leaf_key"`
		RecipientCredential string `json:"recipient_credential"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key, err := h.svc.DeriveMLSWrappingKey(r.Context(), in.SenderLeafKey, in.RecipientCredential)
	if errors.Is(err, ErrMLSDisabled) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"wrapping_key": key})
}

func (h *Handlers) mlsRekey(w http.ResponseWriter, r *http.Request) {
	linkID := r.PathValue("linkId")
	var in struct {
		Participants []string `json:"participants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	key, err := h.svc.RekeyConfidentialMessage(r.Context(), linkID, in.Participants)
	if errors.Is(err, ErrMLSDisabled) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"wrapping_key": key})
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	var in struct {
		SenderID         string `json:"sender_id"`
		EncryptedBlobRef string `json:"encrypted_blob_ref"`
		Password         string `json:"password"`
		ExpiresInSec     int    `json:"expires_in_seconds"`
		MaxViews         int    `json:"max_views"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.svc.CreateSecureMessage(r.Context(), CreateRequest{
		TenantID:         tenantID,
		SenderID:         in.SenderID,
		EncryptedBlobRef: in.EncryptedBlobRef,
		Password:         in.Password,
		ExpiresIn:        time.Duration(in.ExpiresInSec) * time.Second,
		MaxViews:         in.MaxViews,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	senderID := r.URL.Query().Get("sender_id")
	out, err := h.svc.ListSentSecureMessages(r.Context(), tenantID, senderID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if out == nil {
		out = []SecureMessage{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) revoke(w http.ResponseWriter, r *http.Request) {
	tenantID := r.PathValue("id")
	linkID := r.PathValue("linkId")
	if err := h.svc.RevokeLink(r.Context(), tenantID, linkID); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// portal handles both GET (initial fetch / no password required)
// and POST (with password in the body). Rate-limited per token.
func (h *Handlers) portal(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		writeError(w, http.StatusBadRequest, errors.New("token required"))
		return
	}
	if !h.allowAttempt(r.Context(), token) {
		writeError(w, http.StatusTooManyRequests, errors.New("too many attempts; try again later"))
		return
	}
	password := ""
	if r.Method == http.MethodPost {
		var in struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err == nil {
			password = in.Password
		}
	}
	out, err := h.svc.GetSecureMessage(r.Context(), token, password)
	if err != nil {
		switch {
		case errors.Is(err, ErrLinkNotFound):
			writeError(w, http.StatusNotFound, err)
		case errors.Is(err, ErrLinkRevoked), errors.Is(err, ErrLinkExpired), errors.Is(err, ErrViewsExceeded):
			writeError(w, http.StatusGone, err)
		case errors.Is(err, ErrInvalidPassword):
			writeError(w, http.StatusUnauthorized, err)
		default:
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// allowAttempt enforces a sliding-bucket rate limit on the public
// portal. Returns false when the per-token attempt count exceeds
// `maxAttempts` within `attemptWindow`. Falls open when Valkey is
// unavailable to avoid taking the portal offline.
func (h *Handlers) allowAttempt(ctx context.Context, token string) bool {
	if h.valkey == nil {
		return true
	}
	bucket := time.Now().UTC().Truncate(h.attemptWindow).Unix()
	key := fmt.Sprintf("kmail:cs:portal:%s:%d", token, bucket)
	count, err := h.valkey.Incr(ctx, key).Result()
	if err != nil {
		h.logger.Printf("confidentialsend: valkey incr %s: %v", key, err)
		return true
	}
	if count == 1 {
		_ = h.valkey.Expire(ctx, key, h.attemptWindow+5*time.Second).Err()
	}
	return int(count) <= h.maxAttempts
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
