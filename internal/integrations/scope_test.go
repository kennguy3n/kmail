package integrations

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/oauth"
	"github.com/kennguy3n/kmail/internal/webhooks"
)

// TestEventRequiredScope_IsExhaustive is the load-bearing test
// for the scope map. It walks every webhooks.Event* constant
// reachable from this package and asserts the map has a
// non-empty entry for it.
//
// Without this test, a future PR can add a new event type to
// internal/webhooks and the dispatcher will silently never
// deliver it to any third-party client (EventAllowedForClient
// returns false for unknown events by design — see
// scope.go:EventAllowedForClient).
func TestEventRequiredScope_IsExhaustive(t *testing.T) {
	// Hand-maintained list of constants — kept in sync with the
	// `const (` block in internal/webhooks/service.go. The
	// duplication is intentional: it forces a deliberate change
	// here when a new event is added, rather than relying on
	// reflection over an exported variable that might not exist.
	knownEvents := []string{
		webhooks.EventEmailReceived,
		webhooks.EventEmailBounced,
		webhooks.EventEmailComplaint,
		webhooks.EventCalendarCreated,
		webhooks.EventCalendarUpdated,
		webhooks.EventMigrationDone,
	}
	for _, ev := range knownEvents {
		t.Run(ev, func(t *testing.T) {
			scope, ok := EventRequiredScope[ev]
			if !ok {
				t.Fatalf("EventRequiredScope has no entry for %q — every event in internal/webhooks "+
					"must map to a scope here, or the dispatcher will silently drop the event "+
					"for every third-party client (see scope.go:EventAllowedForClient default case)", ev)
			}
			if scope == "" {
				t.Fatalf("EventRequiredScope[%q] = \"\" — empty scope is the admin-only sentinel; "+
					"if this event is intentionally admin-only, document the rationale inline; "+
					"otherwise pick the right scope", ev)
			}
		})
	}

	// Inverse direction: every entry in EventRequiredScope must
	// correspond to a real webhooks.Event* constant. A stale
	// entry (constant renamed but the map not updated) would
	// not be caught above.
	for ev := range EventRequiredScope {
		found := false
		for _, known := range knownEvents {
			if known == ev {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("EventRequiredScope has a stale entry for %q — constant no longer exists in webhooks package", ev)
		}
	}
}

// TestEventRequiredScope_PointsAtKnownScopes asserts every
// value in the map is a recognised scope. A typo (e.g.
// "read:mial") would otherwise silently cause every dispatch
// to fail the scope check.
func TestEventRequiredScope_PointsAtKnownScopes(t *testing.T) {
	for ev, sc := range EventRequiredScope {
		if sc == "" {
			continue // sentinel "admin-only"; not subject to this check
		}
		if _, ok := oauth.KnownScopes[sc]; !ok {
			t.Errorf("EventRequiredScope[%q] = %q is not in oauth.KnownScopes — typo or stale scope", ev, sc)
		}
	}
}

// TestEventAllowedForClient_GrantedScopeExact pins the basic
// "client has exactly the required scope" path.
func TestEventAllowedForClient_GrantedScopeExact(t *testing.T) {
	got := EventAllowedForClient([]string{oauth.ScopeReadMail}, webhooks.EventEmailReceived)
	if !got {
		t.Errorf("EventAllowedForClient(read:mail, email.received) = false; want true")
	}
}

// TestEventAllowedForClient_WriteImpliesRead pins the
// write:* → read:* hierarchy. This MUST behave the same way
// as oauth.AccessTokenContext.HasScope.
func TestEventAllowedForClient_WriteImpliesRead(t *testing.T) {
	cases := []struct {
		name        string
		granted     []string
		event       string
		wantAllowed bool
	}{
		{"write:mail implies read:mail for email events",
			[]string{oauth.ScopeWriteMail}, webhooks.EventEmailReceived, true},
		{"write:mail does NOT imply read:calendar",
			[]string{oauth.ScopeWriteMail}, webhooks.EventCalendarCreated, false},
		{"write:calendar implies read:calendar for calendar events",
			[]string{oauth.ScopeWriteCalendar}, webhooks.EventCalendarCreated, true},
		{"write:calendar does NOT imply read:mail",
			[]string{oauth.ScopeWriteCalendar}, webhooks.EventEmailReceived, false},
		{"write:contacts does NOT cross over to read:mail",
			[]string{oauth.ScopeWriteContacts}, webhooks.EventEmailReceived, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EventAllowedForClient(tc.granted, tc.event)
			if got != tc.wantAllowed {
				t.Errorf("EventAllowedForClient(%v, %q) = %v; want %v", tc.granted, tc.event, got, tc.wantAllowed)
			}
		})
	}
}

// TestEventAllowedForClient_ReadNeverImpliesWrite asserts the
// hierarchy is one-way: a read scope alone is enough to
// receive read events but a write scope must NOT be inferable
// from a read.
func TestEventAllowedForClient_ReadNeverImpliesWrite(t *testing.T) {
	// All EventRequiredScope values are read scopes today, so
	// holding the write scope is "extra" and irrelevant. This
	// test instead pins that a synthetic "write-required"
	// event would not be reachable through a read scope —
	// implemented by direct call to the helper.
	if oauth.ScopesInclude([]string{oauth.ScopeReadMail}, oauth.ScopeWriteMail) {
		t.Errorf("oauth.ScopesInclude(read:mail, write:mail) = true; read MUST NOT imply write")
	}
}

// TestEventAllowedForClient_NoMatchingScope_Denies pins the
// default-deny behaviour for clients with the wrong scope set.
func TestEventAllowedForClient_NoMatchingScope_Denies(t *testing.T) {
	got := EventAllowedForClient([]string{oauth.ScopeReadContacts}, webhooks.EventEmailReceived)
	if got {
		t.Errorf("EventAllowedForClient(read:contacts, email.received) = true; want false (no scope match)")
	}
}

// TestEventAllowedForClient_EmptyScopes_Denies asserts a
// client with NO granted scopes can never receive any event.
func TestEventAllowedForClient_EmptyScopes_Denies(t *testing.T) {
	for ev := range EventRequiredScope {
		t.Run(ev, func(t *testing.T) {
			if EventAllowedForClient([]string{}, ev) {
				t.Errorf("EventAllowedForClient([], %q) = true; want false (no scopes granted)", ev)
			}
			if EventAllowedForClient(nil, ev) {
				t.Errorf("EventAllowedForClient(nil, %q) = true; want false (no scopes granted)", ev)
			}
		})
	}
}

// TestEventAllowedForClient_UnknownEvent_Denies pins the
// deny-by-default behaviour for unknown event types. A future
// PR that adds an event type without updating
// EventRequiredScope will see every delivery dropped — better
// than silently allowing.
func TestEventAllowedForClient_UnknownEvent_Denies(t *testing.T) {
	got := EventAllowedForClient(
		[]string{oauth.ScopeReadMail, oauth.ScopeReadCalendar, oauth.ScopeReadContacts},
		"unknown.event_type",
	)
	if got {
		t.Errorf("EventAllowedForClient(all scopes, unknown.event_type) = true; want false (unknown event is deny-by-default)")
	}
}

// TestFilterEventsForClient_SplitsAllowedAndDenied is the
// integration test for the subscribe-time filter. Given a
// scope set and a requested-events list, it returns (allowed,
// denied) preserving input order.
func TestFilterEventsForClient_SplitsAllowedAndDenied(t *testing.T) {
	requested := []string{
		webhooks.EventEmailReceived,
		webhooks.EventCalendarCreated,
		webhooks.EventEmailBounced,
		"some.unknown.event",
		webhooks.EventCalendarUpdated,
	}
	// Granted only mail scope: the two calendar events and the
	// unknown event MUST be in the denied slice.
	allowed, denied := FilterEventsForClient([]string{oauth.ScopeReadMail}, requested)

	wantAllowed := []string{webhooks.EventEmailReceived, webhooks.EventEmailBounced}
	wantDenied := []string{webhooks.EventCalendarCreated, "some.unknown.event", webhooks.EventCalendarUpdated}

	if !reflect.DeepEqual(allowed, wantAllowed) {
		t.Errorf("allowed = %v; want %v", allowed, wantAllowed)
	}
	if !reflect.DeepEqual(denied, wantDenied) {
		t.Errorf("denied = %v; want %v", denied, wantDenied)
	}
}

// TestFilterEventsForClient_PreservesOrder pins the contract
// that the returned slices match the input order. A client
// surfacing the denied list to a user as "you also need scope
// X for events Y and Z" depends on the order being stable.
func TestFilterEventsForClient_PreservesOrder(t *testing.T) {
	requested := []string{
		webhooks.EventCalendarCreated,
		webhooks.EventEmailReceived,
		webhooks.EventCalendarUpdated,
		webhooks.EventEmailBounced,
	}
	allowed, denied := FilterEventsForClient([]string{oauth.ScopeReadMail}, requested)

	// Allowed should contain email events in their input order.
	wantAllowed := []string{webhooks.EventEmailReceived, webhooks.EventEmailBounced}
	if !reflect.DeepEqual(allowed, wantAllowed) {
		t.Errorf("allowed = %v; want %v (input order must be preserved)", allowed, wantAllowed)
	}
	// Denied should contain calendar events in their input order.
	wantDenied := []string{webhooks.EventCalendarCreated, webhooks.EventCalendarUpdated}
	if !reflect.DeepEqual(denied, wantDenied) {
		t.Errorf("denied = %v; want %v (input order must be preserved)", denied, wantDenied)
	}
}

// TestFilterEventsForClient_EmptyRequest_NoOp pins that an
// empty request list returns empty allowed and empty denied
// (not nil) so callers can range over either safely.
func TestFilterEventsForClient_EmptyRequest_NoOp(t *testing.T) {
	allowed, denied := FilterEventsForClient([]string{oauth.ScopeReadMail}, nil)
	if len(allowed) != 0 {
		t.Errorf("allowed = %v; want empty", allowed)
	}
	if len(denied) != 0 {
		t.Errorf("denied = %v; want empty", denied)
	}
}

// TestFilterEventsForClient_AllAllowed_DeniedIsEmpty pins the
// common-case shape (every event allowed) so the handler can
// distinguish "no events were denied" from "the user has no
// scopes at all".
func TestFilterEventsForClient_AllAllowed_DeniedIsEmpty(t *testing.T) {
	requested := []string{webhooks.EventEmailReceived, webhooks.EventEmailBounced}
	allowed, denied := FilterEventsForClient([]string{oauth.ScopeReadMail}, requested)
	if !reflect.DeepEqual(allowed, requested) {
		t.Errorf("allowed = %v; want %v (every requested event has read:mail scope)", allowed, requested)
	}
	if len(denied) != 0 {
		t.Errorf("denied = %v; want empty", denied)
	}
}

// TestIntegrationEligibleScopes_NoDuplicates pins the de-dup
// behaviour of the helper. Multiple events sharing a scope
// (e.g. EventEmailReceived and EventEmailBounced both require
// read:mail) must surface that scope ONCE in the eligibility
// list.
func TestIntegrationEligibleScopes_NoDuplicates(t *testing.T) {
	// integrationEligibleScopes() returns a slice cached by
	// sync.OnceValue at handlers.go:117 — every call site receives
	// the same backing array, and the contract at handlers.go:104-107
	// is "MUST NOT mutate". A direct sort.Strings(got) would
	// (a) mutate that shared cache (already sorted, so values
	// don't change today, but the precedent is wrong) and (b)
	// race against any future caller / test that adds
	// t.Parallel(). Copy first so this test has its own backing
	// array.
	got := append([]string(nil), integrationEligibleScopes()...)
	sort.Strings(got)
	seen := make(map[string]struct{}, len(got))
	for _, sc := range got {
		if _, dup := seen[sc]; dup {
			t.Errorf("integrationEligibleScopes returned duplicate %q in %v", sc, got)
		}
		seen[sc] = struct{}{}
	}
}

// TestIntegrationEligibleScopes_CoversAllReadScopes pins that
// the helper surfaces every read scope that any event maps to.
// A new event whose scope is already in the table should
// automatically pass through this assertion without a code
// change here — that's the whole point of deriving the list.
func TestIntegrationEligibleScopes_CoversAllReadScopes(t *testing.T) {
	got := integrationEligibleScopes()
	gotSet := make(map[string]struct{}, len(got))
	for _, sc := range got {
		gotSet[sc] = struct{}{}
	}
	for ev, sc := range EventRequiredScope {
		if sc == "" {
			continue
		}
		if _, ok := gotSet[sc]; !ok {
			t.Errorf("integrationEligibleScopes() did not surface scope %q (required by event %q)", sc, ev)
		}
	}
}

// TestIntegrationEligibleScopes_NoEmptyEntries pins that the
// "" sentinel (admin-only events) does not leak through to
// the boundary scope check. Including "" would let a token
// without any scope pass requireAnyIntegrationScope.
func TestIntegrationEligibleScopes_NoEmptyEntries(t *testing.T) {
	for _, sc := range integrationEligibleScopes() {
		if strings.TrimSpace(sc) == "" {
			t.Errorf("integrationEligibleScopes returned an empty / whitespace scope; this would bypass the boundary check")
		}
	}
}
