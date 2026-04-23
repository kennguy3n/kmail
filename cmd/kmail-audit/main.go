// Command kmail-audit is the Audit / Compliance API entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7): tamper-evident
// audit log consumer, export tooling, eDiscovery preparation, and
// retention policy enforcement. Backed by the audit_log table
// defined in docs/SCHEMA.md §5.8.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kmail-audit: not yet implemented")
	os.Exit(1)
}
