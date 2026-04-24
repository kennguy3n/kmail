package deliverability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Pool types — kept in sync with the CHECK constraint on
// `ip_pools.pool_type`.
const (
	PoolSystemTransactional = "system_transactional"
	PoolMatureTrusted       = "mature_trusted"
	PoolNewWarming          = "new_warming"
	PoolRestricted          = "restricted"
	PoolDedicatedEnterprise = "dedicated_enterprise"
)

// IPPool is the API representation of a row in `ip_pools`.
type IPPool struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	PoolType    string    `json:"pool_type"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// IPAddress is the API representation of a row in `ip_addresses`.
type IPAddress struct {
	ID              string    `json:"id"`
	PoolID          string    `json:"pool_id"`
	Address         string    `json:"address"`
	ReverseDNS      string    `json:"reverse_dns"`
	ReputationScore int       `json:"reputation_score"`
	DailyVolume     int64     `json:"daily_volume"`
	WarmupDay       int       `json:"warmup_day"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// TenantPoolAssignment captures (tenant, pool, priority).
type TenantPoolAssignment struct {
	TenantID  string    `json:"tenant_id"`
	PoolID    string    `json:"pool_id"`
	PoolName  string    `json:"pool_name"`
	PoolType  string    `json:"pool_type"`
	Priority  int       `json:"priority"`
	CreatedAt time.Time `json:"created_at"`
}

// CreatePoolInput carries the fields accepted by
// POST /api/v1/admin/ip-pools.
type CreatePoolInput struct {
	Name        string `json:"name"`
	PoolType    string `json:"pool_type"`
	Description string `json:"description"`
}

// AddIPInput carries the fields accepted by POST
// /api/v1/admin/ip-pools/{id}/ips.
type AddIPInput struct {
	Address    string `json:"address"`
	ReverseDNS string `json:"reverse_dns"`
}

// IPPoolService owns the IP pool registry.
type IPPoolService struct {
	pool *pgxpool.Pool
}

// ListPools returns every pool in the registry.
func (s *IPPoolService) ListPools(ctx context.Context) ([]IPPool, error) {
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, pool_type, description, created_at, updated_at
		FROM ip_pools
		ORDER BY pool_type, name
	`)
	if err != nil {
		return nil, fmt.Errorf("list ip_pools: %w", err)
	}
	defer rows.Close()
	var out []IPPool
	for rows.Next() {
		var p IPPool
		if err := rows.Scan(
			&p.ID, &p.Name, &p.PoolType, &p.Description, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreatePool inserts a new pool.
func (s *IPPoolService) CreatePool(ctx context.Context, in CreatePoolInput) (*IPPool, error) {
	if in.Name == "" || in.PoolType == "" {
		return nil, fmt.Errorf("%w: name and pool_type required", ErrInvalidInput)
	}
	if !isValidPoolType(in.PoolType) {
		return nil, fmt.Errorf("%w: invalid pool_type %q", ErrInvalidInput, in.PoolType)
	}
	if s.pool == nil {
		return nil, fmt.Errorf("no pool configured")
	}
	var p IPPool
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ip_pools (name, pool_type, description)
		VALUES ($1, $2, $3)
		RETURNING id::text, name, pool_type, description, created_at, updated_at
	`, in.Name, in.PoolType, in.Description).Scan(
		&p.ID, &p.Name, &p.PoolType, &p.Description, &p.CreatedAt, &p.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert ip_pool: %w", err)
	}
	return &p, nil
}

// AddIP inserts an IP address into a pool.
func (s *IPPoolService) AddIP(ctx context.Context, poolID string, in AddIPInput) (*IPAddress, error) {
	if poolID == "" || in.Address == "" {
		return nil, fmt.Errorf("%w: poolID and address required", ErrInvalidInput)
	}
	if s.pool == nil {
		return nil, fmt.Errorf("no pool configured")
	}
	var ip IPAddress
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ip_addresses (pool_id, address, reverse_dns, status)
		VALUES ($1::uuid, $2::inet, $3, 'warming')
		RETURNING id::text, pool_id::text, address::text, reverse_dns,
		          reputation_score, daily_volume, warmup_day, status,
		          created_at, updated_at
	`, poolID, in.Address, in.ReverseDNS).Scan(
		&ip.ID, &ip.PoolID, &ip.Address, &ip.ReverseDNS,
		&ip.ReputationScore, &ip.DailyVolume, &ip.WarmupDay, &ip.Status,
		&ip.CreatedAt, &ip.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert ip_address: %w", err)
	}
	return &ip, nil
}

// ListIPs returns the IPs in the given pool.
func (s *IPPoolService) ListIPs(ctx context.Context, poolID string) ([]IPAddress, error) {
	if poolID == "" {
		return nil, fmt.Errorf("%w: poolID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, pool_id::text, address::text, reverse_dns,
		       reputation_score, daily_volume, warmup_day, status,
		       created_at, updated_at
		FROM ip_addresses
		WHERE pool_id = $1::uuid
		ORDER BY reputation_score DESC, address
	`, poolID)
	if err != nil {
		return nil, fmt.Errorf("list ip_addresses: %w", err)
	}
	defer rows.Close()
	var out []IPAddress
	for rows.Next() {
		var ip IPAddress
		if err := rows.Scan(
			&ip.ID, &ip.PoolID, &ip.Address, &ip.ReverseDNS,
			&ip.ReputationScore, &ip.DailyVolume, &ip.WarmupDay, &ip.Status,
			&ip.CreatedAt, &ip.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, ip)
	}
	return out, rows.Err()
}

// AssignTenantPool assigns a tenant to a pool by pool type. If the
// tenant is already in a pool of the same type the row is left as-is.
// Lower `priority` (smaller int) wins when multiple pools are
// assigned.
func (s *IPPoolService) AssignTenantPool(ctx context.Context, tenantID, poolType string, priority int) error {
	if tenantID == "" || poolType == "" {
		return fmt.Errorf("%w: tenantID and poolType required", ErrInvalidInput)
	}
	if !isValidPoolType(poolType) {
		return fmt.Errorf("%w: invalid poolType %q", ErrInvalidInput, poolType)
	}
	if priority <= 0 {
		priority = 100
	}
	if s.pool == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		var poolID string
		err := tx.QueryRow(ctx, `
			SELECT id::text FROM ip_pools
			WHERE pool_type = $1
			ORDER BY created_at ASC
			LIMIT 1
		`, poolType).Scan(&poolID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: pool for type %q not provisioned", ErrNotFound, poolType)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO tenant_pool_assignments (tenant_id, pool_id, priority)
			VALUES ($1::uuid, $2::uuid, $3)
			ON CONFLICT (tenant_id, pool_id) DO UPDATE
			    SET priority = EXCLUDED.priority
		`, tenantID, poolID, priority)
		return err
	})
}

// GetTenantPool returns the tenant's primary pool assignment (the
// row with the lowest priority).
func (s *IPPoolService) GetTenantPool(ctx context.Context, tenantID string) (*TenantPoolAssignment, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	if s.pool == nil {
		return nil, ErrNotFound
	}
	var a TenantPoolAssignment
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT tpa.tenant_id::text, tpa.pool_id::text, p.name, p.pool_type,
			       tpa.priority, tpa.created_at
			FROM tenant_pool_assignments tpa
			JOIN ip_pools p ON p.id = tpa.pool_id
			WHERE tpa.tenant_id = $1::uuid
			ORDER BY tpa.priority ASC
			LIMIT 1
		`, tenantID).Scan(
			&a.TenantID, &a.PoolID, &a.PoolName, &a.PoolType, &a.Priority, &a.CreatedAt,
		)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get tenant pool: %w", err)
	}
	return &a, nil
}

// SelectSendingIP picks the best active IP from the tenant's
// primary pool, ranked by reputation_score DESC then daily_volume
// ASC (prefer headroom). Returns ErrNotFound when the pool has no
// active IPs.
func (s *IPPoolService) SelectSendingIP(ctx context.Context, tenantID string) (*IPAddress, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("%w: tenantID required", ErrInvalidInput)
	}
	assignment, err := s.GetTenantPool(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	ips, err := s.ListIPs(ctx, assignment.PoolID)
	if err != nil {
		return nil, err
	}
	best := selectBestIP(ips)
	if best == nil {
		return nil, fmt.Errorf("%w: no active IPs in pool %s", ErrNotFound, assignment.PoolName)
	}
	return best, nil
}

// selectBestIP is the pure-logic ranker so unit tests can exercise
// the selection rule without a database.
func selectBestIP(ips []IPAddress) *IPAddress {
	var best *IPAddress
	for i := range ips {
		ip := &ips[i]
		if ip.Status != "active" {
			continue
		}
		if best == nil {
			best = ip
			continue
		}
		if ip.ReputationScore > best.ReputationScore {
			best = ip
			continue
		}
		if ip.ReputationScore == best.ReputationScore && ip.DailyVolume < best.DailyVolume {
			best = ip
		}
	}
	return best
}

func isValidPoolType(pt string) bool {
	switch pt {
	case PoolSystemTransactional, PoolMatureTrusted, PoolNewWarming,
		PoolRestricted, PoolDedicatedEnterprise:
		return true
	default:
		return false
	}
}
