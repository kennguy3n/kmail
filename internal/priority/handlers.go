package priority

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/smartfeatures"
)

// Handlers exposes the Priority Inbox REST surface.
//
// Routes (behind the OIDC middleware):
//
//	GET /api/v1/priority-inbox        → top-scored emails (recompute)
//	GET /api/v1/priority-inbox?cached=1 → serve the cached ranking only
type Handlers struct {
	svc    *Service
	store  *Store
	logger *log.Logger
}

// NewHandlers constructs the surface. A nil service is a wiring
// bug and panics, matching the package convention.
func NewHandlers(svc *Service, store *Store, logger *log.Logger) *Handlers {
	if svc == nil {
		panic("priority.NewHandlers: svc is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, store: store, logger: logger}
}

// Register wires the routes behind the OIDC middleware.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("GET /api/v1/priority-inbox", authMW.Wrap(http.HandlerFunc(h.priorityInbox)))
}

// priorityItem is the wire shape for one ranked email. The
// metadata fields let the client render the Priority mailbox
// without a second round-trip to fetch each message.
type priorityItem struct {
	EmailID    string                  `json:"email_id"`
	ThreadID   string                  `json:"thread_id,omitempty"`
	Score      int                     `json:"score"`
	Subject    string                  `json:"subject"`
	Preview    string                  `json:"preview,omitempty"`
	From       []smartfeatures.Address `json:"from,omitempty"`
	ReceivedAt string                  `json:"received_at,omitempty"`
}

func (h *Handlers) priorityInbox(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	userID := middleware.KChatUserIDFrom(r.Context())
	if tenantID == "" || userID == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "missing auth context"})
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 20, 100)

	// cached=1 serves only the cached ranking ids+scores (cheap,
	// no Stalwart round-trip). Without it we recompute, which also
	// refreshes the cache as a side effect.
	if r.URL.Query().Get("cached") == "1" {
		if h.store == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "cache unavailable"})
			return
		}
		ids, err := h.store.Top(r.Context(), tenantID, userID, limit)
		if err != nil {
			h.logger.Printf("priority: cached read: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
			return
		}
		items := make([]priorityItem, 0, len(ids))
		for _, s := range ids {
			items = append(items, priorityItem{EmailID: s.EmailID, Score: s.Score})
		}
		writeJSON(w, http.StatusOK, map[string]any{"cached": true, "items": items})
		return
	}

	scored, err := h.svc.Compute(r.Context(), tenantID, userID, limit)
	if err != nil {
		h.logger.Printf("priority: compute: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "could not compute priority inbox"})
		return
	}
	items := make([]priorityItem, 0, len(scored))
	for _, sc := range scored {
		item := priorityItem{
			EmailID:  sc.Message.ID,
			ThreadID: sc.Message.ThreadID,
			Score:    sc.Score,
			Subject:  sc.Message.Subject,
			Preview:  sc.Message.Preview,
			From:     sc.Message.From,
		}
		if !sc.Message.ReceivedAt.IsZero() {
			item.ReceivedAt = sc.Message.ReceivedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"cached": false, "items": items})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func clampLimit(raw string, def, max int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	n := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		if n > max {
			return max
		}
	}
	if n <= 0 {
		return def
	}
	return n
}
