// `Store` — shared SQLite handle.
//
// Wraps `rusqlite::Connection` in an `Arc<Mutex<...>>` so the SDK
// can clone the handle freely across async tasks and across the
// UniFFI / napi binding boundaries. SQLite's WAL mode allows
// concurrent readers and a single writer; the `Mutex` here is
// sufficient for the SDK's workload (one writer + occasional
// reader on the UI thread).

use crate::error::{Error, Result};
use rusqlite::Connection;
use std::path::Path;
use std::sync::{Arc, Mutex};

/// Cloneable connection handle.
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
