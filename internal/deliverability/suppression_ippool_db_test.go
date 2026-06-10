package deliverability

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestCheckRecipientDB covers the pre-send suppression hook used by
// the JMAP proxy: a clean recipient passes, a suppressed one returns
// ErrSuppressed.
func TestCheckRecipientDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	// Clean recipient ⇒ no error.
	if err := svc.Suppression.CheckRecipient(ctx, tenant, "clean@example.com"); err != nil {
		t.Fatalf("CheckRecipient clean=%v want nil", err)
	}

	// Suppress, then the hook short-circuits with ErrSuppressed.
	if _, err := svc.Suppression.AddSuppression(ctx, tenant, "blocked@example.com", ReasonHardBounce, "test"); err != nil {
		t.Fatalf("AddSuppression: %v", err)
	}
	err := svc.Suppression.CheckRecipient(ctx, tenant, "Blocked@Example.com")
	if !errors.Is(err, ErrSuppressed) {
		t.Errorf("CheckRecipient suppressed=%v want ErrSuppressed", err)
	}

	// Validation guard.
	if err := svc.Suppression.CheckRecipient(ctx, "", "x@example.com"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("CheckRecipient empty tenant=%v want ErrInvalidInput", err)
	}
}

// TestSelectSendingIPActiveDB covers the SelectSendingIP success
// path: an active IP in the tenant's assigned pool is returned.
func TestSelectSendingIPActiveDB(t *testing.T) {
	svc, tenant := dbService(t)
	ctx := context.Background()

	// Ensure a pool of the target type exists, then assign the
	// tenant. AssignTenantPool resolves to the oldest pool of that
	// type, so we read back the resolved pool and add the active IP
	// there to keep the test deterministic regardless of any
	// leftover global pools.
	name := "active-" + itoa(time.Now().UnixNano())
	pool, err := svc.IPPool.CreatePool(ctx, CreatePoolInput{Name: name, PoolType: PoolMatureTrusted, Description: "d"})
	if err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	t.Cleanup(func() {
		raw := testsupport.Pool(t)
		_, _ = raw.Exec(context.Background(), `DELETE FROM ip_pools WHERE id = $1::uuid`, pool.ID)
	})

	if err := svc.IPPool.AssignTenantPool(ctx, tenant, PoolMatureTrusted, 5); err != nil {
		t.Fatalf("AssignTenantPool: %v", err)
	}
	assign, err := svc.IPPool.GetTenantPool(ctx, tenant)
	if err != nil {
		t.Fatalf("GetTenantPool: %v", err)
	}

	n := time.Now().UnixNano()
	addr := "10." + itoa((n>>16)&255) + "." + itoa((n>>8)&255) + "." + itoa(n&255)
	ip, err := svc.IPPool.AddIP(ctx, assign.PoolID, AddIPInput{Address: addr, ReverseDNS: "mx.example.com"})
	if err != nil {
		t.Fatalf("AddIP: %v", err)
	}
	// Promote the IP to active so it becomes selectable, and remove
	// it on cleanup (it may live in a pre-existing global pool).
	raw := testsupport.Pool(t)
	if _, err := raw.Exec(ctx, `UPDATE ip_addresses SET status='active', reputation_score=95 WHERE id=$1::uuid`, ip.ID); err != nil {
		t.Fatalf("promote ip: %v", err)
	}
	t.Cleanup(func() {
		c := testsupport.Pool(t)
		_, _ = c.Exec(context.Background(), `DELETE FROM ip_addresses WHERE id = $1::uuid`, ip.ID)
	})

	best, err := svc.IPPool.SelectSendingIP(ctx, tenant)
	if err != nil {
		t.Fatalf("SelectSendingIP: %v", err)
	}
	if best == nil || best.ID != ip.ID {
		t.Errorf("SelectSendingIP=%+v want id=%s", best, ip.ID)
	}
}
