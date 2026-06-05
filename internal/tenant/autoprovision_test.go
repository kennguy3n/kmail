package tenant

import (
	"context"
	"errors"
	"testing"
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
