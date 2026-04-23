// Command kmail-api is the API Gateway / BFF entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7 and
// docs/JMAP-CONTRACT.md): translate KChat OIDC auth into Stalwart
// auth, proxy JMAP between the React client and Stalwart, enforce
// tenant policy and rate limits, and fan JMAP push events into
// KChat notifications via the Chat Bridge.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kmail-api: not yet implemented")
	os.Exit(1)
}
