// Package deliverability hosts the Deliverability Control Plane
// business logic: IP pool manager, warmup scheduler, suppression
// lists, bounce processor, DMARC report ingester, Gmail
// Postmaster / Yahoo feedback loop consumers, abuse scoring, and
// compromised-account detection.
//
// See docs/ARCHITECTURE.md §7 and docs/PROPOSAL.md §9.
package deliverability

// Service is the placeholder root type for the Deliverability
// Control Plane.
//
// It will hold dependencies (IP pool registry, suppression list,
// Postmaster / FBL API clients, Stalwart admin client) and expose
// pool-placement, warmup, and bounce-processing methods in
// Phase 2.
type Service struct{}
