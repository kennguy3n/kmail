package tenant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

// DefaultProvisionThreshold is the cluster-utilisation fraction at or
// above which AutoProvisionShard provisions a new shard. At 0.8 the
// automation reacts while ~20% headroom remains across active shards,
// which (at the 60s health/provision tick) comfortably covers tenant
// growth between ticks without flapping.
const DefaultProvisionThreshold = 0.80

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
func (s *ShardService) AutoProvisionShard(ctx context.Context, threshold float64) (*Shard, error) {
	if threshold <= 0 {
		threshold = DefaultProvisionThreshold
	}
	shards, err := s.ListShards(ctx)
	if err != nil {
		return nil, fmt.Errorf("auto-provision: list shards: %w", err)
	}
	if !needsProvision(shards, threshold) {
		return nil, nil
	}
	if s.provisioner == nil {
		return nil, ErrNoProvisioner
	}
	name := fmt.Sprintf("shard-auto-%d", time.Now().UTC().Unix())
	provisioned, err := s.provisioner.Provision(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("auto-provision: provision %q: %w", name, err)
	}
	// Default the name from our generated one if the provisioner left
	// it blank, so RegisterShard's required-field check passes.
	if provisioned.Name == "" {
		provisioned.Name = name
	}
	registered, err := s.RegisterShard(ctx, provisioned)
	if err != nil {
		return nil, fmt.Errorf("auto-provision: register %q: %w", provisioned.Name, err)
	}
	s.Logger.Printf("auto-provisioned shard %s (%s) at >=%.0f%% cluster utilisation", registered.ID, registered.Name, threshold*100)
	return registered, nil
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
