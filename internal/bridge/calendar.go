package bridge

// CalendarService is the placeholder root type for the Calendar
// Bridge.
//
// It will hold dependencies (Stalwart CalDAV client, KChat API
// client) and expose calendar ↔ chat operations in Phase 2:
// meeting creation from chat threads, RSVP-as-chat, resource
// calendars, and scheduling assistants. See
// docs/ARCHITECTURE.md §7.
type CalendarService struct{}
