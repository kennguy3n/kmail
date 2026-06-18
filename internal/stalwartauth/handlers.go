package stalwartauth

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// discoveryMaxAge bounds how long Stalwart (or any client) may cache
// the discovery / JWKS documents. Kept short so a key rotation is
// picked up promptly.
const discoveryMaxAge = 5 * time.Minute

// Handlers serves the unauthenticated OIDC endpoints Stalwart fetches
// to validate BFF-minted bearer tokens: the discovery document, the
// JWKS, and a userinfo endpoint. They are intentionally registered
// without the BFF's auth middleware — Stalwart fetches them
// pre-authentication, exactly like the autoconfig endpoints.
type Handlers struct {
	signer *Signer
	logger *log.Logger
}

// NewHandlers builds the HTTP handlers around a Signer.
func NewHandlers(signer *Signer, logger *log.Logger) *Handlers {
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{signer: signer, logger: logger}
}

// Register mounts the endpoints relative to the issuer's path so the
// documents are reachable at exactly the URLs Stalwart derives:
// `{issuer}/.well-known/openid-configuration` and the advertised
// `jwks_uri`. When the issuer has no path component the routes mount
// at the server root.
func (h *Handlers) Register(mux *http.ServeMux) {
	base := h.signer.issuerPath
	mux.HandleFunc("GET "+base+"/.well-known/openid-configuration", h.discovery)
	mux.HandleFunc("GET "+base+"/jwks.json", h.jwks)
	mux.HandleFunc("GET "+base+"/userinfo", h.userinfo)
	// Advertised by discovery at exactly these paths (Stalwart's
	// deserialiser requires the fields) but never used in the
	// directly-minted-token model.
	mux.HandleFunc(base+"/token", h.notImplemented)
	mux.HandleFunc(base+"/authorize", h.notImplemented)
}

func (h *Handlers) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, h.signer.Discovery())
}

func (h *Handlers) jwks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, h.signer.JWKS())
}

// userinfo validates the presented bearer and echoes its principal
// claims. Stalwart does not call this in the local-JWKS validation
// path, but implementing it keeps every advertised GET endpoint real
// and gives operators a way to confirm a minted token end-to-end.
func (h *Handlers) userinfo(w http.ResponseWriter, r *http.Request) {
	authz := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(authz) <= len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_request"`)
		http.Error(w, "bearer token required", http.StatusUnauthorized)
		return
	}
	claims, err := h.signer.Verify(strings.TrimSpace(authz[len(prefix):]))
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	resp := map[string]any{}
	for _, k := range []string{"sub", "email", "preferred_username"} {
		if v, ok := claims[k]; ok {
			resp[k] = v
		}
	}
	body, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "encode failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

func (h *Handlers) notImplemented(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "the KMail BFF issues service tokens directly; the interactive OAuth flow is not supported", http.StatusNotImplemented)
}

func writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(int(discoveryMaxAge.Seconds())))
	_, _ = w.Write(body)
}
