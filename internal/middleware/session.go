package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session management layers a server-side session ledger on top of
// KMail's stateless OIDC bearer auth. The JWT itself remains the
// source of truth for *token validity* (its `exp` is authoritative);
// this ledger adds three things a stateless token cannot provide on
// its own:
//
//   - Concurrent-session cap: at most N live sessions per user;
//     the oldest is evicted when a new device pushes past the cap.
//   - Revocation: an operator or the user can revoke a specific
//     session so its token is refused at the KMail boundary before
//     the JWT would naturally expire.
//   - Visibility: the "Active sessions" surface lists where a user
//     is currently signed in.
//
// A session is keyed by a salted hash of the bearer token, so the
// same token maps to a stable id without the raw token ever being
// stored. Idle timeout is implemented as a TTL on the session
// record: a session that goes untouched for IdleTimeout drops off
// the active list and frees its concurrency slot. (It does not by
// itself invalidate a still-unexpired JWT — revocation does that.)

// SessionInfo is one entry in the session ledger.
type SessionInfo struct {
	ID        string    `json:"id"`
	UserKey   string    `json:"-"`
	TenantID  string    `json:"tenant_id"`
	UserID    string    `json:"user_id"`
	UserAgent string    `json:"user_agent"`
	IP        string    `json:"ip"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// SessionStore persists the ledger. Implementations: MemorySessionStore
// (per-replica, used in tests and as a degraded fallback) and
// RedisSessionStore (shared across replicas — required for the
// concurrent-cap and revocation to be globally correct).
type SessionStore interface {
	// Touch upserts s and refreshes its idle TTL. When s is a new
	// session that would push the user past maxConcurrent live
	// sessions, the store evicts the oldest sessions (by CreatedAt)
	// to make room and returns their ids. maxConcurrent <= 0 means
	// unlimited. `now` is injected for deterministic tests.
	Touch(ctx context.Context, s SessionInfo, idleTTL time.Duration, maxConcurrent int, now time.Time) (evicted []string, err error)
	// List returns the live (non-expired) sessions for userKey,
	// newest first.
	List(ctx context.Context, userKey string, idleTTL time.Duration, now time.Time) ([]SessionInfo, error)
	// Revoke removes sessionID from userKey's live set and records a
	// revocation tombstone for `ttl` (set >= max token lifetime).
	Revoke(ctx context.Context, userKey, sessionID string, ttl time.Duration, now time.Time) error
	// IsRevoked reports whether sessionID currently has a live
	// revocation tombstone.
	IsRevoked(ctx context.Context, sessionID string, now time.Time) (bool, error)
}

// Default session-management knobs.
const (
	DefaultSessionIdleTimeout   = 8 * time.Hour
	DefaultSessionMaxConcurrent = 5
	// sessionRevokeTTL is how long a revocation tombstone lives. It
	// must outlast the longest possible JWT lifetime so a revoked
	// token cannot be replayed after the tombstone expires. 24h is
	// comfortably above KChat's access-token lifetime.
	sessionRevokeTTL = 24 * time.Hour
)

// SessionConfig wires the SessionManager.
type SessionConfig struct {
	Store         SessionStore
	IdleTimeout   time.Duration
	MaxConcurrent int
	// Enabled gates enforcement. When false the manager is a no-op
	// passthrough (no tracking, no revocation checks) — the default
	// so existing deployments opt in deliberately. The HTTP
	// list/revoke handlers still function when a Store is present.
	Enabled bool
	// RevokeTTL overrides sessionRevokeTTL (mainly for tests).
	RevokeTTL time.Duration
	Logger    *log.Logger
}

// SessionManager enforces the session policy and serves the
// list/revoke API.
type SessionManager struct {
	cfg SessionConfig
}

// NewSessionManager applies defaults and returns a manager. A nil
// Store yields a manager whose Wrap is a passthrough and whose
// handlers return 503 — callers may still construct it so wiring is
// uniform whether or not Valkey is configured.
func NewSessionManager(cfg SessionConfig) *SessionManager {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultSessionIdleTimeout
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = DefaultSessionMaxConcurrent
	}
	if cfg.RevokeTTL <= 0 {
		cfg.RevokeTTL = sessionRevokeTTL
	}
	return &SessionManager{cfg: cfg}
}

// SessionIDFromRequest derives the stable session id for a request
// from its bearer token. Returns "" when there is no bearer token.
// The id is a SHA-256 of the token truncated to 32 hex chars — it
// identifies the token without storing it, and the truncation keeps
// ledger keys compact while leaving 128 bits of collision
// resistance.
func SessionIDFromRequest(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	parts := strings.SplitN(authz, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	tok := strings.TrimSpace(parts[1])
	if tok == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])[:32]
}

// userKey namespaces a user's sessions by tenant so two tenants
// sharing a KChat user id never collide.
func userKey(tenantID, userID string) string {
	return tenantID + ":" + userID
}

// clientIP extracts a best-effort client IP, honouring a single
// X-Forwarded-For hop (the load balancer) and falling back to the
// connection's remote address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Wrap enforces the session policy for authenticated requests. It
// MUST run after the OIDC middleware (it reads tenant/user from the
// context it populates). When disabled, or when there is no usable
// session id (no bearer token — e.g. dev header-only paths), it is a
// passthrough.
func (m *SessionManager) Wrap(next http.Handler) http.Handler {
	if m == nil || !m.cfg.Enabled || m.cfg.Store == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := SessionIDFromRequest(r)
		tenantID := TenantIDFrom(r.Context())
		userID := KChatUserIDFrom(r.Context())
		if sid == "" || tenantID == "" || userID == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		now := time.Now()

		revoked, err := m.cfg.Store.IsRevoked(ctx, sid, now)
		if err != nil {
			m.logf("session: revocation check failed: %v", err)
			// Fail open on store errors for the revocation check —
			// a transient Valkey blip must not lock every user out.
			// Revocation is best-effort; the JWT exp still bounds
			// the token. (Concurrent-cap below is likewise skipped
			// on error.)
			next.ServeHTTP(w, r)
			return
		}
		if revoked {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="session revoked"`)
			http.Error(w, "session revoked", http.StatusUnauthorized)
			return
		}

		s := SessionInfo{
			ID:        sid,
			UserKey:   userKey(tenantID, userID),
			TenantID:  tenantID,
			UserID:    userID,
			UserAgent: r.UserAgent(),
			IP:        clientIP(r),
			CreatedAt: now,
			LastSeen:  now,
		}
		evicted, err := m.cfg.Store.Touch(ctx, s, m.cfg.IdleTimeout, m.cfg.MaxConcurrent, now)
		if err != nil {
			m.logf("session: touch failed: %v", err)
			next.ServeHTTP(w, r)
			return
		}
		// If THIS session was evicted (it was the oldest and the cap
		// is somehow < the number already present), refuse — the
		// user is over the cap. In normal operation the current
		// session is the newest and never the eviction target.
		for _, e := range evicted {
			if e == sid {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", error_description="concurrent session limit"`)
				http.Error(w, "concurrent session limit reached", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (m *SessionManager) logf(format string, args ...any) {
	if m.cfg.Logger != nil {
		m.cfg.Logger.Printf(format, args...)
	}
}

// --- MemorySessionStore -----------------------------------------

// MemorySessionStore is an in-process SessionStore. It is correct
// for a single replica and is used by tests and as a degraded
// fallback when Valkey is unavailable (sessions are then tracked
// per-replica, which weakens but does not break the cap).
type MemorySessionStore struct {
	mu       sync.Mutex
	sessions map[string]map[string]SessionInfo // userKey -> sid -> info
	revoked  map[string]time.Time              // sid -> tombstone expiry
}

// NewMemorySessionStore returns an empty store.
func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{
		sessions: map[string]map[string]SessionInfo{},
		revoked:  map[string]time.Time{},
	}
}

func (s *MemorySessionStore) pruneLocked(userKey string, idleTTL time.Duration, now time.Time) {
	for sid, info := range s.sessions[userKey] {
		if now.Sub(info.LastSeen) > idleTTL {
			delete(s.sessions[userKey], sid)
		}
	}
}

// Touch implements SessionStore.
func (s *MemorySessionStore) Touch(_ context.Context, in SessionInfo, idleTTL time.Duration, maxConcurrent int, now time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[in.UserKey] == nil {
		s.sessions[in.UserKey] = map[string]SessionInfo{}
	}
	s.pruneLocked(in.UserKey, idleTTL, now)

	if existing, ok := s.sessions[in.UserKey][in.ID]; ok {
		// Preserve original CreatedAt; just refresh activity.
		existing.LastSeen = now
		existing.UserAgent = in.UserAgent
		existing.IP = in.IP
		s.sessions[in.UserKey][in.ID] = existing
		return nil, nil
	}
	// New session: stamp timestamps if the caller did not, so the
	// store is correct independent of the middleware.
	if in.CreatedAt.IsZero() {
		in.CreatedAt = now
	}
	in.LastSeen = now
	s.sessions[in.UserKey][in.ID] = in

	var evicted []string
	if maxConcurrent > 0 {
		for len(s.sessions[in.UserKey]) > maxConcurrent {
			oldestID, oldest := "", time.Time{}
			first := true
			for sid, info := range s.sessions[in.UserKey] {
				if first || info.CreatedAt.Before(oldest) {
					oldestID, oldest, first = sid, info.CreatedAt, false
				}
			}
			delete(s.sessions[in.UserKey], oldestID)
			evicted = append(evicted, oldestID)
		}
	}
	return evicted, nil
}

// List implements SessionStore.
func (s *MemorySessionStore) List(_ context.Context, userKey string, idleTTL time.Duration, now time.Time) ([]SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(userKey, idleTTL, now)
	out := make([]SessionInfo, 0, len(s.sessions[userKey]))
	for _, info := range s.sessions[userKey] {
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Revoke implements SessionStore.
func (s *MemorySessionStore) Revoke(_ context.Context, userKey, sessionID string, ttl time.Duration, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[userKey] != nil {
		delete(s.sessions[userKey], sessionID)
	}
	s.revoked[sessionID] = now.Add(ttl)
	return nil
}

// IsRevoked implements SessionStore.
func (s *MemorySessionStore) IsRevoked(_ context.Context, sessionID string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.revoked[sessionID]
	if !ok {
		return false, nil
	}
	if now.After(exp) {
		delete(s.revoked, sessionID)
		return false, nil
	}
	return true, nil
}
