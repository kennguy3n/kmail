// Package config hosts shared configuration loading for the Go
// control plane — environment variables, config files, and
// secrets-manager references consumed by every cmd/* binary.
//
// Responsibilities (per docs/PROPOSAL.md §5 and
// docs/ARCHITECTURE.md §7): provide a single strongly-typed
// surface for Postgres / Valkey / Stalwart / zk-object-fabric /
// KChat endpoint wiring so individual services do not roll their
// own configuration schema.
package config
