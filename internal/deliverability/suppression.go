package deliverability

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Suppression reasons accepted by AddSuppression.
const (
	ReasonHardBounce  = "hard_bounce"
	ReasonComplaint   = "complaint"
	ReasonManual      = "manual"
	ReasonUnsubscribe = "unsubscribe"
)

// Suppression is the API representation of a row in
// `suppression_list`.
type Suppression struct {
	ID        string    `json:"id"`
	TenantID  string    `json:"tenant_id"`
	Email     string    `json:"email"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// SuppressionService owns the `suppression_list` table.
type SuppressionService struct {
	pool *pgxpool.Pool
}

// ListSuppressionsOptions controls the paginated listing call.
type ListSuppressionsOptions struct {
	Reason string
	Limit  int
	Offset int
}

// AddSuppression inserts or updates the (tenant, email) tuple on
// the suppression list. Re-adding an address with a different
// reason updates the row rather than inserting a duplicate.
func (s *SuppressionService) AddSuppression(ctx context.Context, tenantID, email, reason, source string) (*Suppression, error) {
	if tenantID == "" || email == "" {
		return nil, fmt.Errorf("%w: tenantID and email required", ErrInvalidInput)
	}
	if !isValidReason(reason) {
		return nil, fmt.Errorf("%w: invalid reason %q", ErrInvalidInput, reason)
	}
	email = normalizeEmail(email)
	if s.pool == nil {
		return nil, fmt.Errorf("suppression service: no pool")
	}
	var row Suppression
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO suppression_list (tenant_id, email, reason, source)
			VALUES ($1::uuid, $2, $3, $4)
			ON CONFLICT (tenant_id, email) DO UPDATE
			    SET reason = EXCLUDED.reason,
			        source = EXCLUDED.source
			RETURNING id::text, tenant_id::text, email, reason, source, created_at
		`, tenantID, email, reason, source).Scan(
			&row.ID, &row.TenantID, &row.Email, &row.Reason, &row.Source, &row.CreatedAt,
		)
	})
	if err != nil {
		return nil, fmt.Errorf("insert suppression: %w", err)
	}
	return &row, nil
}

// RemoveSuppression clears a (tenant, email) row from the
// suppression list. Returns ErrNotFound when the row doesn't exist.
func (s *SuppressionService) RemoveSuppression(ctx context.Context, tenantID, email string) error {
	if tenantID == "" || email == "" {
		return fmt.Errorf("%w: tenantID and email required", ErrInvalidInput)
	}
	email = normalizeEmail(email)
	if s.pool == nil {
		return ErrNotFound
	}
	var affected int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			DELETE FROM suppression_list
			WHERE tenant_id = $1::uuid AND email = $2
		`, tenantID, email)
		if err != nil {
			return err
		}
		affected = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete suppression: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// IsSuppressed returns true when the email address is on the
// tenant's suppression list.
func (s *SuppressionService) IsSuppressed(ctx context.Context, tenantID, email string) (bool, error) {
	if tenantID == "" || email == "" {
		return false, fmt.Errorf("%w: tenantID and email required", ErrInvalidInput)
	}
	email = normalizeEmail(email)
	if s.pool == nil {
		return false, nil
	}
	var exists bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM suppression_list
				WHERE tenant_id = $1::uuid AND email = $2
			)
		`, tenantID, email).Scan(&exists)
	})
	if err != nil {
		return false, fmt.Errorf("check suppression: %w", err)
	}
	return exists, nil
}

// CheckRecipient is the pre-send hook called by the JMAP proxy
// before `EmailSubmission/set`. It returns ErrSuppressed when the
// recipient is on the list so the proxy can short-circuit the
// submission with a descriptive error.
func (s *SuppressionService) CheckRecipient(ctx context.Context, tenantID, recipientEmail string) error {
	suppressed, err := s.IsSuppressed(ctx, tenantID, recipientEmail)
	if err != nil {
		return err
	}
	if suppressed {
		return fmt.Errorf("%w: %s", ErrSuppressed, recipientEmail)
	}
	return nil
}

// ListSuppressions returns the suppression list for the tenant
// with optional paging and reason filter.
func (s *SuppressionService) ListSuppressions(ctx context.Context, tenantID string, opts ListSuppressionsOptions) ([]Suppression, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if opts.Limit <= 0 || opts.Limit > 500 {
		opts.Limit = 100
	}
	if opts.Offset < 0 {
		opts.Offset = 0
	}
	if s.pool == nil {
		return nil, nil
	}
	var out []Suppression
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, email, reason, source, created_at
			FROM suppression_list
			WHERE tenant_id = $1::uuid
			  AND ($2 = '' OR reason = $2)
			ORDER BY created_at DESC
			LIMIT $3 OFFSET $4
		`, tenantID, opts.Reason, opts.Limit, opts.Offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Suppression
			if err := rows.Scan(
				&r.ID, &r.TenantID, &r.Email, &r.Reason, &r.Source, &r.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list suppressions: %w", err)
	}
	return out, nil
}

func isValidReason(r string) bool {
	switch r {
	case ReasonHardBounce, ReasonComplaint, ReasonManual, ReasonUnsubscribe:
		return true
	default:
		return false
	}
}

func normalizeEmail(e string) string {
	return strings.ToLower(strings.TrimSpace(e))
}

// Sentinel re-export for readability at call sites.
var _ = errors.New
