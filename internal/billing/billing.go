// Package billing hosts the Billing / Quota Service business
// logic: storage accounting, seat accounting, plan enforcement,
// zk-object-fabric usage event ingestion, invoice generation, and
// plan change workflows.
//
// Authoritative for the quotas table in docs/SCHEMA.md §5.7.
// See docs/ARCHITECTURE.md §7 and docs/PROPOSAL.md §11.
package billing

// Service is the placeholder root type for the Billing Service.
//
// It will hold dependencies (Postgres pool, zk-object-fabric
// usage feed, invoice provider) and expose plan management,
// quota enforcement, and usage rollup methods in Phase 2.
type Service struct{}
