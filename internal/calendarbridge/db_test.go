package calendarbridge

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestChannelResolverDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	r := NewDBChannelResolver(pool, "fallback-channel")
	ctx := context.Background()

	t.Cleanup(func() {
		_ = r.DeleteCalendarChannel(context.Background(), tenant, "")
		_ = r.DeleteCalendarChannel(context.Background(), tenant, "cal-1")
	})

	// No mapping yet → resolves to the static fallback.
	if ch, err := r.ResolveChannel(ctx, tenant, "cal-1"); err != nil || ch != "fallback-channel" {
		t.Fatalf("resolve fallback: ch=%q err=%v", ch, err)
	}
	if m, err := r.GetCalendarChannel(ctx, tenant, "cal-1"); err != nil || m != nil {
		t.Fatalf("get missing: m=%+v err=%v", m, err)
	}

	// Tenant default mapping.
	if _, err := r.SetCalendarChannel(ctx, tenant, "", "tenant-default"); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if ch, err := r.ResolveChannel(ctx, tenant, "cal-1"); err != nil || ch != "tenant-default" {
		t.Errorf("resolve default: ch=%q err=%v", ch, err)
	}

	// Per-calendar override takes priority.
	if _, err := r.SetCalendarChannel(ctx, tenant, "cal-1", "cal-channel"); err != nil {
		t.Fatalf("set per-calendar: %v", err)
	}
	if ch, err := r.ResolveChannel(ctx, tenant, "cal-1"); err != nil || ch != "cal-channel" {
		t.Errorf("resolve override: ch=%q err=%v", ch, err)
	}

	// Upsert updates the channel id in place.
	if m, err := r.SetCalendarChannel(ctx, tenant, "cal-1", "cal-channel-2"); err != nil || m.ChannelID != "cal-channel-2" {
		t.Errorf("upsert: m=%+v err=%v", m, err)
	}
	if m, err := r.GetCalendarChannel(ctx, tenant, "cal-1"); err != nil || m.ChannelID != "cal-channel-2" {
		t.Errorf("get after upsert: m=%+v err=%v", m, err)
	}

	// Delete the override → falls back to tenant default.
	if err := r.DeleteCalendarChannel(ctx, tenant, "cal-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if ch, err := r.ResolveChannel(ctx, tenant, "cal-1"); err != nil || ch != "tenant-default" {
		t.Errorf("resolve after delete: ch=%q err=%v", ch, err)
	}
}

func TestSharingStoreDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	store := NewSharingStore(pool)
	ctx := context.Background()

	// Validation.
	if _, err := store.ShareCalendar(ctx, tenant, "owner", "cal", "target", "bogus"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad perm: want ErrInvalidInput got %v", err)
	}
	if _, err := store.ShareCalendar(ctx, "", "", "", "", PermRead); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("missing fields: want ErrInvalidInput got %v", err)
	}

	share, err := store.ShareCalendar(ctx, tenant, "owner-acct", "cal-9", "target-acct", PermRead)
	if err != nil {
		t.Fatalf("ShareCalendar: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM calendar_shares WHERE id=$1::uuid`, share.ID)
	})

	// Upsert: re-share with a different permission updates in place.
	share2, err := store.ShareCalendar(ctx, tenant, "owner-acct", "cal-9", "target-acct", PermReadWrite)
	if err != nil || share2.ID != share.ID || share2.Permission != PermReadWrite {
		t.Fatalf("upsert share: %+v err=%v", share2, err)
	}

	shares, err := store.ListSharedCalendars(ctx, tenant, "target-acct")
	if err != nil || len(shares) != 1 || shares[0].Permission != PermReadWrite {
		t.Fatalf("ListSharedCalendars=%+v err=%v", shares, err)
	}

	// Resource calendars.
	if _, err := store.CreateResourceCalendar(ctx, tenant, ResourceCalendar{Name: "Room", ResourceType: "bogus"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad resource type: want ErrInvalidInput got %v", err)
	}
	rc, err := store.CreateResourceCalendar(ctx, tenant, ResourceCalendar{
		Name: "Boardroom", ResourceType: "room", Location: "HQ", Capacity: 10, CalDAVID: "res-1",
	})
	if err != nil {
		t.Fatalf("CreateResourceCalendar: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM resource_calendars WHERE id=$1::uuid`, rc.ID)
	})

	list, err := store.ListResourceCalendars(ctx, tenant)
	if err != nil {
		t.Fatalf("ListResourceCalendars: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == rc.ID {
			found = true
		}
	}
	if !found {
		t.Error("ListResourceCalendars missing created resource")
	}
}
