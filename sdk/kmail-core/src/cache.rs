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

/// Attachment cache wrapping the shared `Store`.
pub struct AttachmentCache {
    store: Store,
    /// Soft cap on total cached bytes. Once exceeded, oldest
    /// entries by `last_accessed_at` are evicted until the cache
    /// fits.
    max_bytes: u64,
}

impl AttachmentCache {
    pub fn new(store: Store, max_bytes: u64) -> Self {
        Self { store, max_bytes }
    }

    /// Insert or refresh a blob in the cache, evicting LRU entries
    /// if the total cached bytes would exceed `max_bytes`.
    pub fn put(&self, blob_id: &str, content_type: Option<&str>, payload: &[u8]) -> Result<()> {
        if blob_id.is_empty() {
            return Err(Error::InvalidArgument("blob_id is empty".into()));
        }
        let now = Utc::now().timestamp();
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
            // Evict LRU rows until total cached bytes fit.
            let total: i64 =
                tx.query_row("SELECT COALESCE(SUM(size), 0) FROM blob_cache", [], |r| {
                    r.get(0)
                })?;
            if (total as u64) > max_bytes {
                let mut over = (total as u64) - max_bytes;
                let mut victims: Vec<String> = Vec::new();
                {
                    let mut stmt = tx.prepare(
                        "SELECT blob_id, size FROM blob_cache ORDER BY last_accessed_at ASC",
                    )?;
                    let mut rows = stmt.query([])?;
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
                    rusqlite::params![Utc::now().timestamp(), blob_id],
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
    use tempfile::tempdir;

    fn fresh_store() -> Store {
        let dir = tempdir().unwrap();
        let path = dir.path().join("kmail.db");
        // Leak `dir` so the file outlives the test scope.
        std::mem::forget(dir);
        Store::open(&path).unwrap()
    }

    #[test]
    fn put_get_roundtrip_updates_lru() {
        let store = fresh_store();
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
    #[test]
    fn eviction_respects_lru_order() {
        let store = fresh_store();
        let cache = AttachmentCache::new(store, 20); // hold ~2 small blobs

        cache.put("a", None, &[1u8; 8]).unwrap();
        // Sleep so `last_accessed_at` strictly orders the entries.
        std::thread::sleep(std::time::Duration::from_secs(1));
        cache.put("b", None, &[2u8; 8]).unwrap();
        // Now `a` is older. Inserting `c` should evict `a`.
        std::thread::sleep(std::time::Duration::from_secs(1));
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
        let store = fresh_store();
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
        let store = fresh_store();
        let cache = AttachmentCache::new(store, 1024);
        let err = cache.put("", None, b"x").unwrap_err();
        assert!(matches!(err, Error::InvalidArgument(_)));
    }
}
