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
// The caller is expected to invoke this in a goroutine at
// startup. The function blocks until every (backend, shard)
// pair has been attempted, so the caller can wait on it during
// graceful shutdown if needed.
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
	for _, b := range backends {
		if b == nil {
			continue
		}
		for _, shardID := range ids {
			if err := b.EnsureIndex(ctx, shardID); err != nil {
				logger.Printf("search.EnsureSharedIndexes: backend=%s shard=%s: %v", b.Name(), shardID, err)
				continue
			}
		}
	}
	return nil
}
