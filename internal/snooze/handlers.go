package snooze

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// manager is the slice of Service the REST surface depends on.
// Tests substitute an in-memory fake so the handlers can be
// exercised without a real Postgres pool.
//
// `Get` and `Cancel` take `kchatUserID` because the snooze authz
// model is per-user, not per-tenant (a snooze row is private to
// the user who created it). Threading the id through the
// interface forces the handler to extract it from the auth
// context and prevents a refactor from silently widening the
// scope back to tenant-only.
type manager interface {
	Snooze(ctx context.Context, in SnoozeInput) (*Snooze, error)
	Get(ctx context.Context, tenantID, kchatUserID, id string) (*Snooze, error)
	ListByUser(ctx context.Context, tenantID, kchatUserID string) ([]Snooze, error)
	Cancel(ctx context.Context, tenantID, kchatUserID, id string) error
}

// dispatcher is the subset of jmap.InternalClient the handlers
// use to apply the user-side mailbox patches. Both the "move to
// snoozed folder" patch (on POST) and the "move back to
// originals" patch (on DELETE) go through this surface.
type dispatcher interface {
	ResolveAccountID(ctx context.Context, tenantID, kchatUserID string) (string, error)
	Dispatch(ctx context.Context, tenantID, kchatUserID string, req jmap.JmapRequest) (*jmap.JmapResponse, error)
}

// Handlers exposes the snooze REST surface.
//
// Routes:
//
//	POST   /api/v1/snooze            → snooze an email
//	GET    /api/v1/snoozed           → list this user's snoozed emails
//	GET    /api/v1/snoozed/{id}      → fetch one row
//	DELETE /api/v1/snoozed/{id}      → wake an email immediately + flip to cancelled
//
// The DELETE path is "unsnooze now": it applies the same
// JMAP patch the worker would have applied at wake-time, so
// the email returns to its original mailboxes within the
// request lifetime. Status is recorded as `cancelled` (vs the
// worker's `unsnoozed`) so audits can distinguish user-driven
// early wakes from worker-driven ones.
type Handlers struct {
	svc        manager
	dispatcher dispatcher
	logger     *log.Logger
	now        func() time.Time
}

// NewHandlers constructs the REST surface. Panics on nil so a
// wiring bug in main.go is loud — same pattern as
// scheduledsend.NewHandlers. The logger is used for orphaned-
// snooze visibility: when the JMAP move succeeds but the DB
// persist fails AND the best-effort revert also fails, an
// operator must be able to find the email in the user's
// Snoozed folder and patch it back manually. Nil falls back to
// `log.Default()`.
func NewHandlers(svc *Service, d dispatcher, logger *log.Logger) *Handlers {
	if svc == nil {
		panic("snooze.NewHandlers: svc is required")
	}
	if d == nil {
		panic("snooze.NewHandlers: dispatcher is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	return &Handlers{svc: svc, dispatcher: d, logger: logger, now: time.Now}
}

// newHandlersWith is the test seam — both deps are interfaces,
// and the clock is injectable.
func newHandlersWith(m manager, d dispatcher, now func() time.Time) *Handlers {
	if now == nil {
		now = time.Now
	}
	return &Handlers{svc: m, dispatcher: d, logger: log.Default(), now: now}
}

// Register wires the routes behind the OIDC middleware. The
// middleware populates tenant_id + kchat_user_id on the request
// context so the handlers can authorize without parsing headers
// themselves.
func (h *Handlers) Register(mux *http.ServeMux, authMW *middleware.OIDC) {
	mux.Handle("POST /api/v1/snooze", authMW.Wrap(http.HandlerFunc(h.create)))
	mux.Handle("GET /api/v1/snoozed", authMW.Wrap(http.HandlerFunc(h.list)))
	mux.Handle("GET /api/v1/snoozed/{id}", authMW.Wrap(http.HandlerFunc(h.get)))
	mux.Handle("DELETE /api/v1/snoozed/{id}", authMW.Wrap(http.HandlerFunc(h.wakeNow)))
}

// createRequest is the wire body for POST /api/v1/snooze.
type createRequest struct {
	EmailID            string          `json:"email_id"`
	OriginalMailboxIDs map[string]bool `json:"original_mailbox_ids"`
	SnoozedMailboxID   string          `json:"snoozed_mailbox_id"`
	SnoozeUntil        time.Time       `json:"snooze_until"`
	MarkUnreadOnWake   *bool           `json:"mark_unread_on_wake,omitempty"`
}

func (h *Handlers) create(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.Context())
	if tenantID == "" || kchatUserID == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "missing auth context"})
		return
	}
	defer r.Body.Close()
	var body createRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}
	if strings.TrimSpace(body.EmailID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email_id is required"})
		return
	}
	if len(body.OriginalMailboxIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "original_mailbox_ids is required"})
		return
	}
	if strings.TrimSpace(body.SnoozedMailboxID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "snoozed_mailbox_id is required"})
		return
	}
	if body.SnoozeUntil.IsZero() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "snooze_until is required"})
		return
	}
	// Refuse a snooze whose target mailbox is also in the
	// "originals" set — the post-wake patch would otherwise
	// have to invent a deterministic ordering for null-vs-true
	// on the same mailbox key (JMAP rejects the patch).
	if body.OriginalMailboxIDs[body.SnoozedMailboxID] {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "snoozed_mailbox_id must not be present in original_mailbox_ids",
		})
		return
	}
	markUnread := true
	if body.MarkUnreadOnWake != nil {
		markUnread = *body.MarkUnreadOnWake
	}
	accountID, err := h.dispatcher.ResolveAccountID(r.Context(), tenantID, kchatUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "resolve stalwart account"})
		return
	}
	originalJSON, err := json.Marshal(body.OriginalMailboxIDs)
	if err != nil {
		// json.Marshal on a string→bool map cannot fail in
		// practice; surface as 500 if it ever does.
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "encode mailbox ids"})
		return
	}
	// Apply the "move to snoozed folder" patch BEFORE
	// persisting the row. If the patch fails — bad email id,
	// missing mailbox — we never persist a row that points at a
	// state Stalwart doesn't have. The reverse order would
	// leave a stale "snoozed" row for an email that's still in
	// its original mailbox.
	if err := h.applyMove(
		r.Context(),
		tenantID, kchatUserID, accountID, body.EmailID,
		body.OriginalMailboxIDs, body.SnoozedMailboxID,
		false,
	); err != nil {
		// Stalwart's verbatim error message is logged at
		// `applyMove` and is not re-surfaced to the client to
		// avoid leaking internal infrastructure detail (server
		// names, mailbox ids that didn't resolve, etc.). The
		// 502 is enough for the user; an operator pulls the
		// surrounding log lines for triage.
		h.logger.Printf("snooze: stalwart refused snooze move: tenant=%s user=%s email_id=%s err=%v", tenantID, kchatUserID, body.EmailID, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "stalwart refused move"})
		return
	}
	row, err := h.svc.Snooze(r.Context(), SnoozeInput{
		TenantID:           tenantID,
		KChatUserID:        kchatUserID,
		StalwartAccountID:  accountID,
		EmailID:            body.EmailID,
		OriginalMailboxIDs: originalJSON,
		SnoozedMailboxID:   body.SnoozedMailboxID,
		SnoozeUntil:        body.SnoozeUntil,
		MarkUnreadOnWake:   markUnread,
	})
	if err != nil {
		// Persistence failed — best-effort revert the JMAP
		// patch so the user doesn't see a phantom-snoozed
		// email. The revert is the WAKE-patch shape: pass the
		// original mailbox set as `originals` AND the snoozed
		// folder as `snoozedMailboxID`, with
		// `restoreOriginals=true` so `applyMove` adds the
		// originals back and drops the snoozed folder. If the
		// revert itself fails we still surface the original
		// error to the client AND log the orphan state at
		// ERROR level so an operator can find the email in
		// the user's Snoozed folder and patch it back
		// manually. Without the log the orphan would only be
		// recoverable via the user noticing a phantom-snoozed
		// email — silent data loss from an operations
		// standpoint.
		if revertErr := h.applyMove(
			r.Context(),
			tenantID, kchatUserID, accountID, body.EmailID,
			body.OriginalMailboxIDs, body.SnoozedMailboxID,
			true,
		); revertErr != nil {
			h.logger.Printf(
				"snooze: ORPHAN — email moved to snoozed folder but DB persist failed AND revert failed; "+
					"manual patch required: tenant_id=%s kchat_user_id=%s stalwart_account_id=%s email_id=%s snoozed_mailbox_id=%s persist_err=%v revert_err=%v",
				tenantID, kchatUserID, accountID, body.EmailID, body.SnoozedMailboxID, err, revertErr,
			)
		}
		switch {
		case errors.Is(err, ErrInvalidSnooze):
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		case errors.Is(err, ErrAlreadySnoozed):
			writeJSON(w, http.StatusConflict, map[string]any{"error": "email is already snoozed"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, toResponse(row))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.Context())
	if tenantID == "" || kchatUserID == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "missing auth context"})
		return
	}
	rows, err := h.svc.ListByUser(r.Context(), tenantID, kchatUserID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snoozes": toResponses(rows)})
}

func (h *Handlers) get(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.Context())
	if tenantID == "" || kchatUserID == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "missing auth context"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
		return
	}
	// Per-USER scope (not just tenant): a snooze row is owned by
	// the user that created it, and an inadvertent fall-through
	// to tenant-only scoping would let any user in the tenant
	// read another user's snoozed-email metadata (mailbox-id
	// sets, EmailIDs) by guessing UUIDs.
	row, err := h.svc.Get(r.Context(), tenantID, kchatUserID, id)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, toResponse(row))
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrTenantMismatch):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
	}
}

// wakeNow handles DELETE /api/v1/snoozed/{id}: applies the
// reverse JMAP patch (restore originals, drop snoozed) and
// marks the row cancelled. This is the user-initiated early
// wake — distinct from the worker's natural wake at
// snooze_until.
func (h *Handlers) wakeNow(w http.ResponseWriter, r *http.Request) {
	tenantID := middleware.TenantIDFrom(r.Context())
	kchatUserID := middleware.KChatUserIDFrom(r.Context())
	if tenantID == "" || kchatUserID == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "missing auth context"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing id"})
		return
	}
	// Per-USER scope (not just tenant): without this, any user
	// in the tenant could wake another user's snoozed email by
	// guessing UUIDs.
	row, err := h.svc.Get(r.Context(), tenantID, kchatUserID, id)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrTenantMismatch):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	if row.Status != StatusSnoozed {
		// Already terminal — make the call idempotent: 200
		// with the same body the happy path returns, so a
		// double-click from the UI doesn't surface as an
		// error.
		writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
		return
	}
	var orig map[string]bool
	if err := json.Unmarshal(row.OriginalMailboxIDs, &orig); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "decode mailbox ids"})
		return
	}
	if err := h.applyMove(
		r.Context(),
		row.TenantID, row.KChatUserID, row.StalwartAccountID, row.EmailID,
		orig, row.SnoozedMailboxID,
		true,
	); err != nil {
		// As in `create`: don't echo Stalwart's verbatim
		// error to the client — log it, return a generic 502.
		h.logger.Printf("snooze: stalwart refused wake: tenant=%s user=%s email_id=%s err=%v", tenantID, kchatUserID, row.EmailID, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "stalwart refused wake"})
		return
	}
	if err := h.svc.Cancel(r.Context(), tenantID, kchatUserID, id); err != nil {
		// JMAP move succeeded but DB flip failed — surface as
		// 500 so an operator can investigate; the worker
		// won't re-dispatch because the email is already at
		// its target location.
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

// applyMove builds and dispatches the JMAP `Email/set update`
// patch that moves an email between the snoozed folder and its
// original mailboxes.
//
// `restoreOriginals=true` produces the wake patch (add the
// originals, drop the snoozed folder, optionally clear $seen).
// `restoreOriginals=false` produces the snooze patch (drop the
// originals, add the snoozed folder; $seen is left alone — the
// user might have already opened the email).
func (h *Handlers) applyMove(
	ctx context.Context,
	tenantID, kchatUserID, accountID, emailID string,
	originals map[string]bool,
	snoozedMailboxID string,
	restoreOriginals bool,
) error {
	patch := make(map[string]any, len(originals)+2)
	if restoreOriginals {
		for mb := range originals {
			if mb == "" {
				continue
			}
			patch["mailboxIds/"+mb] = true
		}
		if snoozedMailboxID != "" {
			patch["mailboxIds/"+snoozedMailboxID] = nil
		}
	} else {
		// Snoozing: drop every original, add the snoozed
		// folder. Drop set is closed under `originals` so a
		// later expansion of "original mailboxes" doesn't
		// leak.
		for mb := range originals {
			if mb == "" {
				continue
			}
			patch["mailboxIds/"+mb] = nil
		}
		patch["mailboxIds/"+snoozedMailboxID] = true
	}
	req := jmap.JmapRequest{
		Using: []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:mail",
		},
		MethodCalls: [][]any{
			{
				"Email/set",
				map[string]any{
					"accountId": accountID,
					"update":    map[string]any{emailID: patch},
				},
				"snz",
			},
		},
	}
	resp, err := h.dispatcher.Dispatch(ctx, tenantID, kchatUserID, req)
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("snooze: empty dispatch response")
	}
	_, args, ok := resp.CallByID("snz")
	if !ok {
		return errors.New("snooze: dispatch response missing client id")
	}
	if notUpdated, ok := args["notUpdated"].(map[string]any); ok {
		if reason, ok := notUpdated[emailID]; ok {
			return formatNotUpdated(reason)
		}
	}
	return nil
}

func formatNotUpdated(reason any) error {
	if m, ok := reason.(map[string]any); ok {
		typ, _ := m["type"].(string)
		desc, _ := m["description"].(string)
		if desc != "" {
			return errors.New(typ + ": " + desc)
		}
		if typ != "" {
			return errors.New(typ)
		}
	}
	b, _ := json.Marshal(reason)
	return errors.New(string(b))
}

// responsePayload is the wire shape we expose for snooze rows.
// We do NOT echo back the original_mailbox_ids — they're
// effectively a private restoration token; exposing them gives
// the client no value (the React UI doesn't need to render
// per-mailbox state) and would force an extra "Mailbox/get"
// round-trip to translate ids to names.
type responsePayload struct {
	ID                string  `json:"id"`
	Status            string  `json:"status"`
	EmailID           string  `json:"email_id"`
	SnoozedMailboxID  string  `json:"snoozed_mailbox_id"`
	SnoozeUntil       string  `json:"snooze_until"`
	MarkUnreadOnWake  bool    `json:"mark_unread_on_wake"`
	Attempts          int     `json:"attempts"`
	LastError         string  `json:"last_error,omitempty"`
	WokenAt           *string `json:"woken_at,omitempty"`
	CreatedAt         string  `json:"created_at"`
}

func toResponse(s *Snooze) responsePayload {
	var wokenAt *string
	if s.WokenAt != nil {
		w := s.WokenAt.UTC().Format(time.RFC3339)
		wokenAt = &w
	}
	return responsePayload{
		ID:               s.ID,
		Status:           s.Status,
		EmailID:          s.EmailID,
		SnoozedMailboxID: s.SnoozedMailboxID,
		SnoozeUntil:      s.SnoozeUntil.UTC().Format(time.RFC3339),
		MarkUnreadOnWake: s.MarkUnreadOnWake,
		Attempts:         s.Attempts,
		LastError:        s.LastError,
		WokenAt:          wokenAt,
		CreatedAt:        s.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func toResponses(rows []Snooze) []responsePayload {
	out := make([]responsePayload, len(rows))
	for i := range rows {
		out[i] = toResponse(&rows[i])
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
