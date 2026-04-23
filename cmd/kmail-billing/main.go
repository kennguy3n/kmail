// Command kmail-billing is the Billing / Quota Service entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7 and
// docs/PROPOSAL.md §11): storage accounting, seat accounting,
// plan enforcement, zk-object-fabric usage event ingestion,
// invoice generation, and plan change workflows for the three
// retail tiers (KChat Core Email / Mail Pro / Privacy).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kmail-billing: not yet implemented")
	os.Exit(1)
}
