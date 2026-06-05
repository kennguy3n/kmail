package tenant

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultProvisionThreshold is the cluster-utilisation fraction at or
// above which AutoProvisionShard provisions a new shard. At 0.8 the
// automation reacts while ~20% headroom remains across active shards,
// which (at the 60s health/provision tick) comfortably covers tenant
// growth between ticks without flapping.
const DefaultProvisionThreshold = 0.80

// provisionLockKey is the Postgres advisory-lock key that serialises
// AutoProvisionShard across worker replicas. Multiple kmail-worker pods
// each run the AutoProvisionWorker on a timer; without coordination two
// pods can independently observe "over threshold", each mint a distinct
// shard name, and provision two shards for one capacity event (the
// idempotent-on-name provisioner contract does NOT help because the
// names differ). A single advisory lock turns the decide-then-provision
// sequence into a critical section: the holder re-reads capacity under
// the lock and provisions at most one shard; contenders skip the tick
// and converge on the next one. The constant is derived from
// "kmail.shard_provision" and must stay stable across releases.
const provisionLockKey int64 = 0x6b6d61696c5f7370 // "kmail_sp"

// ErrNoProvisioner is returned by AutoProvisionShard when capacity is
// exhausted but no ShardProvisioner has been configured — the operator
// must wire one (or provision manually). It is intentionally distinct
// from ErrNoCapacity so the worker can log "configure a provisioner"
// rather than silently swallowing a real capacity event.
var ErrNoProvisioner = errors.New("tenant: capacity threshold reached but no shard provisioner configured")

// ShardProvisioner stands up the infrastructure for a new shard (the
// `deploy/terraform/shard/` module: 3 Stalwart VMs + Postgres +
// Meilisearch + Valkey) and returns the connection details for the
// resulting shard. Implementations are expected to be idempotent on
// `name` so a retried provision after a partial failure converges
// rather than leaking infrastructure.
//
// The interface is deliberately small so it can be backed by a
// Terraform/Pulumi runner in production and by a fake in tests. The
// returned Shard's Name/StalwartURL (and optionally PostgresDSN /
// MaxMailboxes) are persisted verbatim by RegisterShard.
type ShardProvisioner interface {
	Provision(ctx context.Context, name string) (Shard, error)
}

// SetProvisioner installs the provisioner used by AutoProvisionShard.
// Returns the receiver for chaining at construction.
func (s *ShardService) SetProvisioner(p ShardProvisioner) *ShardService {
	s.provisioner = p
	return s
}

// clusterUtilisation reports the global mailbox utilisation across the
// supplied shards as used/capacity, considering only shards that can
// actually accept tenants (active). It returns (0,false) when there is
// no active capacity at all — the caller treats that as "must
// provision" since there is nowhere to place the next tenant.
func clusterUtilisation(shards []Shard) (fraction float64, hasActiveCapacity bool) {
	var used, capacity int
	for _, sh := range shards {
		if sh.Status != ShardStatusActive {
			continue
		}
		if sh.MaxMailboxes <= 0 {
			continue
		}
		used += sh.CurrentMailboxes
		capacity += sh.MaxMailboxes
	}
	if capacity == 0 {
		return 0, false
	}
	return float64(used) / float64(capacity), true
}

// needsProvision decides whether a new shard should be provisioned
// given the active shards and a utilisation threshold. It is a pure
// function so the policy is unit-testable without a database.
//
// A new shard is needed when EITHER:
//   - there is no active shard with a free mailbox slot (hard wall — a
//     new tenant would get ErrNoCapacity right now), OR
//   - the aggregate active-cluster utilisation is at/above threshold
//     (soft wall — react before the hard wall is hit).
func needsProvision(shards []Shard, threshold float64) bool {
	frac, hasActiveCapacity := clusterUtilisation(shards)
	if !hasActiveCapacity {
		return true
	}
	freeSlot := false
	for _, sh := range shards {
		if sh.Status == ShardStatusActive && sh.CurrentMailboxes < sh.MaxMailboxes {
			freeSlot = true
			break
		}
	}
	if !freeSlot {
		return true
	}
	return frac >= threshold
}

// AutoProvisionShard provisions and registers a new shard when active
// capacity is at/above `threshold` (or exhausted). It is a no-op (nil,
// nil) while spare capacity remains, so it is safe to call on a timer.
//
// A non-positive threshold falls back to DefaultProvisionThreshold.
// When provisioning is required but no ShardProvisioner is configured,
// it returns ErrNoProvisioner.
//
// Concurrency: the decide-then-provision body runs under a Postgres
// advisory lock (provisionLockKey) so that when several kmail-worker
// replicas tick at once, exactly one provisions per capacity event.
// The lock is taken non-blocking (pg_try_advisory_lock): a replica that
// loses the race returns (nil, nil) immediately rather than queuing
// behind a multi-minute Terraform run, and re-evaluates on its next
// tick — by which point the winner's new shard is visible and the
// threshold check is satisfied. Capacity is re-read INSIDE the lock so
// the cheap pre-check race (TOCTOU between the lock-free pre-check and
// acquiring the lock) cannot double-provision.
func (s *ShardService) AutoProvisionShard(ctx context.Context, threshold float64) (*Shard, error) {
	if threshold <= 0 {
		threshold = DefaultProvisionThreshold
	}
	// Lock-free pre-check: the common case is "spare capacity remains",
	// so avoid taking a connection + advisory lock on every tick.
	shards, err := s.ListShards(ctx)
	if err != nil {
		return nil, fmt.Errorf("auto-provision: list shards: %w", err)
	}
	if !needsProvision(shards, threshold) {
		return nil, nil
	}
	// provisioner is set once at construction (SetProvisioner) before
	// any worker goroutine starts and is identical across replicas, so
	// it is safe to short-circuit before contending for the lock.
	if s.provisioner == nil {
		return nil, ErrNoProvisioner
	}

	var registered *Shard
	err = s.withProvisionLock(ctx, func(conn *pgxpool.Conn) error {
		// Re-read under the lock: another replica may have provisioned
		// between our pre-check and acquiring the lock, in which case
		// the cluster is no longer over threshold and we must not add a
		// second shard for the same capacity event.
		shards, err := s.ListShards(ctx)
		if err != nil {
			return fmt.Errorf("auto-provision: re-list shards: %w", err)
		}
		if !needsProvision(shards, threshold) {
			return nil
		}
		name := autoShardName()
		provisioned, err := s.provisioner.Provision(ctx, name)
		if err != nil {
			return fmt.Errorf("auto-provision: provision %q: %w", name, err)
		}
		// Default the name from our generated one if the provisioner
		// left it blank, so RegisterShard's required-field check passes.
		if provisioned.Name == "" {
			provisioned.Name = name
		}
		reg, err := s.RegisterShard(ctx, provisioned)
		if err != nil {
			return fmt.Errorf("auto-provision: register %q: %w", provisioned.Name, err)
		}
		s.Logger.Printf("auto-provisioned shard %s (%s) at >=%.0f%% cluster utilisation", reg.ID, reg.Name, threshold*100)
		registered = reg
		return nil
	})
	if err != nil {
		return nil, err
	}
	return registered, nil
}

// withProvisionLock runs fn while holding the provision advisory lock on
// a dedicated connection. It uses the non-blocking pg_try_advisory_lock:
// if another replica already holds the lock (i.e. is mid-provision), fn
// is skipped and (nil) is returned so the caller no-ops this tick. When
// s.Pool is nil (unit tests that drive provisioning directly) fn runs
// without locking so the policy stays testable without a database.
func (s *ShardService) withProvisionLock(ctx context.Context, fn func(conn *pgxpool.Conn) error) error {
	if s.Pool == nil {
		return fn(nil)
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("auto-provision: acquire conn: %w", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", provisionLockKey).Scan(&locked); err != nil {
		return fmt.Errorf("auto-provision: acquire advisory lock: %w", err)
	}
	if !locked {
		// Another replica is provisioning right now; back off and let
		// the next tick re-evaluate once its shard is committed.
		return nil
	}
	defer func() {
		if _, uerr := conn.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", provisionLockKey); uerr != nil {
			s.Logger.Printf("auto-provision: advisory unlock: %v", uerr)
		}
	}()
	return fn(conn)
}

// autoShardName generates a unique name for an auto-provisioned shard.
// The Unix-second prefix keeps names roughly sortable/readable while a
// short random suffix prevents collisions when two worker pods race to
// provision within the same second (the provisioner contract is
// idempotent on name, so two identical names would otherwise converge
// onto one shard or trip the unique constraint).
func autoShardName() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively impossible; fall back to
		// the nanosecond clock so we still avoid same-second clashes.
		return fmt.Sprintf("shard-auto-%d", time.Now().UTC().UnixNano())
	}
	return fmt.Sprintf("shard-auto-%d-%s", time.Now().UTC().Unix(), hex.EncodeToString(b[:]))
}

// CapacityReport summarises cluster capacity for the shard-health
// dashboard (consumed by the admin SloAdmin page). It is the read-only
// companion to AutoProvisionShard: it surfaces the same utilisation
// signal the automation acts on so an operator can see why (or whether)
// a provision will fire.
type CapacityReport struct {
	Shards            []ShardCapacity `json:"shards"`
	ActiveShards      int             `json:"active_shards"`
	TotalMailboxes    int             `json:"total_mailboxes"`
	TotalCapacity     int             `json:"total_capacity"`
	ClusterUtilised   float64         `json:"cluster_utilised"`
	Threshold         float64         `json:"threshold"`
	NeedsProvisioning bool            `json:"needs_provisioning"`
}

// ShardCapacity is one shard's slice of a CapacityReport.
type ShardCapacity struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	Mailboxes    int     `json:"mailboxes"`
	MaxMailboxes int     `json:"max_mailboxes"`
	Utilised     float64 `json:"utilised"`
}

// CapacityReportWithThreshold builds the capacity report against the
// supplied threshold (defaulting to DefaultProvisionThreshold).
func (s *ShardService) CapacityReportWithThreshold(ctx context.Context, threshold float64) (*CapacityReport, error) {
	if threshold <= 0 {
		threshold = DefaultProvisionThreshold
	}
	shards, err := s.ListShards(ctx)
	if err != nil {
		return nil, fmt.Errorf("capacity report: %w", err)
	}
	rep := &CapacityReport{Threshold: threshold, Shards: []ShardCapacity{}}
	for _, sh := range shards {
		var util float64
		if sh.MaxMailboxes > 0 {
			util = float64(sh.CurrentMailboxes) / float64(sh.MaxMailboxes)
		}
		rep.Shards = append(rep.Shards, ShardCapacity{
			ID:           sh.ID,
			Name:         sh.Name,
			Status:       sh.Status,
			Mailboxes:    sh.CurrentMailboxes,
			MaxMailboxes: sh.MaxMailboxes,
			Utilised:     util,
		})
		if sh.Status == ShardStatusActive && sh.MaxMailboxes > 0 {
			rep.ActiveShards++
			rep.TotalMailboxes += sh.CurrentMailboxes
			rep.TotalCapacity += sh.MaxMailboxes
		}
	}
	if rep.TotalCapacity > 0 {
		rep.ClusterUtilised = float64(rep.TotalMailboxes) / float64(rep.TotalCapacity)
	}
	rep.NeedsProvisioning = needsProvision(shards, threshold)
	return rep, nil
}

// AutoProvisionWorker periodically invokes AutoProvisionShard so the
// cluster grows without operator intervention. It mirrors HealthWorker
// in shape (Service + Interval + Logger) so the worker registry wires
// it the same way.
type AutoProvisionWorker struct {
	Service   *ShardService
	Interval  time.Duration
	Threshold float64
	Logger    *log.Logger
}

// Run loops until ctx is cancelled, provisioning on each tick when
// capacity demands it. A missing provisioner is logged once per tick
// at most and does not stop the loop — an operator may wire one (or
// add a shard manually) at any time.
func (w *AutoProvisionWorker) Run(ctx context.Context) {
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
	tick := func() {
		shard, err := w.Service.AutoProvisionShard(ctx, w.Threshold)
		switch {
		case errors.Is(err, ErrNoProvisioner):
			logger.Printf("auto-provision: capacity threshold reached but no provisioner configured")
		case err != nil:
			logger.Printf("auto-provision: %v", err)
		case shard != nil:
			logger.Printf("auto-provision: added shard %s (%s)", shard.ID, shard.Name)
		}
	}
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	tick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick()
		}
	}
}
