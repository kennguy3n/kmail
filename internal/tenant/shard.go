package tenant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Shard statuses.
const (
	ShardStatusActive   = "active"
	ShardStatusDraining = "draining"
	ShardStatusOffline  = "offline"
)

// Shard is the API + persisted shape of one Stalwart shard.
type Shard struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	StalwartURL      string    `json:"stalwart_url"`
	PostgresDSN      string    `json:"postgres_dsn,omitempty"`
	MaxMailboxes     int       `json:"max_mailboxes"`
	CurrentMailboxes int       `json:"current_mailboxes"`
	Status           string    `json:"status"`
	HealthCheckedAt  time.Time `json:"health_checked_at"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// TenantShardAssignment is the per-tenant pointer to a shard.
type TenantShardAssignment struct {
	TenantID   string    `json:"tenant_id"`
	ShardID    string    `json:"shard_id"`
	AssignedAt time.Time `json:"assigned_at"`
}

// ErrNoCapacity is returned by AssignTenantToShard when no active
// shard has room.
var ErrNoCapacity = errors.New("no shard with free capacity")

// ShardService owns the `stalwart_shards` and
// `tenant_shard_assignments` tables. Shard rows are global; the
// pool does not set the tenant GUC for these queries.
type ShardService struct {
	Pool       *pgxpool.Pool
	HTTPClient *http.Client
	Logger     *log.Logger

	mu    sync.RWMutex
	cache map[string]string // tenantID -> StalwartURL
}

// NewShardService builds a ShardService with sensible defaults.
func NewShardService(pool *pgxpool.Pool, logger *log.Logger) *ShardService {
	if logger == nil {
		logger = log.Default()
	}
	return &ShardService{
		Pool:       pool,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
		Logger:     logger,
		cache:      make(map[string]string),
	}
}

// RegisterShard inserts a new shard row.
func (s *ShardService) RegisterShard(ctx context.Context, shard Shard) (*Shard, error) {
	if shard.Name == "" || shard.StalwartURL == "" {
		return nil, fmt.Errorf("name and stalwart_url required")
	}
	if shard.MaxMailboxes <= 0 {
		shard.MaxMailboxes = 10000
	}
	if shard.Status == "" {
		shard.Status = ShardStatusActive
	}
	out := shard
	if s.Pool == nil {
		out.ID = "stub"
		out.CreatedAt = time.Now()
		out.UpdatedAt = out.CreatedAt
		return &out, nil
	}
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO stalwart_shards (name, stalwart_url, postgres_dsn, max_mailboxes, status)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, created_at, updated_at
	`, shard.Name, shard.StalwartURL, shard.PostgresDSN, shard.MaxMailboxes, shard.Status).Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("register shard: %w", err)
	}
	return &out, nil
}

// ListShards returns every shard row.
func (s *ShardService) ListShards(ctx context.Context) ([]Shard, error) {
	if s.Pool == nil {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT id::text, name, stalwart_url, COALESCE(postgres_dsn, ''),
		       max_mailboxes, current_mailboxes, status,
		       COALESCE(health_checked_at, '1970-01-01'::timestamptz),
		       created_at, updated_at
		FROM stalwart_shards
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("list shards: %w", err)
	}
	defer rows.Close()
	var out []Shard
	for rows.Next() {
		var sh Shard
		if err := rows.Scan(&sh.ID, &sh.Name, &sh.StalwartURL, &sh.PostgresDSN, &sh.MaxMailboxes, &sh.CurrentMailboxes, &sh.Status, &sh.HealthCheckedAt, &sh.CreatedAt, &sh.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// GetShard returns a single shard by ID.
func (s *ShardService) GetShard(ctx context.Context, shardID string) (*Shard, error) {
	if s.Pool == nil {
		return nil, ErrNoCapacity
	}
	var sh Shard
	err := s.Pool.QueryRow(ctx, `
		SELECT id::text, name, stalwart_url, COALESCE(postgres_dsn, ''),
		       max_mailboxes, current_mailboxes, status,
		       COALESCE(health_checked_at, '1970-01-01'::timestamptz),
		       created_at, updated_at
		FROM stalwart_shards WHERE id = $1::uuid
	`, shardID).Scan(&sh.ID, &sh.Name, &sh.StalwartURL, &sh.PostgresDSN, &sh.MaxMailboxes, &sh.CurrentMailboxes, &sh.Status, &sh.HealthCheckedAt, &sh.CreatedAt, &sh.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get shard: %w", err)
	}
	return &sh, nil
}

// UpdateShard patches the updatable fields on a shard.
func (s *ShardService) UpdateShard(ctx context.Context, shardID string, in Shard) (*Shard, error) {
	if s.Pool == nil {
		return &in, nil
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE stalwart_shards SET
			name = COALESCE(NULLIF($2, ''), name),
			stalwart_url = COALESCE(NULLIF($3, ''), stalwart_url),
			max_mailboxes = CASE WHEN $4 > 0 THEN $4 ELSE max_mailboxes END,
			status = COALESCE(NULLIF($5, ''), status),
			updated_at = now()
		WHERE id = $1::uuid
	`, shardID, in.Name, in.StalwartURL, in.MaxMailboxes, in.Status)
	if err != nil {
		return nil, fmt.Errorf("update shard: %w", err)
	}
	// stalwart_url may have just changed; the per-tenant URL cache
	// in GetTenantShard has no TTL, so flush every tenant on this
	// shard. Worst case: each tenant pays one extra DB lookup on
	// the next JMAP request.
	tenants, terr := s.ListTenantsOnShard(ctx, shardID)
	if terr != nil {
		return nil, fmt.Errorf("update shard: refresh cache: %w", terr)
	}
	for _, tid := range tenants {
		s.invalidate(tid)
	}
	return s.GetShard(ctx, shardID)
}

// UpdateShardHealth records the result of a health probe.
func (s *ShardService) UpdateShardHealth(ctx context.Context, shardID string, healthy bool) error {
	if s.Pool == nil {
		return nil
	}
	status := ShardStatusActive
	if !healthy {
		status = ShardStatusOffline
	}
	_, err := s.Pool.Exec(ctx, `
		UPDATE stalwart_shards
		SET health_checked_at = now(),
		    status = CASE WHEN status = 'draining' THEN 'draining' ELSE $2 END,
		    updated_at = now()
		WHERE id = $1::uuid
	`, shardID, status)
	if err != nil {
		return fmt.Errorf("update shard health: %w", err)
	}
	return nil
}

// AssignTenantToShard chooses the least-loaded active shard with
// free capacity and records a tenant_shard_assignments row.
func (s *ShardService) AssignTenantToShard(ctx context.Context, tenantID string) (*TenantShardAssignment, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenantID required")
	}
	if s.Pool == nil {
		return &TenantShardAssignment{TenantID: tenantID, ShardID: "stub"}, nil
	}
	var assignment TenantShardAssignment
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Look up any existing assignment so we can decrement the
		// old shard's counter if the tenant is being reassigned.
		var oldShardID string
		err := tx.QueryRow(ctx, `
			SELECT shard_id::text FROM tenant_shard_assignments
			WHERE tenant_id = $1::uuid
		`, tenantID).Scan(&oldShardID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Pick the shard with the fewest current_mailboxes that
		// still has capacity and is active.
		var shardID string
		err = tx.QueryRow(ctx, `
			SELECT id::text FROM stalwart_shards
			WHERE status = 'active' AND current_mailboxes < max_mailboxes
			ORDER BY current_mailboxes::float / NULLIF(max_mailboxes, 0) ASC, id
			LIMIT 1
		`).Scan(&shardID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoCapacity
		}
		if err != nil {
			return err
		}
		// Upsert the assignment.
		if err := tx.QueryRow(ctx, `
			INSERT INTO tenant_shard_assignments (tenant_id, shard_id)
			VALUES ($1::uuid, $2::uuid)
			ON CONFLICT (tenant_id) DO UPDATE SET shard_id = EXCLUDED.shard_id, assigned_at = now()
			RETURNING tenant_id::text, shard_id::text, assigned_at
		`, tenantID, shardID).Scan(&assignment.TenantID, &assignment.ShardID, &assignment.AssignedAt); err != nil {
			return err
		}
		// Decrement the old shard's counter if this is a reassignment
		// to a different shard. Mirrors the pattern in RebalanceShard.
		if oldShardID != "" && oldShardID != shardID {
			if _, err := tx.Exec(ctx, `
				UPDATE stalwart_shards
				SET current_mailboxes = GREATEST(current_mailboxes - 1, 0), updated_at = now()
				WHERE id = $1::uuid
			`, oldShardID); err != nil {
				return err
			}
		}
		// Bump the mailbox counter on the winning shard only when
		// we actually moved; a no-op reassignment to the same shard
		// should leave the counter alone.
		if oldShardID == shardID {
			return nil
		}
		_, err = tx.Exec(ctx, `
			UPDATE stalwart_shards SET current_mailboxes = current_mailboxes + 1, updated_at = now()
			WHERE id = $1::uuid
		`, shardID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("assign tenant shard: %w", err)
	}
	s.invalidate(tenantID)
	return &assignment, nil
}

// GetTenantShard returns the Stalwart URL for the tenant, using
// an in-process cache.
func (s *ShardService) GetTenantShard(ctx context.Context, tenantID string) (string, error) {
	if tenantID == "" {
		return "", fmt.Errorf("tenantID required")
	}
	s.mu.RLock()
	url, ok := s.cache[tenantID]
	s.mu.RUnlock()
	if ok {
		return url, nil
	}
	if s.Pool == nil {
		return "", ErrNoCapacity
	}
	err := s.Pool.QueryRow(ctx, `
		SELECT sh.stalwart_url
		FROM tenant_shard_assignments tsa
		JOIN stalwart_shards sh ON sh.id = tsa.shard_id
		WHERE tsa.tenant_id = $1::uuid
	`, tenantID).Scan(&url)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoCapacity
	}
	if err != nil {
		return "", fmt.Errorf("get tenant shard: %w", err)
	}
	s.mu.Lock()
	s.cache[tenantID] = url
	s.mu.Unlock()
	return url, nil
}

// RebalanceShard moves a tenant from one shard to another.
func (s *ShardService) RebalanceShard(ctx context.Context, fromShardID, toShardID, tenantID string) (*TenantShardAssignment, error) {
	if tenantID == "" || toShardID == "" {
		return nil, fmt.Errorf("tenantID and toShardID required")
	}
	if s.Pool == nil {
		return &TenantShardAssignment{TenantID: tenantID, ShardID: toShardID}, nil
	}
	var out TenantShardAssignment
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO tenant_shard_assignments (tenant_id, shard_id)
			VALUES ($1::uuid, $2::uuid)
			ON CONFLICT (tenant_id) DO UPDATE SET shard_id = EXCLUDED.shard_id, assigned_at = now()
			RETURNING tenant_id::text, shard_id::text, assigned_at
		`, tenantID, toShardID).Scan(&out.TenantID, &out.ShardID, &out.AssignedAt); err != nil {
			return err
		}
		if fromShardID != "" {
			if _, err := tx.Exec(ctx, `
				UPDATE stalwart_shards SET current_mailboxes = GREATEST(current_mailboxes - 1, 0), updated_at = now()
				WHERE id = $1::uuid
			`, fromShardID); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE stalwart_shards SET current_mailboxes = current_mailboxes + 1, updated_at = now()
			WHERE id = $1::uuid
		`, toShardID)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("rebalance shard: %w", err)
	}
	s.invalidate(tenantID)
	return &out, nil
}

// ListTenantsOnShard returns every tenant currently assigned to
// the shard.
func (s *ShardService) ListTenantsOnShard(ctx context.Context, shardID string) ([]string, error) {
	if s.Pool == nil {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT tenant_id::text FROM tenant_shard_assignments WHERE shard_id = $1::uuid
	`, shardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *ShardService) invalidate(tenantID string) {
	s.mu.Lock()
	delete(s.cache, tenantID)
	s.mu.Unlock()
}

// HealthCheck runs one HTTP probe per shard and calls
// UpdateShardHealth with the result.
func (s *ShardService) HealthCheck(ctx context.Context) error {
	shards, err := s.ListShards(ctx)
	if err != nil {
		return err
	}
	for _, sh := range shards {
		if sh.Status == ShardStatusDraining {
			continue
		}
		ok := s.probe(ctx, sh.StalwartURL)
		if err := s.UpdateShardHealth(ctx, sh.ID, ok); err != nil {
			s.Logger.Printf("shard %s: health update failed: %v", sh.ID, err)
		}
	}
	return nil
}

func (s *ShardService) probe(ctx context.Context, stalwartURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stalwartURL+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Only treat 2xx as healthy. 4xx (auth/route errors) and 5xx
	// should drain the shard rather than keep it in rotation.
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// GetSecondaryShards returns the failover Stalwart URLs for the
// tenant's primary shard, ordered by `shard_failover_config.priority`.
// The list is intentionally empty when no backups are configured —
// the caller (JMAP proxy) treats that as "no failover available"
// and falls through to the global default.
func (s *ShardService) GetSecondaryShards(ctx context.Context, tenantID string) ([]string, error) {
	if tenantID == "" {
		return nil, fmt.Errorf("tenantID required")
	}
	if s.Pool == nil {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT bsh.stalwart_url
		FROM tenant_shard_assignments tsa
		JOIN shard_failover_config fc ON fc.shard_id = tsa.shard_id
		JOIN stalwart_shards bsh       ON bsh.id     = fc.backup_shard_id
		WHERE tsa.tenant_id = $1::uuid AND bsh.healthy = true
		ORDER BY fc.priority ASC, bsh.created_at ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("get secondary shards: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// HealthWorker is the background probe loop.
type HealthWorker struct {
	Service  *ShardService
	Interval time.Duration
	Logger   *log.Logger
}

// Run loops until ctx is cancelled.
func (w *HealthWorker) Run(ctx context.Context) {
	if w.Service == nil {
		return
	}
	if w.Interval <= 0 {
		w.Interval = 60 * time.Second
	}
	logger := w.Logger
	if logger == nil {
		logger = log.Default()
	}
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	if err := w.Service.HealthCheck(ctx); err != nil {
		logger.Printf("shard health first tick: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.Service.HealthCheck(ctx); err != nil {
				logger.Printf("shard health tick: %v", err)
			}
		}
	}
}
