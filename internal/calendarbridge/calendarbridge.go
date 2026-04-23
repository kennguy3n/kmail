// Package calendarbridge hosts the Calendar Bridge business
// logic — the calendar side of the "KMail lives inside KChat"
// integration surface.
//
// Responsibilities (per docs/PROPOSAL.md §5 and
// docs/ARCHITECTURE.md §7): meeting creation from chat threads,
// RSVP-as-chat, resource calendars, and scheduling assistants.
// Talks to Stalwart CalDAV and the KChat API.
package calendarbridge

// Service is the placeholder root type for the Calendar Bridge.
//
// It will hold dependencies (Stalwart CalDAV client, KChat API
// client) and expose calendar ↔ chat operations in Phase 2.
type Service struct{}
