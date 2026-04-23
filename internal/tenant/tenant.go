// Package tenant hosts the Tenant Service business logic: the
// tenant lifecycle (create / suspend / delete / rename / rotate),
// user lifecycle, aliases, shared inboxes, and quotas.
//
// Authoritative for the control-plane Postgres schema defined in
// docs/SCHEMA.md. See docs/ARCHITECTURE.md §7 for the service
// topology.
package tenant

// Service is the placeholder root type for the Tenant Service.
//
// It will hold dependencies (Postgres pool, KChat client, MLS
// client) and expose tenant / user / alias / shared-inbox
// lifecycle methods in Phase 2.
type Service struct{}
