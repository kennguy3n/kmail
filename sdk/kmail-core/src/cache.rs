// Attachment LRU disk cache backed by the local SQLite store.
//
// Stalwart's blob IDs are BLAKE3 content hashes (see
// docs/ARCHITECTURE.md §4), so the cache key is identity-stable
// across re-fetches and we can dedupe across mailboxes without
// extra bookkeeping. The cache is bounded by total bytes; LRU
// eviction runs on every successful `put` once the limit is hit.
//
// All SQLite access is funnelled through `Store::with_conn`, which
// converts a poisoned `Mutex<Connection>` into `Error::Store`
// rather than panicking. This matches the convention used by the
// repo types in `sync::*Repo` so a poisoned lock surfaces as a
// recoverable error everywhere, not "process aborts on cache call
// vs. error on repo call".

use crate::error::{Error, Result};
use crate::sync::store::Store;
use chrono::Utc;
use std::sync::Arc;

/// Injectable monotonic-ish clock returning the value the cache
/// will stamp into `last_accessed_at`.
///
/// **Production.** `wall_clock_ms` returns wall-clock
/// milliseconds since the Unix epoch. Millisecond precision (not
/// whole seconds) prevents two `put`s issued back-to-back from
/// sharing a timestamp, which would let the `ORDER BY
/// last_accessed_at ASC` eviction query fall back to SQLite's
/// implementation-defined row order — i.e. non-deterministic
/// LRU. `i64` covers us until year 292,277,026.
///
/// **Tests.** The cache is constructed with
/// [`AttachmentCache::with_clock`] passing a strictly
/// monotonically increasing counter (e.g. `AtomicI64::fetch_add`).
/// That removes the need for `std::thread::sleep` calls between
/// `put`s to enforce eviction order — the wall-clock sleep
/// approach is fragile on slow CI runners where scheduler stalls
/// can compress two timestamps into the same millisecond.
pub type ClockMs = Arc<dyn Fn() -> i64 + Send + Sync>;

/// Default wall-clock used when no explicit clock is supplied to
/// [`AttachmentCache::new`].
pub fn wall_clock_ms() -> i64 {
    Utc::now().timestamp_millis()
}

/// Attachment cache wrapping the shared `Store`.
pub struct AttachmentCache {
    store: Store,
    /// Soft cap on total cached bytes. Once exceeded, oldest
    /// entries by `last_accessed_at` are evicted until the cache
    /// fits.
    max_bytes: u64,
    /// Clock used to stamp `last_accessed_at`. Production uses
    /// `wall_clock_ms`; tests inject a monotonic counter via
    /// `with_clock` so eviction ordering is deterministic
    /// without `thread::sleep`.
    clock: ClockMs,
}

impl AttachmentCache {
    /// Construct a cache backed by the system wall clock. Use
    /// [`AttachmentCache::with_clock`] from tests to inject a
    /// deterministic monotonic counter instead.
    pub fn new(store: Store, max_bytes: u64) -> Self {
        Self {
            store,
            max_bytes,
            clock: Arc::new(wall_clock_ms),
        }
    }

    /// Construct a cache with a caller-supplied clock. The clock
    /// MUST return strictly monotonically increasing values
    /// across the lifetime of the cache, otherwise LRU eviction
    /// ordering becomes ambiguous. The production constructor
    /// (`new`) uses `wall_clock_ms`; tests use this entry point
    /// with an `AtomicI64::fetch_add(1, Relaxed)` counter to
    /// avoid wall-clock dependence (which is fragile on slow CI
    /// runners where the OS scheduler can stall longer than the
    /// sleep interval and collapse two timestamps onto the same
    /// millisecond).
    pub fn with_clock(store: Store, max_bytes: u64, clock: ClockMs) -> Self {
        Self {
            store,
            max_bytes,
            clock,
        }
    }

    fn now_ms(&self) -> i64 {
        (self.clock)()
    }

    /// Insert or refresh a blob in the cache, evicting LRU entries
    /// if the total cached bytes would exceed `max_bytes`.
    ///
    /// Payloads larger than `max_bytes` are rejected up-front with
    /// `Error::InvalidArgument` — caching them would be a no-op (the
    /// eviction sweep would immediately reclaim the just-inserted row
    /// after `get()` returns `None`), so we fail loudly instead of
    /// silently dropping the write.
    ///
    /// Eviction never targets the just-inserted blob; the candidate
    /// SELECT explicitly excludes `blob_id`. This guarantees that any
    /// successful `put(id, ...)` followed by `get(id)` returns the
    /// payload, no matter how many other entries get evicted.
    pub fn put(&self, blob_id: &str, content_type: Option<&str>, payload: &[u8]) -> Result<()> {
        if blob_id.is_empty() {
            return Err(Error::InvalidArgument("blob_id is empty".into()));
        }
        let payload_len = payload.len() as u64;
        if payload_len > self.max_bytes {
            return Err(Error::InvalidArgument(format!(
                "payload of {payload_len} bytes exceeds attachment_cache_bytes ({})",
                self.max_bytes,
            )));
        }
        let now = self.now_ms();
        let max_bytes = self.max_bytes;
        self.store.with_conn(|conn| {
            let tx = conn.transaction()?;
            tx.execute(
                "INSERT INTO blob_cache (blob_id, content_type, size, fetched_at, last_accessed_at, payload)
                 VALUES (?1, ?2, ?3, ?4, ?4, ?5)
                 ON CONFLICT(blob_id) DO UPDATE SET
                    content_type = excluded.content_type,
                    size = excluded.size,
                    fetched_at = excluded.fetched_at,
                    last_accessed_at = excluded.last_accessed_at,
                    payload = excluded.payload",
                rusqlite::params![
                    blob_id,
                    content_type,
                    payload.len() as i64,
                    now,
                    payload,
                ],
            )?;
            // Evict LRU rows until total cached bytes fit. Exclude the
            // just-inserted row so a payload at the capacity boundary
            // never kicks itself out.
            let total: i64 =
                tx.query_row("SELECT COALESCE(SUM(size), 0) FROM blob_cache", [], |r| {
                    r.get(0)
                })?;
            if (total as u64) > max_bytes {
                let mut over = (total as u64) - max_bytes;
                let mut victims: Vec<String> = Vec::new();
                {
                    let mut stmt = tx.prepare(
                        "SELECT blob_id, size FROM blob_cache \
                         WHERE blob_id != ?1 \
                         ORDER BY last_accessed_at ASC",
                    )?;
                    let mut rows = stmt.query([blob_id])?;
                    while let Some(row) = rows.next()? {
                        if over == 0 {
                            break;
                        }
                        let id: String = row.get(0)?;
                        let sz: i64 = row.get(1)?;
                        victims.push(id);
                        over = over.saturating_sub(sz.max(0) as u64);
                    }
                }
                for v in victims {
                    tx.execute("DELETE FROM blob_cache WHERE blob_id = ?1", [&v])?;
                }
            }
            tx.commit()?;
            Ok(())
        })
    }

    /// Return the cached payload (and refresh its `last_accessed_at`)
    /// or `None` if absent.
    pub fn get(&self, blob_id: &str) -> Result<Option<Vec<u8>>> {
        self.store.with_conn(|conn| {
            let tx = conn.transaction()?;
            let payload: Option<Vec<u8>> = tx
                .query_row(
                    "SELECT payload FROM blob_cache WHERE blob_id = ?1",
                    [blob_id],
                    |r| r.get::<_, Vec<u8>>(0),
                )
                .map(Some)
                .or_else(|e| match e {
                    rusqlite::Error::QueryReturnedNoRows => {
                        Ok::<Option<Vec<u8>>, rusqlite::Error>(None)
                    }
                    other => Err(other),
                })?;
            if payload.is_some() {
                tx.execute(
                    "UPDATE blob_cache SET last_accessed_at = ?1 WHERE blob_id = ?2",
                    rusqlite::params![self.now_ms(), blob_id],
                )?;
            }
            tx.commit()?;
            Ok(payload)
        })
    }

    /// Total cached bytes. Used by the desktop "Storage" pane.
    pub fn total_bytes(&self) -> Result<u64> {
        self.store.with_conn(|conn| {
            let total: i64 =
                conn.query_row("SELECT COALESCE(SUM(size), 0) FROM blob_cache", [], |r| {
                    r.get(0)
                })?;
            Ok(total.max(0) as u64)
        })
    }

    /// Drop every cached entry. Used by the "Clear cache" affordance.
    pub fn purge(&self) -> Result<u64> {
        self.store.with_conn(|conn| {
            let purged = conn.execute("DELETE FROM blob_cache", [])?;
            Ok(purged as u64)
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sync::store::Store;
    use tempfile::{tempdir, TempDir};

    /// Test harness that keeps the `TempDir` alive for the
    /// lifetime of the `Store`.
    ///
    /// Previous implementation used `std::mem::forget(dir)` to
    /// stop the `TempDir` `Drop` from racing the `Store`'s
    /// SQLite `Connection`, but that leaked every temporary
    /// directory the test suite created. Holding the `TempDir`
    /// here makes the cleanup deterministic: the explicit
    /// `Drop` impl below closes the SQLite connection BEFORE
    /// the `TempDir` field is implicitly dropped (and unlinks
    /// the on-disk database file).
    ///
    /// **Drop-order robustness.** The previous shape of this
    /// helper relied on declaration order of the `store` /
    /// `_dir` struct fields (Rust drops fields top-to-bottom).
    /// That contract is real but fragile: a future refactor
    /// that "sorts the fields alphabetically" or "groups
    /// public fields first" would silently flip the drop
    /// order, unlink the database while the connection is
    /// still open, and produce platform-dependent test flakes
    /// (especially on Windows, where unlinking an open file
    /// is forbidden). Wrapping `store` in `Option` and
    /// dropping it explicitly in `Drop` makes the ordering
    /// guarantee independent of the struct layout — the
    /// manual `Drop::drop` runs BEFORE the compiler-generated
    /// field drops, so `store.take()` shuts the connection
    /// while the directory is still alive regardless of how
    /// the fields are ordered.
    struct StoreEnv {
        store: Option<Store>,
        // Held only to keep the directory alive — never read
        // directly, hence the underscore-prefixed binding.
        // The `#[allow(dead_code)]` is defensive against
        // future clippy versions that don't recognise the
        // prefix convention.
        #[allow(dead_code)]
        _dir: TempDir,
    }

    impl StoreEnv {
        fn new() -> Self {
            let _dir = tempdir().unwrap();
            let path = _dir.path().join("kmail.db");
            let store = Store::open(&path).unwrap();
            Self {
                store: Some(store),
                _dir,
            }
        }

        /// Reach the inner `Store`. Panics only if accessed
        /// after `Drop::drop` has already taken the value,
        /// which would mean a use-after-free in the test
        /// harness and should fail loudly.
        fn store(&self) -> &Store {
            self.store
                .as_ref()
                .expect("StoreEnv::store accessed after Drop")
        }
    }

    impl Drop for StoreEnv {
        fn drop(&mut self) {
            // Run BEFORE the compiler drops `_dir`. Taking the
            // `Store` out of the `Option` drops its
            // `Arc<Mutex<Connection>>`; when that's the last
            // outstanding clone of the connection (the test
            // clones are scoped to the closing `}`), SQLite
            // closes the file handle here. THEN `_dir` runs
            // and unlinks the (now-closed) database file.
            drop(self.store.take());
        }
    }

    fn fresh_env() -> StoreEnv {
        StoreEnv::new()
    }

    /// Build a strictly-monotonic injected clock for the
    /// LRU-ordering tests. Production uses wall-clock
    /// milliseconds via `AttachmentCache::new`, but the tests
    /// need every `put`/`get` to observe a value strictly greater
    /// than every previous one without relying on
    /// `std::thread::sleep` to elapse real time. Wall-clock-based
    /// timing is fragile on slow CI runners where scheduler
    /// stalls can compress two timestamps onto the same
    /// millisecond, making LRU ordering ambiguous (the
    /// `ORDER BY last_accessed_at` query then falls back to
    /// SQLite's implementation-defined row order). The atomic
    /// counter avoids the issue entirely.
    fn monotonic_clock() -> ClockMs {
        use std::sync::atomic::{AtomicI64, Ordering};
        let counter = Arc::new(AtomicI64::new(1));
        Arc::new(move || counter.fetch_add(1, Ordering::Relaxed))
    }

    #[test]
    fn put_get_roundtrip_updates_lru() {
        let env = fresh_env();
        let store = env.store().clone();
        let cache = AttachmentCache::new(store, 1024 * 1024);
        cache
            .put("blob-a", Some("text/plain"), b"hello world")
            .unwrap();

        let got = cache.get("blob-a").unwrap().unwrap();
        assert_eq!(got, b"hello world");
        assert_eq!(cache.total_bytes().unwrap(), 11);

        assert!(cache.get("missing").unwrap().is_none());
    }

    /// When `max_bytes` is exceeded, eviction must drop the LRU
    /// entry — never the just-inserted one.
    ///
    /// Uses an injected monotonic clock so ordering is
    /// deterministic on CI — the previous `thread::sleep`-based
    /// approach could intermittently produce the same
    /// `last_accessed_at` for two `put`s if the scheduler stalled
    /// the test runner past the sleep interval, making the LRU
    /// query order ambiguous.
    #[test]
    fn eviction_respects_lru_order() {
        let env = fresh_env();
        let store = env.store().clone();
        let cache = AttachmentCache::with_clock(store, 20, monotonic_clock()); // hold ~2 small blobs

        cache.put("a", None, &[1u8; 8]).unwrap();
        cache.put("b", None, &[2u8; 8]).unwrap();
        // `a` is older (lower counter). Inserting `c` should
        // evict `a` because total bytes 24 > cap 20.
        cache.put("c", None, &[3u8; 8]).unwrap();

        assert!(
            cache.get("a").unwrap().is_none(),
            "LRU entry must be evicted"
        );
        assert!(cache.get("b").unwrap().is_some());
        assert!(cache.get("c").unwrap().is_some());
    }

    #[test]
    fn purge_clears_cache() {
        let env = fresh_env();
        let store = env.store().clone();
        let cache = AttachmentCache::new(store, 1024);
        cache.put("a", None, &[0u8; 8]).unwrap();
        cache.put("b", None, &[0u8; 8]).unwrap();
        assert_eq!(cache.purge().unwrap(), 2);
        assert_eq!(cache.total_bytes().unwrap(), 0);
    }

    /// Regression: empty `blob_id` must be rejected up-front, not
    /// reach the SQLite layer and produce a confusing constraint
    /// error.
    #[test]
    fn empty_blob_id_rejected() {
        let env = fresh_env();
        let store = env.store().clone();
        let cache = AttachmentCache::new(store, 1024);
        let err = cache.put("", None, b"x").unwrap_err();
        assert!(matches!(err, Error::InvalidArgument(_)));
    }

    /// Regression: a payload larger than `max_bytes` must be rejected
    /// up-front rather than silently inserted then immediately evicted
    /// by the LRU sweep (which would leave the caller observing
    /// `Ok(())` from `put` followed by `None` from `get`).
    #[test]
    fn oversized_payload_rejected() {
        let env = fresh_env();
        let store = env.store().clone();
        let cache = AttachmentCache::new(store, 16);
        let err = cache.put("too-big", None, &[0u8; 64]).unwrap_err();
        assert!(matches!(err, Error::InvalidArgument(_)));
        // And nothing was written.
        assert_eq!(cache.total_bytes().unwrap(), 0);
    }

    /// Regression: when the cache is already saturated, inserting a
    /// new blob right at the capacity boundary must keep the new blob
    /// retrievable (eviction targets older entries, not the row we
    /// just wrote).
    ///
    /// Uses an injected monotonic clock for the same reason as
    /// `eviction_respects_lru_order` — the boundary case depends
    /// on the strict `old-1 < old-2 < new` ordering, which
    /// `thread::sleep` cannot guarantee under CI scheduler stalls.
    #[test]
    fn just_inserted_blob_survives_eviction_at_boundary() {
        let env = fresh_env();
        let store = env.store().clone();
        // Capacity holds exactly two 8-byte entries.
        let cache = AttachmentCache::with_clock(store, 16, monotonic_clock());

        cache.put("old-1", None, &[0u8; 8]).unwrap();
        cache.put("old-2", None, &[0u8; 8]).unwrap();
        // Inserting a third 8-byte blob would push total to 24, so
        // the cache must evict the oldest (`old-1`) — and must NOT
        // evict `new` itself.
        cache.put("new", None, &[1u8; 8]).unwrap();

        assert!(
            cache.get("new").unwrap().is_some(),
            "just-inserted blob must never be the eviction victim"
        );
        assert!(
            cache.get("old-1").unwrap().is_none(),
            "oldest LRU entry must be evicted to make room"
        );
        assert!(cache.get("old-2").unwrap().is_some());
    }
}
