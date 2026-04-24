package deliverability

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Bounce types accepted by ProcessBounce.
const (
	BounceHard      = "hard"
	BounceSoft      = "soft"
	BounceComplaint = "complaint"
)

// BounceEvent is the API representation of a row in
// `bounce_events`.
type BounceEvent struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	Email      string    `json:"email"`
	BounceType string    `json:"bounce_type"`
	DSNCode    string    `json:"dsn_code"`
	Diagnostic string    `json:"diagnostic"`
	MessageID  string    `json:"message_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// BounceProcessor owns the `bounce_events` table and the
// auto-escalation rule that moves repeat soft-bouncers onto the
// suppression list.
type BounceProcessor struct {
	pool              *pgxpool.Pool
	suppression       *SuppressionService
	softEscalateCount int
	softEscalateWin   time.Duration
}

// ProcessBounce persists a bounce event and, per the configured
// policy, auto-suppresses the recipient when:
//
//   - BounceType == "hard" or "complaint": immediate suppression.
//   - BounceType == "soft": suppression only after
//     `softEscalateCount` soft bounces within `softEscalateWin`.
func (b *BounceProcessor) ProcessBounce(ctx context.Context, tenantID string, evt BounceEvent) (*BounceEvent, error) {
	if tenantID == "" || evt.Email == "" {
		return nil, fmt.Errorf("%w: tenantID and email required", ErrInvalidInput)
	}
	switch evt.BounceType {
	case BounceHard, BounceSoft, BounceComplaint:
	default:
		return nil, fmt.Errorf("%w: invalid bounce_type %q", ErrInvalidInput, evt.BounceType)
	}
	evt.Email = normalizeEmail(evt.Email)
	var recent int
	if b.pool != nil {
		err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
			if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `
				INSERT INTO bounce_events (
					tenant_id, email, bounce_type, dsn_code, diagnostic, message_id
				) VALUES ($1::uuid, $2, $3, $4, $5, $6)
				RETURNING id::text, tenant_id::text, email, bounce_type,
				          dsn_code, diagnostic, message_id, created_at
			`, tenantID, evt.Email, evt.BounceType, evt.DSNCode, evt.Diagnostic, evt.MessageID,
			).Scan(
				&evt.ID, &evt.TenantID, &evt.Email, &evt.BounceType,
				&evt.DSNCode, &evt.Diagnostic, &evt.MessageID, &evt.CreatedAt,
			); err != nil {
				return err
			}
			// Count recent soft bounces so the escalation rule runs
			// inside the same transaction as the insert.
			if evt.BounceType == BounceSoft {
				since := time.Now().Add(-b.softEscalateWin)
				if err := tx.QueryRow(ctx, `
					SELECT COUNT(*)::int FROM bounce_events
					WHERE tenant_id = $1::uuid AND email = $2
					  AND bounce_type = 'soft'
					  AND created_at >= $3
				`, tenantID, evt.Email, since).Scan(&recent); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("record bounce: %w", err)
		}
	}

	switch evt.BounceType {
	case BounceHard:
		_, _ = b.suppression.AddSuppression(ctx, tenantID, evt.Email, ReasonHardBounce, "bounce-processor")
	case BounceComplaint:
		_, _ = b.suppression.AddSuppression(ctx, tenantID, evt.Email, ReasonComplaint, "bounce-processor")
	case BounceSoft:
		if recent >= b.softEscalateCount {
			_, _ = b.suppression.AddSuppression(ctx, tenantID, evt.Email, ReasonHardBounce, "bounce-processor-soft-escalation")
		}
	}
	return &evt, nil
}

// ShouldEscalateSoft is the pure decision function used by
// ProcessBounce and unit tests. Exposed to keep bounce escalation
// logic independently testable from the database path.
func (b *BounceProcessor) ShouldEscalateSoft(recent int) bool {
	return recent >= b.softEscalateCount
}

// ListBounces returns the bounce log for the tenant.
func (b *BounceProcessor) ListBounces(ctx context.Context, tenantID string, limit, offset int) ([]BounceEvent, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	if b.pool == nil {
		return nil, nil
	}
	var out []BounceEvent
	err := pgx.BeginFunc(ctx, b.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, tenant_id::text, email, bounce_type,
			       dsn_code, diagnostic, message_id, created_at
			FROM bounce_events
			WHERE tenant_id = $1::uuid
			ORDER BY created_at DESC
			LIMIT $2 OFFSET $3
		`, tenantID, limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r BounceEvent
			if err := rows.Scan(
				&r.ID, &r.TenantID, &r.Email, &r.BounceType,
				&r.DSNCode, &r.Diagnostic, &r.MessageID, &r.CreatedAt,
			); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("list bounces: %w", err)
	}
	return out, nil
}
