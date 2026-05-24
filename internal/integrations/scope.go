package integrations

import (
	"github.com/kennguy3n/kmail/internal/oauth"
	"github.com/kennguy3n/kmail/internal/webhooks"
)

// EventRequiredScope maps the event types defined in
// internal/webhooks to the OAuth2 scope a third-party client
// must hold to receive that event.
//
// The map is the single source of truth — both
// RegisterWebhookForClient (subscribe-time check) and
// EventAllowedForClient (dispatch-time check) consult it. New
// event types MUST be added here at the same time they are
// introduced upstream; the unit test in scope_test.go pins this
// by walking every Event* constant in internal/webhooks and
// asserting the table is exhaustive. Without that test, a future
// PR could add a new event type and forget the scope mapping,
// which would default to "no client can subscribe" (deny-by-
// default — see EventRequiredScope's empty-string return).
//
// Empty string in this map is a sentinel meaning "scope is
// undefined / not allowed for any third-party client" — used so
// callers can distinguish "lookup miss = unrecognised event"
// from "event exists but is admin-only". The current set has no
// admin-only events; if one is added later, the convention is to
// set its value to the sentinel "" so the dispatcher denies
// all client subscriptions.
var EventRequiredScope = map[string]string{
	webhooks.EventEmailReceived:   oauth.ScopeReadMail,
	webhooks.EventEmailBounced:    oauth.ScopeReadMail,
	webhooks.EventEmailComplaint:  oauth.ScopeReadMail,
	webhooks.EventCalendarCreated: oauth.ScopeReadCalendar,
	webhooks.EventCalendarUpdated: oauth.ScopeReadCalendar,
	webhooks.EventMigrationDone:   oauth.ScopeReadMail,
}

// EventAllowedForClient returns true if the given OAuth2 client
// is permitted to RECEIVE the given event, based on the scopes
// it was granted by the user. This is the dispatch-time check —
// the subscribe-time check uses the same logic via the public
// helper below.
//
// Defence-in-depth: a client's granted scope set may have been
// narrowed since the webhook was registered (e.g. the user
// re-consented with fewer scopes). The dispatcher consults this
// function for every fire so a stale subscription cannot
// exfiltrate data via a since-revoked scope.
//
// The scope-subset check delegates to oauth.ScopesInclude so the
// write:* → read:* implication is enforced consistently between
// the per-request middleware (AccessTokenContext.HasScope) and
// the dispatch-time path here.
func EventAllowedForClient(grantedScopes []string, eventType string) bool {
	required, known := EventRequiredScope[eventType]
	if !known || required == "" {
		// Unknown or admin-only event: never deliver to a
		// third-party client.
		return false
	}
	return oauth.ScopesInclude(grantedScopes, required)
}

// FilterEventsForClient returns the subset of `requested` event
// types that the client can subscribe to based on its granted
// scope set. Caller uses the returned slice for the actual
// subscription; if any requested event was filtered out, the
// caller should reply with a structured error so the integration
// learns which events it was denied.
//
// The returned `denied` slice carries the rejected event types
// in stable order (matches the input order) so the client can
// surface a precise "missing scope X for event Y" message.
//
// Both `allowed` and `denied` are always returned as
// non-nil slices (possibly empty) so callers that JSON-encode
// the result get `[]` for "none" instead of `null`. The
// `omitempty` tag on caller structs already collapses the
// empty case if needed; consistency with `allowed` is the goal.
func FilterEventsForClient(grantedScopes, requested []string) (allowed []string, denied []string) {
	allowed = make([]string, 0, len(requested))
	denied = make([]string, 0, len(requested))
	for _, ev := range requested {
		if EventAllowedForClient(grantedScopes, ev) {
			allowed = append(allowed, ev)
		} else {
			denied = append(denied, ev)
		}
	}
	return allowed, denied
}
