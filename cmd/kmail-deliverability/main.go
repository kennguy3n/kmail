// Command kmail-deliverability is the Deliverability Control Plane
// entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7 and
// docs/PROPOSAL.md §9): IP pool manager, warmup scheduler,
// suppression lists, bounce processor, DMARC report ingester,
// Gmail Postmaster / Yahoo feedback loop consumer, abuse scoring,
// and compromised-account detection.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kmail-deliverability: not yet implemented")
	os.Exit(1)
}
