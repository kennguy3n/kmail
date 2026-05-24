// `Store` — shared SQLite handle.
//
// Wraps `rusqlite::Connection` in an `Arc<Mutex<...>>` so the SDK
// can clone the handle freely across async tasks and across the
// UniFFI / napi binding boundaries. SQLite's WAL mode allows
// concurrent readers and a single writer at the *engine* level;
// the `Mutex` here serialises ALL access (read and write) at the
// Rust level, which is a deliberately conservative choice rather
// than an oversight.
//
// ## Why a single Mutex<Connection> rather than r2d2 / a writer
// + reader-pool split
//
// The SDK's workload is **mailbox-shaped, not OLAP-shaped**:
//
//   * `sync()` is the only path that issues sustained writes,
//     and it's a single-flight loop already — the JMAP delta
//     batches are sequential and there is no scenario where two
//     concurrent `sync()`s should be in flight against one
//     account.
//   * Reads come from the UI thread (`list_mailboxes`,
//     `list_emails_in_mailbox`, `fetch_email`) and from
//     `flush_pending_actions`. They are O(milliseconds) on the
//     local on-device SQLite; serialising them behind a Mutex
//     adds at most single-digit-microsecond contention even
//     under heavy UI scrolling.
//   * Every repo method on `EmailRepo`/`MailboxRepo`/`StateRepo`
//     /`ActionsRepo` already does its work in one
//     `with_conn(...)` closure, so the critical section is
//     bounded by exactly one user-visible operation. The
//     transactions in `apply_with_state` / `upsert_many_with_state`
//     / `replace_all_with_state` co-commit data + state-token
//     atomically inside that closure, so we never want to
//     interleave another writer mid-transaction anyway.
//
// A connection pool (r2d2 + connection-per-task) would buy
// concurrent reads but would also need separate connection
// lifecycle handling for the `PRAGMA foreign_keys = ON` /
// `journal_mode = WAL` setup, would complicate the
// SyncStateDiverged transactional recovery (which currently
// holds the mutex across the full DELETE + rehydrate + state
// commit sequence), and would not move the needle on either
// the steady-state read latency or the sync throughput.
//
// The escape hatch: if profiling on a real device ever shows
// UI threads blocked on the store mutex (the only failure mode
// where this matters), the fix is to split into two
// `Arc<Mutex<Connection>>` handles — one writer, one read-only
// — both pointing at the same on-disk file with WAL enabled.
// That's a contained refactor inside `Store`; nothing in the
// repos or the client surface depends on there being exactly
// one connection. We intentionally defer it until measurement
// justifies the complexity.

use crate::error::{Error, Result};
use rusqlite::Connection;
use std::path::Path;
use std::sync::{Arc, Mutex};

/// Cloneable connection handle. See the module-level comment
/// above for the design rationale behind the single
/// `Mutex<Connection>` (rather than a connection pool).
#[derive(Clone)]
pub struct Store {
    conn: Arc<Mutex<Connection>>,
}

impl Store {
    /// Open (or create) the SQLite database at `path` and run
    /// every pending migration before returning.
    pub fn open<P: AsRef<Path>>(path: P) -> Result<Self> {
        let path = path.as_ref();
        if let Some(parent) = path.parent() {
            if !parent.as_os_str().is_empty() {
                std::fs::create_dir_all(parent)?;
            }
        }
        let mut conn = Connection::open(path)?;
        super::schema::migrate(&mut conn)?;
        Ok(Self {
            conn: Arc::new(Mutex::new(conn)),
        })
    }

    /// Open an in-memory database. Used by tests and by the
    /// `kmail-cli` `--no-store` flag.
    pub fn open_in_memory() -> Result<Self> {
        let mut conn = Connection::open_in_memory()?;
        super::schema::migrate(&mut conn)?;
        Ok(Self {
            conn: Arc::new(Mutex::new(conn)),
        })
    }

    /// Run a synchronous closure against a locked connection. Used
    /// by the unit tests and by repos that need ad-hoc SQL.
    pub fn with_conn<R, F>(&self, f: F) -> Result<R>
    where
        F: FnOnce(&mut Connection) -> Result<R>,
    {
        let mut g = self
            .conn
            .lock()
            .map_err(|_| Error::Store("store mutex poisoned".into()))?;
        f(&mut g)
    }

    /// Returns the SQLite library version, primarily for the
    /// `kmail-cli doctor` debug command.
    pub fn sqlite_version() -> &'static str {
        rusqlite::version()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn open_in_memory_yields_migrated_schema() {
        let s = Store::open_in_memory().unwrap();
        let v: i64 = s
            .with_conn(|c| {
                Ok(
                    c.query_row("SELECT MAX(version) FROM schema_version", [], |r| {
                        r.get::<_, i64>(0)
                    })?,
                )
            })
            .unwrap();
        assert!(v >= 1);
    }

    #[test]
    fn open_creates_parent_directory() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("nested/dir/kmail.db");
        let _ = Store::open(&path).unwrap();
        assert!(path.exists());
    }
}
