// Package audit hosts the Audit / Compliance API business logic:
// a tamper-evident audit log consumer, export tooling, eDiscovery
// preparation, and retention policy enforcement.
//
// Backed by the audit_log table in docs/SCHEMA.md §5.8. See
// docs/ARCHITECTURE.md §7.
package audit

// Service is the placeholder root type for the Audit Service.
//
// It will hold dependencies (Postgres pool, hash-chain sealer,
// export pipeline) and expose audit ingest, query, and export
// methods in Phase 2.
type Service struct{}
