package tenant

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNeedsProvision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		shards    []Shard
		threshold float64
		want      bool
	}{
		{
			name:      "spare capacity below threshold",
			shards:    []Shard{{Status: ShardStatusActive, CurrentMailboxes: 10, MaxMailboxes: 100}},
			threshold: 0.8,
			want:      false,
		},
		{
			name:      "utilisation at threshold provisions",
			shards:    []Shard{{Status: ShardStatusActive, CurrentMailboxes: 80, MaxMailboxes: 100}},
			threshold: 0.8,
			want:      true,
		},
		{
			name: "aggregate across shards under threshold",
			shards: []Shard{
				{Status: ShardStatusActive, CurrentMailboxes: 90, MaxMailboxes: 100},
				{Status: ShardStatusActive, CurrentMailboxes: 10, MaxMailboxes: 100},
			},
			threshold: 0.8,
			want:      false, // 100/200 = 50%
		},
		{
			name: "no free slot anywhere provisions even if under fraction",
			shards: []Shard{
				{Status: ShardStatusActive, CurrentMailboxes: 100, MaxMailboxes: 100},
			},
			threshold: 0.99,
			want:      true,
		},
		{
			name:      "no active capacity provisions",
			shards:    []Shard{{Status: ShardStatusDraining, CurrentMailboxes: 0, MaxMailboxes: 100}},
			threshold: 0.8,
			want:      true,
		},
		{
			name:      "empty cluster provisions",
			shards:    nil,
			threshold: 0.8,
			want:      true,
		},
		{
			name: "draining shards excluded from utilisation",
			shards: []Shard{
				{Status: ShardStatusActive, CurrentMailboxes: 10, MaxMailboxes: 100},
				{Status: ShardStatusDraining, CurrentMailboxes: 100, MaxMailboxes: 100},
			},
			threshold: 0.8,
			want:      false, // only the active shard (10%) counts
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := needsProvision(tt.shards, tt.threshold); got != tt.want {
				t.Fatalf("needsProvision = %v, want %v", got, tt.want)
			}
		})
	}
}

type fakeShardProvisioner struct {
	calls int
	shard Shard
	err   error
}

func (f *fakeShardProvisioner) Provision(context.Context, string) (Shard, error) {
	f.calls++
	if f.err != nil {
		return Shard{}, f.err
	}
	return f.shard, nil
}

// TestWithProvisionLockSerialises verifies the distributed-coordination
// contract added for the auto-provision over-provisioning race: while
// one ShardService holds the advisory lock, a second one trying to
// acquire it must skip its critical section (non-blocking
// pg_try_advisory_lock) rather than provisioning a duplicate shard.
//
// It needs a real Postgres (advisory locks are server-side) and skips
// when DATABASE_URL is unset/unreachable so DB-less runs stay green.
func TestWithProvisionLockSerialises(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping advisory-lock integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("connect %s: %v", dsn, err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Skipf("ping: %v", err)
	}

	// Two services share the same pool but each acquires its own
	// connection inside withProvisionLock, mirroring two worker pods.
	svcA := NewShardService(pool, nil)
	svcB := NewShardService(pool, nil)

	held := make(chan struct{})    // closed once A holds the lock
	release := make(chan struct{}) // A keeps the lock until this closes
	var aRan, bRan bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := svcA.withProvisionLock(ctx, func(*pgxpool.Conn) error {
			aRan = true
			close(held)
			<-release
			return nil
		}); err != nil {
			t.Errorf("svcA withProvisionLock: %v", err)
		}
	}()

	<-held // A is now holding the lock
	// B must NOT run its critical section while A holds the lock.
	if err := svcB.withProvisionLock(ctx, func(*pgxpool.Conn) error {
		bRan = true
		return nil
	}); err != nil {
		t.Fatalf("svcB withProvisionLock: %v", err)
	}
	if bRan {
		t.Fatal("svcB ran critical section while svcA held the lock — lock did not serialise")
	}
	close(release)
	wg.Wait()
	if !aRan {
		t.Fatal("svcA never ran its critical section")
	}

	// After A released, B should now be able to acquire and run.
	bRan = false
	if err := svcB.withProvisionLock(ctx, func(*pgxpool.Conn) error {
		bRan = true
		return nil
	}); err != nil {
		t.Fatalf("svcB second attempt: %v", err)
	}
	if !bRan {
		t.Fatal("svcB could not acquire the lock after svcA released it")
	}
}

func TestAutoProvisionShardNoPoolNoProvisioner(t *testing.T) {
	t.Parallel()
	// nil pool → ListShards returns nil → needsProvision(true) → no
	// provisioner → ErrNoProvisioner.
	svc := NewShardService(nil, nil)
	_, err := svc.AutoProvisionShard(context.Background(), 0)
	if !errors.Is(err, ErrNoProvisioner) {
		t.Fatalf("want ErrNoProvisioner, got %v", err)
	}
}

func TestAutoProvisionShardRegistersViaProvisioner(t *testing.T) {
	t.Parallel()
	// nil pool makes RegisterShard return a stub row, so this exercises
	// the provision→register happy path without a database.
	prov := &fakeShardProvisioner{shard: Shard{Name: "shard-x", StalwartURL: "http://shard-x:8080"}}
	svc := NewShardService(nil, nil).SetProvisioner(prov)
	got, err := svc.AutoProvisionShard(context.Background(), 0)
	if err != nil {
		t.Fatalf("AutoProvisionShard: %v", err)
	}
	if prov.calls != 1 {
		t.Fatalf("provisioner called %d times, want 1", prov.calls)
	}
	if got == nil || got.Name != "shard-x" {
		t.Fatalf("unexpected shard: %+v", got)
	}
}

func TestCapacityReportEmptyCluster(t *testing.T) {
	t.Parallel()
	// nil pool → ListShards returns nil → empty report. An empty
	// cluster has no active capacity, so provisioning is flagged.
	svc := NewShardService(nil, nil)
	rep, err := svc.CapacityReportWithThreshold(context.Background(), 0)
	if err != nil {
		t.Fatalf("CapacityReport: %v", err)
	}
	if rep.ActiveShards != 0 || rep.TotalCapacity != 0 {
		t.Fatalf("expected empty cluster, got %+v", rep)
	}
	if !rep.NeedsProvisioning {
		t.Fatal("empty cluster should need provisioning")
	}
	if rep.Threshold != DefaultProvisionThreshold {
		t.Fatalf("threshold = %v, want default %v", rep.Threshold, DefaultProvisionThreshold)
	}
}

func TestAutoProvisionShardPropagatesProvisionError(t *testing.T) {
	t.Parallel()
	prov := &fakeShardProvisioner{err: errors.New("terraform boom")}
	svc := NewShardService(nil, nil).SetProvisioner(prov)
	_, err := svc.AutoProvisionShard(context.Background(), 0)
	if err == nil {
		t.Fatal("expected provision error to propagate")
	}
}
