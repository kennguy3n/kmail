// Package billing — dunning service.
//
// Dunning is the lifecycle of failed-payment recovery: notify the
// customer, give them a window to fix the payment method, and
// suspend service if the issue isn't resolved. Phase 7 wires the
// minimum viable dunning flow against the existing Stripe
// webhook surface in `webhook.go`.
//
// Trigger: an `invoice.payment_failed` event arrives at the
// webhook. The webhook handler invokes `DunningService.Handle`
// with the parsed invoice. The service:
//
//   1. Records the failure on `billing_dunning_events`.
//   2. Posts a `payment_failed` notification to the tenant's
//      KChat workspace via `chatbridge.Service`.
//   3. After 3 failures inside a rolling 30-day window, sets
//      `tenants.status = 'suspended'`.
//   4. Emits an audit row for every state transition.
//
// The dunning event store reuses the existing `audit_log` table
// rather than introducing a new one — see the migration note in
// `docs/PROGRESS.md` Phase 7.
package billing

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

// dunningWindow is the rolling lookback we count failures over.
// Three failures inside the window force a suspension.
const dunningWindow = 30 * 24 * time.Hour

// dunningSuspendThreshold is the number of failures inside the
// dunningWindow that trigger a suspension.
const dunningSuspendThreshold = 3

// DunningEvent is the input handed to DunningService.Handle.
type DunningEvent struct {
	TenantID         string
	StripeInvoiceID  string
	StripeCustomerID string
	AmountDue        int64
	Currency         string
	OccurredAt       time.Time
}

// DunningNotifier is the slice of chatbridge.Service the dunning
// flow needs. Defining the interface here lets tests drop in a
// recording stub without spinning up real KChat traffic.
type DunningNotifier interface {
	NotifyPaymentFailed(ctx context.Context, tenantID, invoiceID string, amount int64, currency string) error
}

// DunningAuditor mirrors the slice of audit.Service we need for
// dunning. The narrow interface keeps the package decoupled from
// the rest of the audit surface.
type DunningAuditor interface {
	Log(ctx context.Context, tenantID, actor, action, resource string, meta map[string]any) error
}

// DunningConfig wires NewDunningService.
type DunningConfig struct {
	Pool     *pgxpool.Pool
	Logger   *log.Logger
	Notifier DunningNotifier
	Auditor  DunningAuditor
	// Now overrides time.Now for deterministic tests.
	Now func() time.Time
}

// DunningService handles `invoice.payment_failed` events.
type DunningService struct {
	cfg DunningConfig
}

// NewDunningService returns a DunningService with sensible
// defaults applied.
func NewDunningService(cfg DunningConfig) *DunningService {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &DunningService{cfg: cfg}
}

// Handle records a failure, posts the KChat notification, and
// suspends the tenant if the failure threshold has been hit.
func (s *DunningService) Handle(ctx context.Context, evt DunningEvent) error {
	if evt.TenantID == "" {
		return errors.New("dunning: tenant id required")
	}
	if evt.OccurredAt.IsZero() {
		evt.OccurredAt = s.cfg.Now()
	}
	failures, isNew, err := s.recordAndCount(ctx, evt)
	if err != nil {
		return err
	}
	// Stripe redelivers webhooks aggressively (every retry of the
	// same invoice.payment_failed event arrives with the same
	// stripe_invoice_id). recordAndCount dedupes the row via ON
	// CONFLICT DO NOTHING; gate the side-effecting calls on isNew
	// so retries don't spam the tenant's KChat workspace or the
	// audit log. The suspension check still uses the real failure
	// count, which is unaffected by retries.
	if isNew {
		if s.cfg.Notifier != nil {
			if err := s.cfg.Notifier.NotifyPaymentFailed(ctx, evt.TenantID, evt.StripeInvoiceID, evt.AmountDue, evt.Currency); err != nil {
				s.cfg.Logger.Printf("dunning: notify failed for tenant=%s: %v", evt.TenantID, err)
			}
		}
		if s.cfg.Auditor != nil {
			_ = s.cfg.Auditor.Log(ctx, evt.TenantID, "system:billing", "billing.payment_failed", evt.StripeInvoiceID, map[string]any{
				"failures_30d": failures,
				"amount_due":   evt.AmountDue,
				"currency":     evt.Currency,
			})
		}
	}
	if isNew && failures >= dunningSuspendThreshold {
		if err := s.suspend(ctx, evt.TenantID); err != nil {
			return err
		}
		if s.cfg.Auditor != nil {
			_ = s.cfg.Auditor.Log(ctx, evt.TenantID, "system:billing", "billing.tenant_suspended", evt.TenantID, map[string]any{
				"failures_30d": failures,
			})
		}
	}
	return nil
}

// recordAndCount inserts the failure into billing_dunning_events
// and returns the rolling-window failure count plus a flag
// indicating whether the row was actually inserted (true) or
// deduplicated by ON CONFLICT (false, which means this is a
// Stripe webhook retry of an event we've already processed).
// Pool may be nil in tests; in that case we return (1, true).
func (s *DunningService) recordAndCount(ctx context.Context, evt DunningEvent) (int, bool, error) {
	if s.cfg.Pool == nil {
		return 1, true, nil
	}
	cutoff := evt.OccurredAt.Add(-dunningWindow)
	var (
		count int
		isNew bool
	)
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, evt.TenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO billing_dunning_events (tenant_id, stripe_invoice_id, stripe_customer_id, amount_due, currency, occurred_at)
			VALUES ($1::uuid, $2, $3, $4, $5, $6)
			ON CONFLICT (stripe_invoice_id) DO NOTHING`,
			evt.TenantID, evt.StripeInvoiceID, evt.StripeCustomerID, evt.AmountDue, evt.Currency, evt.OccurredAt)
		if err != nil {
			return err
		}
		isNew = tag.RowsAffected() > 0
		row := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM billing_dunning_events
			 WHERE tenant_id = $1::uuid AND occurred_at >= $2`,
			evt.TenantID, cutoff)
		return row.Scan(&count)
	})
	if err != nil {
		return 0, false, fmt.Errorf("dunning record: %w", err)
	}
	return count, isNew, nil
}

// suspend flips the tenant's status to `suspended`. Idempotent:
// already-suspended tenants are not re-stamped.
func (s *DunningService) suspend(ctx context.Context, tenantID string) error {
	if s.cfg.Pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			UPDATE tenants
			   SET status = 'suspended'
			 WHERE id = $1::uuid AND status <> 'suspended'`, tenantID)
		return err
	})
}
