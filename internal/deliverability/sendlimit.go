package deliverability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// SendLimitService enforces per-tenant daily / hourly send caps.
type SendLimitService struct {
	pool         *pgxpool.Pool
	valkey       *redis.Client
	coreDaily    int
	proDaily     int
	privacyDaily int
}

// SendLimit describes a tenant's effective send limits.
type SendLimit struct {
	TenantID    string `json:"tenant_id"`
	Plan        string `json:"plan"`
	DailyLimit  int    `json:"daily_limit"`
	HourlyLimit int    `json:"hourly_limit"`
}

// PlanDailyLimit returns the plan's default daily cap.
func (s *SendLimitService) PlanDailyLimit(plan string) (int, error) {
	switch plan {
	case "core":
		return s.coreDaily, nil
	case "pro":
		return s.proDaily, nil
	case "privacy":
		return s.privacyDaily, nil
	default:
		return 0, fmt.Errorf("%w: unknown plan %q", ErrInvalidInput, plan)
	}
}

// HourlyFromDaily returns the default hourly cap (daily / 10).
func HourlyFromDaily(daily int) int {
	if daily <= 0 {
		return 0
	}
	h := daily / 10
	if h <= 0 {
		h = 1
	}
	return h
}

// GetLimit resolves the tenant's effective daily / hourly caps. The
// per-tenant override row in `tenant_send_limits` wins; otherwise
// the plan default is used.
func (s *SendLimitService) GetLimit(ctx context.Context, tenantID string) (*SendLimit, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return &SendLimit{TenantID: tenantID, DailyLimit: s.proDaily, HourlyLimit: HourlyFromDaily(s.proDaily)}, nil
	}
	var plan string
	var daily, hourly int
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var planRow string
		if err := tx.QueryRow(ctx, `SELECT plan FROM tenants WHERE id = $1::uuid`, tenantID).Scan(&planRow); err != nil {
			return err
		}
		plan = planRow
		var d, h int
		err := tx.QueryRow(ctx, `
			SELECT daily_limit, hourly_limit FROM tenant_send_limits
			WHERE tenant_id = $1::uuid
		`, tenantID).Scan(&d, &h)
		if errors.Is(err, pgx.ErrNoRows) {
			d, _ = s.PlanDailyLimit(planRow)
			h = HourlyFromDaily(d)
			return nil
		}
		if err != nil {
			return err
		}
		daily, hourly = d, h
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("get send limit: %w", err)
	}
	if daily == 0 {
		daily, _ = s.PlanDailyLimit(plan)
	}
	if hourly == 0 {
		hourly = HourlyFromDaily(daily)
	}
	return &SendLimit{TenantID: tenantID, Plan: plan, DailyLimit: daily, HourlyLimit: hourly}, nil
}

// SetLimit overrides the tenant's send cap.
func (s *SendLimitService) SetLimit(ctx context.Context, tenantID string, daily, hourly int) error {
	if tenantID == "" {
		return fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if daily < 0 || hourly < 0 {
		return fmt.Errorf("%w: limits must be >= 0", ErrInvalidInput)
	}
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO tenant_send_limits (tenant_id, daily_limit, hourly_limit)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (tenant_id) DO UPDATE
			    SET daily_limit = EXCLUDED.daily_limit,
			        hourly_limit = EXCLUDED.hourly_limit
		`, tenantID, daily, hourly)
		return err
	})
}

// dailyKey returns the Valkey key used for the per-tenant per-day
// send counter.
func dailyKey(tenantID string, day time.Time) string {
	return fmt.Sprintf("kmail:sends:daily:%s:%s", tenantID, day.UTC().Format("20060102"))
}

// GetDailyVolume returns the persisted send count for the given UTC
// day. Returns 0 when Valkey is unconfigured or the key has
// expired.
func (s *SendLimitService) GetDailyVolume(ctx context.Context, tenantID string, day time.Time) (int64, error) {
	if s == nil || s.valkey == nil || tenantID == "" {
		return 0, nil
	}
	v, err := s.valkey.Get(ctx, dailyKey(tenantID, day)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get daily volume: %w", err)
	}
	return v, nil
}

// GetVolumeHistory returns up to `days` daily send counts in
// reverse-chronological order (index 0 = today, index 1 = yesterday,
// …). Missing keys count as 0.
func (s *SendLimitService) GetVolumeHistory(ctx context.Context, tenantID string, days int) ([]int64, error) {
	if days <= 0 {
		return nil, nil
	}
	if s == nil || s.valkey == nil || tenantID == "" {
		return make([]int64, days), nil
	}
	now := time.Now().UTC()
	keys := make([]string, days)
	for i := 0; i < days; i++ {
		keys[i] = dailyKey(tenantID, now.AddDate(0, 0, -i))
	}
	raw, err := s.valkey.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("get volume history: %w", err)
	}
	out := make([]int64, days)
	for i, v := range raw {
		switch tv := v.(type) {
		case nil:
			out[i] = 0
		case string:
			n, perr := parseInt64(tv)
			if perr != nil {
				return nil, fmt.Errorf("get volume history: %w", perr)
			}
			out[i] = n
		default:
			return nil, fmt.Errorf("get volume history: unexpected type %T", v)
		}
	}
	return out, nil
}

// AverageDailyVolume returns the mean of the last `days` daily send
// counts (entry 0 = today is excluded so that an in-progress day
// does not skew the baseline). Returns 0 when no history is
// available.
func (s *SendLimitService) AverageDailyVolume(ctx context.Context, tenantID string, days int) (float64, error) {
	hist, err := s.GetVolumeHistory(ctx, tenantID, days+1)
	if err != nil {
		return 0, err
	}
	if len(hist) <= 1 {
		return 0, nil
	}
	var sum int64
	for _, v := range hist[1:] {
		sum += v
	}
	return float64(sum) / float64(len(hist)-1), nil
}

func parseInt64(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty string")
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-numeric byte %q in %q", r, s)
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}

// CheckSendLimit increments the Valkey daily + hourly counters for
// the tenant and returns ErrSendLimitExceeded when either is over
// the configured cap. When Valkey is not wired the check is a
// no-op so local dev can compose without a counter backend.
func (s *SendLimitService) CheckSendLimit(ctx context.Context, tenantID string) error {
	if tenantID == "" {
		return fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	limit, err := s.GetLimit(ctx, tenantID)
	if err != nil {
		return err
	}
	if s.valkey == nil {
		return nil
	}
	now := time.Now().UTC()
	dayKey := fmt.Sprintf("kmail:sends:daily:%s:%s", tenantID, now.Format("20060102"))
	hourKey := fmt.Sprintf("kmail:sends:hourly:%s:%s", tenantID, now.Format("2006010215"))
	pipe := s.valkey.TxPipeline()
	dCmd := pipe.Incr(ctx, dayKey)
	// Keep daily counters for 9 days so the deliverability
	// alert + abuse signals can read a 7-day history.
	pipe.ExpireNX(ctx, dayKey, 9*24*time.Hour)
	hCmd := pipe.Incr(ctx, hourKey)
	pipe.ExpireNX(ctx, hourKey, 2*time.Hour)
	if _, err := pipe.Exec(ctx); err != nil {
		// Fail-open: the limiter should not take the BFF offline
		// when Valkey is momentarily unreachable.
		return nil
	}
	if limit.DailyLimit > 0 && dCmd.Val() > int64(limit.DailyLimit) {
		return fmt.Errorf("%w: tenant %s over daily cap %d",
			ErrSendLimitExceeded, tenantID, limit.DailyLimit)
	}
	if limit.HourlyLimit > 0 && hCmd.Val() > int64(limit.HourlyLimit) {
		return fmt.Errorf("%w: tenant %s over hourly cap %d",
			ErrSendLimitExceeded, tenantID, limit.HourlyLimit)
	}
	return nil
}
