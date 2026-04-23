// Package middleware hosts shared HTTP middleware for the Go
// control plane.
//
// Responsibilities (per docs/PROPOSAL.md §5 and
// docs/ARCHITECTURE.md §7): KChat OIDC authentication, tenant
// context propagation (the `app.tenant_id` Postgres GUC that
// drives row-level security — see docs/SCHEMA.md §4), structured
// request logging, correlation ID injection
// (`X-KMail-Correlation-Id`), and rate limiting.
package middleware
