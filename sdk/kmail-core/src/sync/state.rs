// JMAP state-token CRUD.
//
// JMAP's incremental-sync model hangs off opaque state tokens
// returned by `Foo/get` and `Foo/changes`. The SDK persists the
// most recent token per JMAP type so a cold start can resume from
// the last known checkpoint instead of re-pulling everything.

use crate::error::Result;
use crate::sync::store::Store;
use chrono::Utc;

/// Canonical JMAP type names we track state for. Spelled exactly
/// as they appear on the wire so callers can pass JMAP-derived
/// strings directly.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SyncTypeName {
    Email,
    Mailbox,
    EmailSubmission,
    Thread,
}

impl SyncTypeName {
    pub fn as_str(self) -> &'static str {
        match self {
            SyncTypeName::Email => "Email",
            SyncTypeName::Mailbox => "Mailbox",
            SyncTypeName::EmailSubmission => "EmailSubmission",
            SyncTypeName::Thread => "Thread",
        }
    }
}

/// CRUD over the `sync_state` table.
#[derive(Clone)]
pub struct StateRepo {
    store: Store,
}

impl StateRepo {
    pub fn new(store: Store) -> Self {
        Self { store }
    }

    /// Persist the latest known state token for `type_name`.
    /// Idempotent; overwrites the previous token.
    pub fn put(&self, type_name: SyncTypeName, token: &str) -> Result<()> {
        self.store.with_conn(|c| {
            c.execute(
                "INSERT INTO sync_state (type_name, state_token, last_synced_at) VALUES (?1, ?2, ?3)
                 ON CONFLICT(type_name) DO UPDATE SET
                    state_token = excluded.state_token,
                    last_synced_at = excluded.last_synced_at",
                rusqlite::params![type_name.as_str(), token, Utc::now().timestamp()],
            )?;
            Ok(())
        })
    }

    /// Fetch the latest known state token, or `None` if no sync
    /// has happened yet.
    pub fn get(&self, type_name: SyncTypeName) -> Result<Option<String>> {
        self.store.with_conn(|c| {
            let row = c.query_row(
                "SELECT state_token FROM sync_state WHERE type_name = ?1",
                [type_name.as_str()],
                |r| r.get::<_, String>(0),
            );
            match row {
                Ok(token) => Ok(Some(token)),
                Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
                Err(other) => Err(other.into()),
            }
        })
    }

    /// Returns the unix timestamp of the most recent sync for
    /// `type_name`, if any. Surfaced in the desktop "Sync status"
    /// pane.
    pub fn last_synced_at(&self, type_name: SyncTypeName) -> Result<Option<i64>> {
        self.store.with_conn(|c| {
            let row = c.query_row(
                "SELECT last_synced_at FROM sync_state WHERE type_name = ?1",
                [type_name.as_str()],
                |r| r.get::<_, i64>(0),
            );
            match row {
                Ok(t) => Ok(Some(t)),
                Err(rusqlite::Error::QueryReturnedNoRows) => Ok(None),
                Err(other) => Err(other.into()),
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn put_get_roundtrip() {
        let store = Store::open_in_memory().unwrap();
        let repo = StateRepo::new(store);

        assert!(repo.get(SyncTypeName::Email).unwrap().is_none());
        repo.put(SyncTypeName::Email, "state-1").unwrap();
        assert_eq!(
            repo.get(SyncTypeName::Email).unwrap().as_deref(),
            Some("state-1")
        );

        // Upsert.
        repo.put(SyncTypeName::Email, "state-2").unwrap();
        assert_eq!(
            repo.get(SyncTypeName::Email).unwrap().as_deref(),
            Some("state-2")
        );

        // Distinct keys per type.
        repo.put(SyncTypeName::Mailbox, "state-mbx").unwrap();
        assert_eq!(
            repo.get(SyncTypeName::Mailbox).unwrap().as_deref(),
            Some("state-mbx")
        );
        assert_eq!(
            repo.get(SyncTypeName::Email).unwrap().as_deref(),
            Some("state-2")
        );
    }

    #[test]
    fn last_synced_at_tracks_writes() {
        let store = Store::open_in_memory().unwrap();
        let repo = StateRepo::new(store);
        repo.put(SyncTypeName::Email, "state-1").unwrap();
        let t = repo.last_synced_at(SyncTypeName::Email).unwrap().unwrap();
        // Just verify it's a sane unix timestamp.
        assert!(t > 1_000_000_000);
    }
}
