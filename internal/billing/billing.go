// Package billing hosts the Billing / Quota Service business
// logic: storage accounting, seat accounting, plan enforcement,
// pooled storage quota checks, and invoice generation.
//
// Authoritative for the `quotas` table in docs/SCHEMA.md §5.7 and
// the `billing_events` append-only log added in
// `migrations/005_billing.sql`. See docs/ARCHITECTURE.md §7 and
// docs/PROPOSAL.md §11.
package billing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Plan identifiers accepted by the Billing Service. Mirrors the
// CHECK constraint on `tenants.plan` and the pricing tiers
// documented in docs/PROPOSAL.md §11.
const (
	PlanCore    = "core"
	PlanPro     = "pro"
	PlanPrivacy = "privacy"
)

// ErrNotFound is returned when a lookup resolves no rows.
var ErrNotFound = errors.New("not found")

// ErrInvalidInput wraps caller-visible validation failures.
var ErrInvalidInput = errors.New("invalid input")

// ErrQuotaExceeded is returned by EnforcePlanLimits and
// CheckStorageQuota when the tenant is over its seat or storage
// pool. Handlers surface this as HTTP 402 Payment Required so
// clients distinguish "over quota" from "forbidden".
var ErrQuotaExceeded = errors.New("quota exceeded")

// Config wires the Billing Service.
type Config struct {
	// Pool is the control-plane Postgres pool.
	Pool *pgxpool.Pool

	// Per-seat price in cents. Defaults to the KChat pricing tiers
	// ($3 / $6 / $9 per seat). Override from env via Load.
	CoreSeatCents    int
	ProSeatCents     int
	PrivacySeatCents int

	// Per-seat default storage quota, in bytes, used when a new
	// tenant has no explicit `storage_limit_bytes` set. Defaults to
	// 5 GB / 15 GB / 50 GB.
	CorePerSeatBytes    int64
	ProPerSeatBytes     int64
	PrivacyPerSeatBytes int64
}

// Service is the Billing / Quota service.
type Service struct {
	cfg Config
}

// NewService returns a Billing Service with defaults applied.
func NewService(cfg Config) *Service {
	if cfg.CoreSeatCents <= 0 {
		cfg.CoreSeatCents = 300
	}
	if cfg.ProSeatCents <= 0 {
		cfg.ProSeatCents = 600
	}
	if cfg.PrivacySeatCents <= 0 {
		cfg.PrivacySeatCents = 900
	}
	if cfg.CorePerSeatBytes <= 0 {
		cfg.CorePerSeatBytes = 5 * 1024 * 1024 * 1024
	}
	if cfg.ProPerSeatBytes <= 0 {
		cfg.ProPerSeatBytes = 15 * 1024 * 1024 * 1024
	}
	if cfg.PrivacyPerSeatBytes <= 0 {
		cfg.PrivacyPerSeatBytes = 50 * 1024 * 1024 * 1024
	}
	return &Service{cfg: cfg}
}

// Quota is the API representation of a `quotas` row.
type Quota struct {
	TenantID          string    `json:"tenant_id"`
	StorageUsedBytes  int64     `json:"storage_used_bytes"`
	StorageLimitBytes int64     `json:"storage_limit_bytes"`
	SeatCount         int       `json:"seat_count"`
	SeatLimit         int       `json:"seat_limit"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// BillingSummary bundles the tenant's plan, quota, per-seat price,
// and estimated monthly invoice for the admin console.
type BillingSummary struct {
	TenantID       string `json:"tenant_id"`
	Plan           string `json:"plan"`
	PerSeatCents   int    `json:"per_seat_cents"`
	SeatCount      int    `json:"seat_count"`
	SeatLimit      int    `json:"seat_limit"`
	MonthlyCents   int64  `json:"monthly_cents"`
	StorageUsed    int64  `json:"storage_used_bytes"`
	StorageLimit   int64  `json:"storage_limit_bytes"`
	StoragePercent int    `json:"storage_percent"`
}

// UpdateQuotaLimitsInput carries the optional fields accepted by
// PATCH /api/v1/tenants/{id}/billing.
type UpdateQuotaLimitsInput struct {
	StorageLimitBytes *int64 `json:"storage_limit_bytes,omitempty"`
	SeatLimit         *int   `json:"seat_limit,omitempty"`
}

// GetPlanPricing returns the per-seat price in cents for the given
// plan. Unknown plans are rejected with ErrInvalidInput so callers
// cannot silently bill $0 for a typo.
func (s *Service) GetPlanPricing(plan string) (int, error) {
	switch plan {
	case PlanCore:
		return s.cfg.CoreSeatCents, nil
	case PlanPro:
		return s.cfg.ProSeatCents, nil
	case PlanPrivacy:
		return s.cfg.PrivacySeatCents, nil
	default:
		return 0, fmt.Errorf("%w: unknown plan %q", ErrInvalidInput, plan)
	}
}

// PerSeatStorageBytes returns the default storage allocation a seat
// on the given plan is entitled to. Surfaced by the tenant
// provisioner so newly-created tenants land in `quotas` with a
// sensible `storage_limit_bytes` rather than 0 (which would reject
// every subsequent mail delivery).
func (s *Service) PerSeatStorageBytes(plan string) (int64, error) {
	switch plan {
	case PlanCore:
		return s.cfg.CorePerSeatBytes, nil
	case PlanPro:
		return s.cfg.ProPerSeatBytes, nil
	case PlanPrivacy:
		return s.cfg.PrivacyPerSeatBytes, nil
	default:
		return 0, fmt.Errorf("%w: unknown plan %q", ErrInvalidInput, plan)
	}
}

// GetQuota reads the tenant's `quotas` row. Returns ErrNotFound
// when the row does not exist yet.
func (s *Service) GetQuota(ctx context.Context, tenantID string) (*Quota, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil, ErrNotFound
	}
	var q Quota
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT tenant_id::text, storage_used_bytes, storage_limit_bytes,
			       seat_count, seat_limit, created_at, updated_at
			FROM quotas
			WHERE tenant_id = $1::uuid
		`, tenantID).Scan(
			&q.TenantID, &q.StorageUsedBytes, &q.StorageLimitBytes,
			&q.SeatCount, &q.SeatLimit, &q.CreatedAt, &q.UpdatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select quota: %w", err)
	}
	return &q, nil
}

// UpsertQuota creates or updates the tenant's `quotas` row with the
// provided limits. Called by the tenant provisioner on
// `CreateTenant` so every tenant has a row from day one.
func (s *Service) UpsertQuota(ctx context.Context, tenantID string, storageLimit int64, seatLimit int) error {
	if tenantID == "" {
		return fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO quotas (tenant_id, storage_limit_bytes, seat_limit)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (tenant_id) DO UPDATE
			    SET storage_limit_bytes = EXCLUDED.storage_limit_bytes,
			        seat_limit          = EXCLUDED.seat_limit
		`, tenantID, storageLimit, seatLimit)
		return err
	})
}

// UpdateQuotaLimits patches the tenant's quota limits. Nil fields
// are left unchanged.
func (s *Service) UpdateQuotaLimits(ctx context.Context, tenantID string, in UpdateQuotaLimitsInput) (*Quota, error) {
	if in.StorageLimitBytes == nil && in.SeatLimit == nil {
		return nil, fmt.Errorf("%w: nothing to update", ErrInvalidInput)
	}
	if in.StorageLimitBytes != nil && *in.StorageLimitBytes < 0 {
		return nil, fmt.Errorf("%w: storage_limit_bytes must be >= 0", ErrInvalidInput)
	}
	if in.SeatLimit != nil && *in.SeatLimit < 0 {
		return nil, fmt.Errorf("%w: seat_limit must be >= 0", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil, ErrNotFound
	}
	var q Quota
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			UPDATE quotas
			SET storage_limit_bytes = COALESCE($2, storage_limit_bytes),
			    seat_limit          = COALESCE($3, seat_limit)
			WHERE tenant_id = $1::uuid
			RETURNING tenant_id::text, storage_used_bytes, storage_limit_bytes,
			          seat_count, seat_limit, created_at, updated_at
		`, tenantID, in.StorageLimitBytes, in.SeatLimit).Scan(
			&q.TenantID, &q.StorageUsedBytes, &q.StorageLimitBytes,
			&q.SeatCount, &q.SeatLimit, &q.CreatedAt, &q.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO billing_events (tenant_id, event_type, seat_count, metadata)
			VALUES ($1::uuid, 'limit_adjusted', $2, $3::jsonb)
		`, tenantID, q.SeatLimit, `{}`)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &q, nil
}

// UpdateStorageUsage atomically adjusts `quotas.storage_used_bytes`
// by `deltaBytes`. Negative deltas are accepted (e.g. message
// deletions) but the column's CHECK (>=0) constraint prevents the
// counter from going negative.
func (s *Service) UpdateStorageUsage(ctx context.Context, tenantID string, deltaBytes int64) error {
	if tenantID == "" {
		return fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO quotas (tenant_id, storage_used_bytes)
			VALUES ($1::uuid, GREATEST(0, $2))
			ON CONFLICT (tenant_id) DO UPDATE
			    SET storage_used_bytes = GREATEST(0, quotas.storage_used_bytes + $2)
		`, tenantID, deltaBytes)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO billing_events (tenant_id, event_type, storage_delta)
			VALUES ($1::uuid, 'storage_delta', $2)
		`, tenantID, deltaBytes)
		return err
	})
}

// SetStorageUsage replaces `quotas.storage_used_bytes` with the
// absolute `usedBytes` value. Called by the QuotaWorker after it
// re-scans the tenant bucket so snapshot drift is bounded by the
// worker poll interval rather than accumulating in the delta
// counter.
func (s *Service) SetStorageUsage(ctx context.Context, tenantID string, usedBytes int64) error {
	if tenantID == "" {
		return fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if usedBytes < 0 {
		return fmt.Errorf("%w: usedBytes must be >= 0", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO quotas (tenant_id, storage_used_bytes)
			VALUES ($1::uuid, $2)
			ON CONFLICT (tenant_id) DO UPDATE
			    SET storage_used_bytes = EXCLUDED.storage_used_bytes
		`, tenantID, usedBytes)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO billing_events (tenant_id, event_type, storage_delta)
			VALUES ($1::uuid, 'storage_snapshot', $2)
		`, tenantID, usedBytes)
		return err
	})
}

// CountSeats counts active non-shared-inbox users in the tenant.
// Shared inbox and service accounts are excluded so they do not
// consume paid seats (Phase 3 — Shared Inboxes Without Paid Seats).
func (s *Service) CountSeats(ctx context.Context, tenantID string) (int, error) {
	if tenantID == "" {
		return 0, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return 0, nil
	}
	var n int
	err := pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT COUNT(*)::int
			FROM users
			WHERE tenant_id = $1::uuid
			  AND status = 'active'
			  AND account_type = 'user'
		`, tenantID).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("count seats: %w", err)
	}
	return n, nil
}

// SyncSeatCount recomputes `quotas.seat_count` from the `users`
// table so the admin console and invoice generator always see a
// consistent number after CreateUser / DeleteUser operations.
func (s *Service) SyncSeatCount(ctx context.Context, tenantID string) (int, error) {
	count, err := s.CountSeats(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	if s.cfg.Pool == nil {
		return count, nil
	}
	err = pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO quotas (tenant_id, seat_count)
			VALUES ($1::uuid, $2)
			ON CONFLICT (tenant_id) DO UPDATE
			    SET seat_count = EXCLUDED.seat_count
		`, tenantID, count)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("sync seat count: %w", err)
	}
	return count, nil
}

// EnforcePlanLimits validates that the tenant's current seat and
// storage usage fit inside the limits stored on the `quotas` row.
// Returns ErrQuotaExceeded when either exceeds its limit (0 limit
// = unlimited).
func (s *Service) EnforcePlanLimits(ctx context.Context, tenantID string) error {
	q, err := s.GetQuota(ctx, tenantID)
	if err != nil {
		return err
	}
	if q.SeatLimit > 0 && q.SeatCount > q.SeatLimit {
		return fmt.Errorf("%w: seats %d > limit %d", ErrQuotaExceeded, q.SeatCount, q.SeatLimit)
	}
	if q.StorageLimitBytes > 0 && q.StorageUsedBytes > q.StorageLimitBytes {
		return fmt.Errorf("%w: storage %d > limit %d", ErrQuotaExceeded, q.StorageUsedBytes, q.StorageLimitBytes)
	}
	return nil
}

// CheckStorageQuota is the pre-flight check the JMAP proxy runs
// before accepting a blob upload. `additionalBytes` is the claimed
// size of the incoming blob; the method returns ErrQuotaExceeded
// when `storage_used_bytes + additionalBytes > storage_limit_bytes`.
func (s *Service) CheckStorageQuota(ctx context.Context, tenantID string, additionalBytes int64) error {
	if additionalBytes < 0 {
		return fmt.Errorf("%w: additionalBytes must be >= 0", ErrInvalidInput)
	}
	q, err := s.GetQuota(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		// No quota row means the tenant has not been provisioned
		// yet — fail closed so uploads cannot bypass the pool.
		return fmt.Errorf("%w: tenant has no quota row", ErrQuotaExceeded)
	}
	if err != nil {
		return err
	}
	if q.StorageLimitBytes == 0 {
		return nil
	}
	if q.StorageUsedBytes+additionalBytes > q.StorageLimitBytes {
		return fmt.Errorf("%w: %d + %d > %d",
			ErrQuotaExceeded, q.StorageUsedBytes, additionalBytes, q.StorageLimitBytes)
	}
	return nil
}

// CheckSeatAvailable returns ErrQuotaExceeded when adding one more
// seat would exceed the tenant's `seat_limit`. Called by
// tenant.Service.CreateUser before the INSERT.
func (s *Service) CheckSeatAvailable(ctx context.Context, tenantID string) error {
	q, err := s.GetQuota(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if q.SeatLimit == 0 {
		return nil
	}
	if q.SeatCount+1 > q.SeatLimit {
		return fmt.Errorf("%w: seat %d would exceed limit %d",
			ErrQuotaExceeded, q.SeatCount+1, q.SeatLimit)
	}
	return nil
}

// IncrementSeatCount atomically bumps `quotas.seat_count` by the
// given delta. Called by the Tenant Service after CreateUser /
// DeleteUser so the quota row tracks the authoritative user-table
// count without needing a second round trip to `CountSeats`.
func (s *Service) IncrementSeatCount(ctx context.Context, tenantID string, delta int) error {
	if tenantID == "" {
		return fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return nil
	}
	event := "seat_added"
	if delta < 0 {
		event = "seat_removed"
	}
	return pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO quotas (tenant_id, seat_count)
			VALUES ($1::uuid, GREATEST(0, $2))
			ON CONFLICT (tenant_id) DO UPDATE
			    SET seat_count = GREATEST(0, quotas.seat_count + $2)
		`, tenantID, delta)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO billing_events (tenant_id, event_type, seat_count)
			VALUES ($1::uuid, $2, $3)
		`, tenantID, event, delta)
		return err
	})
}

// CalculateInvoice computes the monthly invoice total in cents for
// the tenant: `seat_count * per_seat_cents`. The tenant's plan is
// read from the `tenants` table directly (invoice generation is an
// admin operation; no RLS scope needed).
func (s *Service) CalculateInvoice(ctx context.Context, tenantID string) (int64, error) {
	if tenantID == "" {
		return 0, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.cfg.Pool == nil {
		return 0, nil
	}
	var plan string
	err := s.cfg.Pool.QueryRow(ctx, `
		SELECT plan FROM tenants WHERE id = $1::uuid
	`, tenantID).Scan(&plan)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("select tenant plan: %w", err)
	}
	price, err := s.GetPlanPricing(plan)
	if err != nil {
		return 0, err
	}
	seats, err := s.CountSeats(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	total := int64(price) * int64(seats)
	// Best-effort: log the calculation so audits can correlate
	// invoice totals with the seat / price inputs at the time.
	_ = pgx.BeginFunc(ctx, s.cfg.Pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO billing_events (tenant_id, event_type, seat_count, amount_cents)
			VALUES ($1::uuid, 'invoice_generated', $2, $3)
		`, tenantID, seats, total)
		return err
	})
	return total, nil
}

// Summary builds the admin-console billing view.
func (s *Service) Summary(ctx context.Context, tenantID string) (*BillingSummary, error) {
	if s.cfg.Pool == nil {
		return nil, ErrNotFound
	}
	var plan string
	err := s.cfg.Pool.QueryRow(ctx, `
		SELECT plan FROM tenants WHERE id = $1::uuid
	`, tenantID).Scan(&plan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select tenant plan: %w", err)
	}
	price, err := s.GetPlanPricing(plan)
	if err != nil {
		return nil, err
	}
	// Read quota; if missing, synthesise a zero row so the admin UI
	// can still render the per-seat price and plan for a newly-
	// provisioned tenant.
	q, err := s.GetQuota(ctx, tenantID)
	if errors.Is(err, ErrNotFound) {
		q = &Quota{TenantID: tenantID}
	} else if err != nil {
		return nil, err
	}
	pct := 0
	if q.StorageLimitBytes > 0 {
		pct = int((q.StorageUsedBytes * 100) / q.StorageLimitBytes)
	}
	return &BillingSummary{
		TenantID:       tenantID,
		Plan:           plan,
		PerSeatCents:   price,
		SeatCount:      q.SeatCount,
		SeatLimit:      q.SeatLimit,
		MonthlyCents:   int64(price) * int64(q.SeatCount),
		StorageUsed:    q.StorageUsedBytes,
		StorageLimit:   q.StorageLimitBytes,
		StoragePercent: pct,
	}, nil
}

// Pool exposes the underlying pgx pool so sibling packages (the
// quota worker) can share the connection pool without needing a
// second copy of the config surface.
func (s *Service) Pool() *pgxpool.Pool { return s.cfg.Pool }
