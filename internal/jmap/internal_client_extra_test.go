package jmap

import (
	"context"
	"testing"
	"time"
)

// TestInternalClientResolveAccountID exercises the cache-hit path:
// a primed proxy account cache lets ResolveAccountID return without
// touching Postgres (the dummy pool is unreachable, so a cache miss
// would error).
func TestInternalClientResolveAccountID(t *testing.T) {
	p := newTestProxy(t)
	p.PrimeAccountCache("t1", "u1", "acc-42")
	c, err := NewInternalClient(p)
	if err != nil {
		t.Fatalf("NewInternalClient: %v", err)
	}
	got, err := c.ResolveAccountID(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatalf("ResolveAccountID: %v", err)
	}
	if got != "acc-42" {
		t.Errorf("account = %q want acc-42", got)
	}
}

func TestInternalClientSetTimeout(t *testing.T) {
	p := newTestProxy(t)
	c, err := NewInternalClient(p)
	if err != nil {
		t.Fatalf("NewInternalClient: %v", err)
	}
	c.SetTimeout(2 * time.Second)
	if c.httpc.Timeout != 2*time.Second {
		t.Errorf("timeout = %v want 2s", c.httpc.Timeout)
	}
	// Non-positive resets to the default.
	c.SetTimeout(0)
	if c.httpc.Timeout != internalClientDefaultTimeout {
		t.Errorf("timeout = %v want default %v", c.httpc.Timeout, internalClientDefaultTimeout)
	}
}

func TestMethodErrorString(t *testing.T) {
	withDesc := &MethodError{Type: "invalidArguments", Description: "bad filter"}
	if withDesc.Error() != "jmap method error: invalidArguments: bad filter" {
		t.Errorf("Error()=%q", withDesc.Error())
	}
	noDesc := &MethodError{Type: "overQuota"}
	if noDesc.Error() != "jmap method error: overQuota" {
		t.Errorf("Error()=%q", noDesc.Error())
	}
}
