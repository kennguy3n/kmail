// Package dns hosts the DNS Onboarding Service business logic:
// the DNS wizard that generates, validates, and monitors MX, SPF,
// DKIM, DMARC, MTA-STS, TLS-RPT, and autoconfig records for
// tenant-owned sending domains.
//
// See docs/ARCHITECTURE.md §7 and docs/PROPOSAL.md §9.3.
package dns

// Service is the placeholder root type for the DNS Onboarding
// Service.
//
// It will hold dependencies (DNS resolver, Postgres pool, DNS
// provider API clients) and expose domain verification and record
// management methods in Phase 2.
type Service struct{}
