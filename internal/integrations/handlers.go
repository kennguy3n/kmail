package integrations

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/kennguy3n/kmail/internal/oauth"
	"github.com/kennguy3n/kmail/internal/webhooks"
)

// Handlers exposes the OAuth2-scoped integration HTTP routes.
// Construct once at boot, attach via Register, and the public
// surface at /api/v1/integ/* is wired in.
type Handlers struct {
	svc    *Service
	logger *log.Logger
}

// NewHandlers wires the handler struct.
func NewHandlers(svc *Service, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, logger: logger}
}

// Register installs the integration routes on `mux`. Each route
// is wrapped with the OAuth2 AuthMiddleware. Most routes also
// flow through RequireScope so an integration that holds no
// relevant scope at all gets a clean 403 at the boundary
// without touching the database. The dispatcher does ITS OWN
// scope check (defence in depth) — see Service.DispatchEvent.
//
// Why per-route RequireScope on the webhook routes? An
// integration that holds NEITHER read:mail NOR read:calendar
// has no events it could subscribe to; rejecting at the
// middleware layer is cheaper than walking the FilterEventsForClient
// path just to discover everything was denied. The dispatcher's
// per-fire scope check is what catches the case where
// (a) the client held a scope at register-time, and
// (b) the user has since narrowed the grant.
//
// We attach EITHER scope on the webhook routes (read:mail OR
// read:calendar) by accepting any of the integration-eligible
// read scopes — the per-event filter inside the handler does
// the per-event narrowing. Implemented as a custom middleware
// rather than oauth.AuthMiddleware.RequireScope so a future
// scope addition doesn't require a code change here.
func (h *Handlers) Register(mux *http.ServeMux, authMW *oauth.AuthMiddleware) {
	mux.Handle("POST /api/v1/integ/webhooks",
		authMW.Wrap(h.requireAnyIntegrationScope(http.HandlerFunc(h.register))))
	mux.Handle("GET /api/v1/integ/webhooks",
		authMW.Wrap(h.requireAnyIntegrationScope(http.HandlerFunc(h.list))))
	mux.Handle("DELETE /api/v1/integ/webhooks/{webhookId}",
		authMW.Wrap(h.requireAnyIntegrationScope(http.HandlerFunc(h.del))))
	mux.Handle("POST /api/v1/integ/webhooks/{webhookId}/test",
		authMW.Wrap(h.requireAnyIntegrationScope(http.HandlerFunc(h.testFire))))
}

// requireAnyIntegrationScope rejects with 403 unless the
// caller's token carries at least one of the read scopes that
// imply integration eligibility. This is the boundary-level
// check; per-event filtering happens INSIDE each handler.
func (h *Handlers) requireAnyIntegrationScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCtx, ok := oauth.FromContext(r.Context())
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "invalid_token", "missing access token context")
			return
		}
		if !hasAnyIntegrationScope(tokenCtx) {
			writeJSONError(w, http.StatusForbidden, "insufficient_scope",
				"token must carry at least one of: "+strings.Join(integrationEligibleScopes(), ", "))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hasAnyIntegrationScope reports whether the token carries any
// scope that maps to an integration-eligible event.
func hasAnyIntegrationScope(tokenCtx *oauth.AccessTokenContext) bool {
	for _, scope := range integrationEligibleScopes() {
		if tokenCtx.HasScope(scope) {
			return true
		}
	}
	return false
}

// integrationEligibleScopes returns the unique set of scopes
// that appear as required-scopes in EventRequiredScope. Derived
// rather than hard-coded so a new event type whose scope is
// already in the table doesn't require a change here, and a
// new scope automatically widens the boundary check.
//
// EventRequiredScope is a package-level map that never changes
// after init, so the derivation is computed exactly once via
// sync.OnceValue and cached. The returned slice is shared across
// all callers — they MUST NOT mutate it. All call sites
// (requireAnyIntegrationScope, hasAnyIntegrationScope, the
// FilterEventsForClient unit tests) only iterate.
//
// The cache is keyed on no state, so a test that swaps
// EventRequiredScope at init must do so BEFORE the first call.
// In practice EventRequiredScope is a package-level `var` set
// at compile time; no test mutates it.
func integrationEligibleScopes() []string {
	return integrationEligibleScopesOnce()
}

var integrationEligibleScopesOnce = sync.OnceValue(func() []string {
	seen := make(map[string]struct{}, len(EventRequiredScope))
	out := make([]string, 0, len(EventRequiredScope))
	for _, sc := range EventRequiredScope {
		if sc == "" {
			continue
		}
		if _, ok := seen[sc]; ok {
			continue
		}
		seen[sc] = struct{}{}
		out = append(out, sc)
	}
	// Deterministic order so the human-readable list in the
	// 403 "insufficient_scope" response is stable across boots
	// (map iteration order is randomised in Go) — easier for
	// integration developers to grep error logs.
	sort.Strings(out)
	return out
})

// registerRequest is the body of POST /api/v1/integ/webhooks.
type registerRequest struct {
	URL            string   `json:"url"`
	Events         []string `json:"events"`
	SigningVersion string   `json:"signing_version,omitempty"`
}

// registerResponse is the body of a successful subscribe. The
// `secret` is returned once; the integration must persist it
// (it is required to verify HMACs on every incoming delivery).
type registerResponse struct {
	Endpoint *webhooks.Endpoint `json:"endpoint"`
	Secret   string             `json:"secret"`
	Denied   []string           `json:"denied,omitempty"`
}

func (h *Handlers) register(w http.ResponseWriter, r *http.Request) {
	tokenCtx, ok := oauth.FromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "invalid_token", "missing access token context")
		return
	}
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "url required")
		return
	}

	result, err := h.svc.RegisterWebhookForClient(
		r.Context(),
		tokenCtx.TenantID,
		tokenCtx.ClientID,
		// The consenting user, threaded from the OAuth2 access
		// token, is the link the dispatcher needs to source
		// per-user granted scopes (instead of static client
		// allowed_scopes) when this user later revokes consent.
		tokenCtx.UserID,
		tokenCtx.Scopes,
		req.URL,
		req.Events,
		req.SigningVersion,
	)
	if errors.Is(err, ErrInsufficientScope) {
		// All requested events were denied. 422 carries the
		// denied list so the integration knows which scopes
		// to ask the user for.
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":             "insufficient_scope",
			"error_description": "no requested events are within the granted scope set",
			"denied_events":     result.Denied,
		})
		return
	}
	if err != nil {
		h.logger.Printf("integrations: register failed for client=%s tenant=%s: %v", tokenCtx.ClientID, tokenCtx.TenantID, err)
		writeJSONError(w, http.StatusInternalServerError, "server_error", "failed to register webhook")
		return
	}
	writeJSON(w, http.StatusCreated, registerResponse{
		Endpoint: result.Endpoint,
		Secret:   result.Secret,
		Denied:   result.Denied,
	})
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tokenCtx, ok := oauth.FromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "invalid_token", "missing access token context")
		return
	}
	out, err := h.svc.ListWebhooksForClient(r.Context(), tokenCtx.TenantID, tokenCtx.ClientID)
	if err != nil {
		h.logger.Printf("integrations: list failed for client=%s tenant=%s: %v", tokenCtx.ClientID, tokenCtx.TenantID, err)
		writeJSONError(w, http.StatusInternalServerError, "server_error", "failed to list webhooks")
		return
	}
	if out == nil {
		out = []webhooks.Endpoint{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) del(w http.ResponseWriter, r *http.Request) {
	tokenCtx, ok := oauth.FromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "invalid_token", "missing access token context")
		return
	}
	webhookID := r.PathValue("webhookId")
	err := h.svc.DeleteWebhookForClient(r.Context(), tokenCtx.TenantID, tokenCtx.ClientID, webhookID)
	if errors.Is(err, ErrWebhookNotFound) {
		writeJSONError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	if err != nil {
		h.logger.Printf("integrations: delete failed for client=%s webhook=%s: %v", tokenCtx.ClientID, webhookID, err)
		writeJSONError(w, http.StatusInternalServerError, "server_error", "failed to delete webhook")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handlers) testFire(w http.ResponseWriter, r *http.Request) {
	tokenCtx, ok := oauth.FromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "invalid_token", "missing access token context")
		return
	}
	webhookID := r.PathValue("webhookId")
	count, err := h.svc.TestFireForClient(r.Context(), tokenCtx.TenantID, tokenCtx.ClientID, webhookID)
	if errors.Is(err, ErrWebhookNotFound) {
		writeJSONError(w, http.StatusNotFound, "not_found", "webhook not found")
		return
	}
	if err != nil {
		h.logger.Printf("integrations: test-fire failed for client=%s webhook=%s: %v", tokenCtx.ClientID, webhookID, err)
		writeJSONError(w, http.StatusInternalServerError, "server_error", "failed to test webhook")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"enqueued": count})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{
		"error":             code,
		"error_description": description,
	})
}
