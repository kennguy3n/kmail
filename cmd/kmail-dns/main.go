// Command kmail-dns is the DNS Onboarding Service entrypoint.
//
// Responsibilities (per docs/ARCHITECTURE.md §7 and
// docs/PROPOSAL.md §9.3): drive the DNS wizard — MX / SPF / DKIM /
// DMARC / MTA-STS / TLS-RPT / autoconfig discovery and
// verification. Talks to external DNS provider APIs (Cloudflare,
// Route 53) when the tenant opts in.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "kmail-dns: not yet implemented")
	os.Exit(1)
}
