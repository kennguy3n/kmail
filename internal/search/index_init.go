// Package search — startup initialisation for shared indexes.
//
// The shared backends (`SharedMeilisearchBackend`,
// `SharedOpenSearchBackend`) need their per-shard index to exist
// with the correct settings before the first per-tenant document
// is written. The lazy `ensureSettings` path on Meilisearch
// covers the steady state, but it pays one extra round-trip on
// the first IndexMessage call per shard per process. More
// importantly, OpenSearch will auto-create an index on first
// write without the keyword mapping we need, leading to a
// `tenant_id` field analysed as text and a broken term filter.
//
// `EnsureSharedIndexes` is the single source of truth. It iterates
// over every Stalwart shard known to the BFF and calls
// `EnsureIndex` on each shared backend that is wired. Designed
// to be called once at startup (cmd/kmail-api/main.go) AND
// re-callable at runtime if an operator adds a new shard while
// the BFF is up. Failures are logged but do not abort the
// caller — a missing shared index will be repaired on the next
// startup or by the lazy path (Meilisearch only).
package search

import (
	"context"
	"errors"
	"fmt"
	"log"
)

// SharedIndexEnsurer is the subset of either shared backend
// (`SharedMeilisearchBackend` / `SharedOpenSearchBackend`) that
// `EnsureSharedIndexes` consumes. Kept narrow so the
// initialiser doesn't depend on the full `SearchBackend`
// surface — a backend that opts into startup initialisation
// only needs to expose `EnsureIndex`.
type SharedIndexEnsurer interface {
	// Name returns the backend identifier (e.g.
	// "shared_meilisearch"). Used in log lines so an operator
	// can tell which backend an init failure came from.
	Name() string
	// EnsureIndex creates / configures the shared index for
	// the given Stalwart shard. MUST be idempotent.
	EnsureIndex(ctx context.Context, shardID string) error
}

// ShardLister is the subset of `tenant.ShardService` that the
// initialiser consumes. Decoupling here keeps the search
// package free of a dependency on internal/tenant.
type ShardLister interface {
	ListShardIDs(ctx context.Context) ([]string, error)
}

// EnsureSharedIndexes runs `EnsureIndex` on each shared backend
// for every active Stalwart shard. Per-shard failures are
// logged and do NOT abort the loop — a single misbehaving
// upstream must not block the rest of the fleet from starting.
//
// Designed to be called SYNCHRONOUSLY from `main` at startup so
// the admin / search surface can fail-fast on a misconfigured
// shared backend (e.g. wrong endpoint, bad credentials) rather
// than letting the first user-facing search call surface the
// problem. The current call site is `cmd/kmail-api/main.go`,
// which blocks the BFF startup until this returns. Do NOT wrap
// the call in `go EnsureSharedIndexes(...)`: that would race
// the first IndexMessage / SearchMessages call against the
// per-shard index creation and either skip the keyword mapping
// or hit `resource_already_exists_exception` on the lazy path.
//
// The function blocks until every (backend, shard) pair has
// been attempted. Per-shard failures are logged AND aggregated
// into the returned `EnsureSharedIndexesError` so the caller has
// a single high-signal log line (`N of M pairs failed`) in
// addition to the per-failure lines — operators don't have to
// grep + count individual entries to know if the fleet is
// healthy. The synchronous call is bounded by `(backends ×
// shards) × per-call timeout` and in practice completes in a
// few seconds even for fleets with dozens of shards because
// `EnsureIndex` is idempotent and cheap on the steady-state
// path.
func EnsureSharedIndexes(ctx context.Context, logger *log.Logger, shards ShardLister, backends []SharedIndexEnsurer) error {
	if logger == nil {
		logger = log.Default()
	}
	if shards == nil {
		return errors.New("search.EnsureSharedIndexes: shards lister is required")
	}
	if len(backends) == 0 {
		// No shared backend wired — nothing to initialise. The
		// per-tenant backends (`MeilisearchBackend`,
		// `OpenSearchBackend`) handle index creation lazily on
		// first write.
		return nil
	}
	ids, err := shards.ListShardIDs(ctx)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		logger.Printf("search.EnsureSharedIndexes: no shards registered, skipping initialisation")
		return nil
	}
	var (
		attempted int
		failures  []EnsureSharedIndexFailure
	)
	for _, b := range backends {
		if b == nil {
			continue
		}
		for _, shardID := range ids {
			attempted++
			if err := b.EnsureIndex(ctx, shardID); err != nil {
				// Keep the per-(backend, shard) log line: it's
				// load-bearing for operator debugging because
				// it carries the exact upstream error message.
				logger.Printf("search.EnsureSharedIndexes: backend=%s shard=%s: %v", b.Name(), shardID, err)
				failures = append(failures, EnsureSharedIndexFailure{
					Backend: b.Name(),
					ShardID: shardID,
					Err:     err,
				})
				continue
			}
		}
	}
	if len(failures) == 0 {
		return nil
	}
	// Aggregate so the caller has ONE high-signal log line
	// (`shared indexes init: 3 of 24 (backend, shard) pairs
	// failed`) on top of the per-shard lines. This is the
	// hook operators wire to a startup-health metric / alert
	// without forcing every alert backend to grep the per-shard
	// log lines.
	return &EnsureSharedIndexesError{Attempted: attempted, Failures: failures}
}

// EnsureSharedIndexFailure is one (backend, shard) pair whose
// EnsureIndex call failed during EnsureSharedIndexes.
type EnsureSharedIndexFailure struct {
	Backend string
	ShardID string
	Err     error
}

// EnsureSharedIndexesError aggregates per-(backend, shard)
// failures from a single EnsureSharedIndexes pass. The caller
// uses it to emit ONE high-signal log line / metric reflecting
// the whole pass without losing per-shard granularity (which is
// preserved via the per-failure log lines and the Failures
// slice).
//
// Wrapping `error` rather than e.g. returning a count keeps the
// shape compatible with the existing `if err != nil` caller
// pattern in `main.go` and makes `errors.As` / `errors.Is`
// work naturally for any future caller that wants to act on a
// specific upstream error (e.g. retry on `context.DeadlineExceeded`).
type EnsureSharedIndexesError struct {
	Attempted int
	Failures  []EnsureSharedIndexFailure
}

// Error renders the aggregate summary. Individual failures are
// not included to keep the line greppable; callers that need
// the full list inspect `Failures` directly.
func (e *EnsureSharedIndexesError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("search.EnsureSharedIndexes: %d of %d (backend, shard) pairs failed",
		len(e.Failures), e.Attempted)
}

// Unwrap returns the first underlying failure so `errors.Is` /
// `errors.As` can target a representative upstream error
// without losing the aggregate count via `Error()`.
func (e *EnsureSharedIndexesError) Unwrap() error {
	if e == nil || len(e.Failures) == 0 {
		return nil
	}
	return e.Failures[0].Err
}
