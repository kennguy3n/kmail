// Package sharedinbox implements the assignment / notes / status
// workflow built on top of the existing shared-inbox CRUD in
// `internal/tenant/service.go`. The CRUD side owns the membership
// ACL; this package owns the per-email state machine.
package sharedinbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// ErrInvalidInput wraps caller-visible validation failures.
var ErrInvalidInput = errors.New("invalid input")

// ErrNotFound is returned when a row lookup resolves nothing.
var ErrNotFound = errors.New("not found")

// Assignment status values. The state machine is:
//   open → in_progress → waiting → resolved → closed
const (
	StatusOpen       = "open"
	StatusInProgress = "in_progress"
	StatusWaiting    = "waiting"
	StatusResolved   = "resolved"
	StatusClosed     = "closed"
)

// validStatus returns true when `s` is a recognised status value.
func validStatus(s string) bool {
	switch s {
	case StatusOpen, StatusInProgress, StatusWaiting, StatusResolved, StatusClosed:
		return true
	default:
		return false
	}
}

// EmailAssignment is the API + persisted shape of one email
// assignment row.
type EmailAssignment struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenant_id"`
	SharedInboxID   string    `json:"shared_inbox_id"`
	EmailID         string    `json:"email_id"`
	AssigneeUserID  string    `json:"assignee_user_id"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// InternalNote is a thread-internal note visible only to shared
// inbox members.
type InternalNote struct {
	ID             string    `json:"id"`
	TenantID       string    `json:"tenant_id"`
	SharedInboxID  string    `json:"shared_inbox_id"`
	EmailID        string    `json:"email_id"`
	AuthorUserID   string    `json:"author_user_id"`
	NoteText       string    `json:"note_text"`
	CreatedAt      time.Time `json:"created_at"`
}

// ListAssignmentsOptions filters the listing.
type ListAssignmentsOptions struct {
	Status         string
	AssigneeUserID string
	Limit          int
	Offset         int
}

// WorkflowService owns the `shared_inbox_assignments` /
// `shared_inbox_notes` tables.
type WorkflowService struct {
	Pool   *pgxpool.Pool
	Logger *log.Logger

	// MLS is the optional shared-inbox MLS group manager. When
	// non-nil and `Enabled()`, member-add / member-remove paths
	// dispatch to its `RotateGroup` to mint a fresh epoch so the
	// next message bound for this inbox is encrypted to the
	// updated member set. (Phase 8 wiring; see mls.go.)
	MLS MLSGroupManager
}

// NewService constructs a WorkflowService.
func NewService(pool *pgxpool.Pool, logger *log.Logger) *WorkflowService {
	if logger == nil {
		logger = log.Default()
	}
	return &WorkflowService{Pool: pool, Logger: logger}
}

// WithMLS attaches an MLSGroupManager. Returning the receiver
// keeps the call chainable in main.go.
func (s *WorkflowService) WithMLS(m MLSGroupManager) *WorkflowService {
	if s == nil {
		return s
	}
	s.MLS = m
	return s
}

// HandleMembershipChange invokes the MLS group manager when the
// underlying tenant Service has just mutated `shared_inbox_members`.
// Errors are logged but never returned: rotation is best-effort
// because failing the membership change because KChat is down is
// worse than briefly running on the previous epoch (the next
// rotation will re-converge).
func (s *WorkflowService) HandleMembershipChange(ctx context.Context, inboxID string, members []string, reason string) {
	if s == nil || s.MLS == nil {
		return
	}
	if !s.MLS.Enabled() {
		s.Logger.Printf("sharedinbox MLS: KCHAT_MLS_ENDPOINT empty — skipping rotation for inbox %s (%s)", inboxID, reason)
		return
	}
	if _, err := s.MLS.RotateGroup(ctx, inboxID, members, reason); err != nil {
		s.Logger.Printf("sharedinbox MLS: rotate inbox=%s reason=%s err=%v", inboxID, reason, err)
	}
}

// AssignEmail assigns (or reassigns) an email to a user. Creates
// the row on first call.
func (s *WorkflowService) AssignEmail(ctx context.Context, tenantID, sharedInboxID, emailID, assigneeUserID string) (*EmailAssignment, error) {
	if tenantID == "" || sharedInboxID == "" || emailID == "" || assigneeUserID == "" {
		return nil, fmt.Errorf("%w: tenantID, sharedInboxID, emailID, assigneeUserID required", ErrInvalidInput)
	}
	return s.upsertAssignment(ctx, tenantID, sharedInboxID, emailID, &assigneeUserID, nil)
}

// UnassignEmail removes the assignee but keeps the row so status
// history is preserved.
func (s *WorkflowService) UnassignEmail(ctx context.Context, tenantID, sharedInboxID, emailID string) error {
	if tenantID == "" || sharedInboxID == "" || emailID == "" {
		return fmt.Errorf("%w: tenantID, sharedInboxID, emailID required", ErrInvalidInput)
	}
	if s.Pool == nil {
		return nil
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		cmd, err := tx.Exec(ctx, `
			UPDATE shared_inbox_assignments
			SET assignee_user_id = NULL, updated_at = now()
			WHERE tenant_id = $1::uuid AND shared_inbox_id = $2::uuid AND email_id = $3
		`, tenantID, sharedInboxID, emailID)
		if err != nil {
			return err
		}
		if cmd.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("unassign email: %w", err)
	}
	return nil
}

// SetStatus updates the assignment status.
func (s *WorkflowService) SetStatus(ctx context.Context, tenantID, sharedInboxID, emailID, status string) (*EmailAssignment, error) {
	if !validStatus(status) {
		return nil, fmt.Errorf("%w: invalid status %q", ErrInvalidInput, status)
	}
	return s.upsertAssignment(ctx, tenantID, sharedInboxID, emailID, nil, &status)
}

// ListAssignments paginates through a shared inbox's assignments.
func (s *WorkflowService) ListAssignments(ctx context.Context, tenantID, sharedInboxID string, opts ListAssignmentsOptions) ([]EmailAssignment, error) {
	if tenantID == "" || sharedInboxID == "" {
		return nil, fmt.Errorf("%w: tenantID and sharedInboxID required", ErrInvalidInput)
	}
	if opts.Limit <= 0 {
		opts.Limit = 100
	}
	if opts.Limit > 500 {
		opts.Limit = 500
	}
	if s.Pool == nil {
		return nil, nil
	}
	var out []EmailAssignment
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		args := []any{tenantID, sharedInboxID}
		where := `tenant_id = $1::uuid AND shared_inbox_id = $2::uuid`
		if opts.Status != "" {
			args = append(args, opts.Status)
			where += fmt.Sprintf(` AND status = $%d`, len(args))
		}
		if opts.AssigneeUserID != "" {
			args = append(args, opts.AssigneeUserID)
			where += fmt.Sprintf(` AND assignee_user_id = $%d`, len(args))
		}
		args = append(args, opts.Limit, opts.Offset)
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, shared_inbox_id::text, email_id,
			       COALESCE(assignee_user_id, '') AS assignee_user_id,
			       status, created_at, updated_at
			FROM shared_inbox_assignments
			WHERE `+where+`
			ORDER BY updated_at DESC
			LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a EmailAssignment
			if err := rows.Scan(&a.ID, &a.TenantID, &a.SharedInboxID, &a.EmailID, &a.AssigneeUserID, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
				return err
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list assignments: %w", err)
	}
	return out, nil
}

// AddNote appends a timeline note.
func (s *WorkflowService) AddNote(ctx context.Context, tenantID, sharedInboxID, emailID, authorUserID, noteText string) (*InternalNote, error) {
	if tenantID == "" || sharedInboxID == "" || emailID == "" || authorUserID == "" {
		return nil, fmt.Errorf("%w: tenantID, sharedInboxID, emailID, authorUserID required", ErrInvalidInput)
	}
	if noteText == "" {
		return nil, fmt.Errorf("%w: note_text required", ErrInvalidInput)
	}
	out := InternalNote{TenantID: tenantID, SharedInboxID: sharedInboxID, EmailID: emailID, AuthorUserID: authorUserID, NoteText: noteText}
	if s.Pool == nil {
		out.ID = "stub"
		out.CreatedAt = time.Now()
		return &out, nil
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO shared_inbox_notes (tenant_id, shared_inbox_id, email_id, author_user_id, note_text)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5)
			RETURNING id::text, created_at
		`, tenantID, sharedInboxID, emailID, authorUserID, noteText).Scan(&out.ID, &out.CreatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("add note: %w", err)
	}
	return &out, nil
}

// ListNotes returns every note on an email thread, oldest-first.
func (s *WorkflowService) ListNotes(ctx context.Context, tenantID, sharedInboxID, emailID string) ([]InternalNote, error) {
	if tenantID == "" || sharedInboxID == "" || emailID == "" {
		return nil, fmt.Errorf("%w: tenantID, sharedInboxID, emailID required", ErrInvalidInput)
	}
	if s.Pool == nil {
		return nil, nil
	}
	var out []InternalNote
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, shared_inbox_id::text, email_id,
			       author_user_id, note_text, created_at
			FROM shared_inbox_notes
			WHERE tenant_id = $1::uuid AND shared_inbox_id = $2::uuid AND email_id = $3
			ORDER BY created_at ASC
		`, tenantID, sharedInboxID, emailID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n InternalNote
			if err := rows.Scan(&n.ID, &n.TenantID, &n.SharedInboxID, &n.EmailID, &n.AuthorUserID, &n.NoteText, &n.CreatedAt); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	return out, nil
}

// upsertAssignment is the shared INSERT ... ON CONFLICT path used
// by both AssignEmail and SetStatus. Either assignee or status is
// updated depending on which pointer is non-nil.
func (s *WorkflowService) upsertAssignment(ctx context.Context, tenantID, sharedInboxID, emailID string, assignee, status *string) (*EmailAssignment, error) {
	out := EmailAssignment{TenantID: tenantID, SharedInboxID: sharedInboxID, EmailID: emailID, Status: StatusOpen}
	if assignee != nil {
		out.AssigneeUserID = *assignee
	}
	if status != nil {
		out.Status = *status
	}
	if s.Pool == nil {
		out.ID = "stub"
		out.CreatedAt = time.Now()
		out.UpdatedAt = out.CreatedAt
		return &out, nil
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var assigneeArg any = nil
		if assignee != nil {
			assigneeArg = *assignee
		}
		statusArg := StatusOpen
		if status != nil {
			statusArg = *status
		}
		return tx.QueryRow(ctx, `
			INSERT INTO shared_inbox_assignments (
				tenant_id, shared_inbox_id, email_id, assignee_user_id, status
			)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5)
			ON CONFLICT (tenant_id, shared_inbox_id, email_id)
			DO UPDATE SET
				assignee_user_id = COALESCE($4, shared_inbox_assignments.assignee_user_id),
				status = CASE WHEN $6::boolean THEN $5 ELSE shared_inbox_assignments.status END,
				updated_at = now()
			RETURNING id::text, COALESCE(assignee_user_id, '') AS assignee_user_id, status, created_at, updated_at
		`, tenantID, sharedInboxID, emailID, assigneeArg, statusArg, status != nil,
		).Scan(&out.ID, &out.AssigneeUserID, &out.Status, &out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("upsert assignment: %w", err)
	}
	return &out, nil
}
