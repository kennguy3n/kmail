// Command kmail-calendar-bridge is the Calendar Bridge entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7): calendar ↔ KChat
// integration — meeting creation from chat threads, RSVP
// reminders as chat messages, resource calendars, and scheduling
// assistants. Talks to Stalwart CalDAV and the KChat API.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kmail-calendar-bridge: not yet implemented")
	os.Exit(1)
}
