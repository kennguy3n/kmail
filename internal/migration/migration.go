// Package migration hosts the Migration Orchestrator business
// logic: Gmail / IMAP imports via imapsync workers with
// checkpoint/resume, staged sync, and cutover workflows.
//
// The same binary (kmail-migration) also applies Postgres
// migrations from docs/SCHEMA.md. See docs/ARCHITECTURE.md §7.
package migration

// Service is the placeholder root type for the Migration
// Orchestrator.
//
// It will hold dependencies (imapsync worker pool, Stalwart
// admin client, Postgres pool) and expose tenant-scoped import
// and cutover methods in Phase 2.
type Service struct{}
