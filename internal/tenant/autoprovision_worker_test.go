package tenant

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"testing"
	"time"
)

// countingProvisioner is a ShardProvisioner that records its calls in
// a mutex-guarded counter (the worker invokes it from its own
// goroutine).
type countingProvisioner struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (c *countingProvisioner) Provision(context.Context, string) (Shard, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	if c.err != nil {
		return Shard{}, c.err
	}
	return Shard{Name: "auto", StalwartURL: "http://auto:8080"}, nil
}

func (c *countingProvisioner) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func runWorkerBriefly(t *testing.T, w *AutoProvisionWorker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("AutoProvisionWorker.Run did not return after context cancel")
	}
}

func TestAutoProvisionWorkerRunNilService(t *testing.T) {
	// A nil service returns immediately without panicking.
	(&AutoProvisionWorker{}).Run(context.Background())
}

func TestAutoProvisionWorkerRunProvisions(t *testing.T) {
	// nil pool → ListShards returns nil → empty cluster needs
	// provisioning → provisioner fires and RegisterShard returns a
	// stub row, exercising Run's success branch.
	prov := &countingProvisioner{}
	svc := NewShardService(nil, log.New(io.Discard, "", 0)).SetProvisioner(prov)
	w := &AutoProvisionWorker{Service: svc, Interval: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)}
	runWorkerBriefly(t, w)
	if prov.count() == 0 {
		t.Error("expected provisioner to be called at least once")
	}
}

func TestAutoProvisionWorkerRunNoProvisioner(t *testing.T) {
	// Over threshold but no provisioner wired → Run logs the
	// ErrNoProvisioner branch each tick and keeps looping until cancel.
	svc := NewShardService(nil, log.New(io.Discard, "", 0))
	w := &AutoProvisionWorker{Service: svc, Interval: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)}
	runWorkerBriefly(t, w)
}

func TestAutoProvisionWorkerRunProvisionError(t *testing.T) {
	// Provisioner returns an error → Run logs the err != nil branch.
	prov := &countingProvisioner{err: errors.New("terraform boom")}
	svc := NewShardService(nil, log.New(io.Discard, "", 0)).SetProvisioner(prov)
	w := &AutoProvisionWorker{Service: svc, Interval: 10 * time.Millisecond, Logger: log.New(io.Discard, "", 0)}
	runWorkerBriefly(t, w)
	if prov.count() == 0 {
		t.Error("expected provisioner to be called")
	}
}
