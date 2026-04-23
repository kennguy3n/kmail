// Command kmail-chat-bridge is the Chat Bridge entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7): bidirectional
// email ↔ KChat channel integration — share an email to a
// channel, route alerts from aliases like alerts@ into a channel,
// extract tasks from emails, and fan JMAP push events into KChat
// notifications (see docs/JMAP-CONTRACT.md §5.3).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kmail-chat-bridge: not yet implemented")
	os.Exit(1)
}
