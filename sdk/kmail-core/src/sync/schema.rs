// SQLite schema and migration registry.
//
// Migrations are versioned by an integer in the `schema_version`
// table. New migrations append; never modify a shipped migration
// in-place — that would silently break clients whose database
// already ran the old version.

use crate::error::{Error, Result};
use rusqlite::Connection;

/// Pragmas applied on every connection open. Foreign keys are off
/// by default in SQLite; we depend on the cascade behaviour for
/// `email_mailboxes` so the pragma is mandatory.
pub(crate) const CONNECTION_PRAGMAS: &str = "
    PRAGMA journal_mode = WAL;
    PRAGMA synchronous = NORMAL;
    PRAGMA foreign_keys = ON;
    PRAGMA temp_store = MEMORY;
    PRAGMA busy_timeout = 5000;
";

/// Ordered list of (version, SQL). The schema migrator applies
/// every row whose version is greater than the current
/// `schema_version`.
pub(crate) const MIGRATIONS: &[(u32, &str)] = &[(
    1,
    "
        CREATE TABLE IF NOT EXISTS schema_version (
            version INTEGER NOT NULL PRIMARY KEY,
            applied_at INTEGER NOT NULL
        );

        CREATE TABLE IF NOT EXISTS mailboxes (
            id TEXT PRIMARY KEY,
            name TEXT NOT NULL,
            role TEXT,
            parent_id TEXT,
            sort_order INTEGER NOT NULL DEFAULT 0,
            total_emails INTEGER NOT NULL DEFAULT 0,
            unread_emails INTEGER NOT NULL DEFAULT 0,
            total_threads INTEGER NOT NULL DEFAULT 0,
            unread_threads INTEGER NOT NULL DEFAULT 0,
            is_vault INTEGER NOT NULL DEFAULT 0,
            rights_json TEXT,
            updated_at INTEGER NOT NULL
        );

        CREATE INDEX IF NOT EXISTS idx_mailboxes_parent_id
            ON mailboxes(parent_id);

        CREATE TABLE IF NOT EXISTS emails (
            id TEXT PRIMARY KEY,
            thread_id TEXT NOT NULL,
            blob_id TEXT,
            received_at INTEGER NOT NULL,
            sent_at INTEGER,
            size INTEGER NOT NULL DEFAULT 0,
            has_attachment INTEGER NOT NULL DEFAULT 0,
            subject TEXT NOT NULL DEFAULT '',
            preview TEXT NOT NULL DEFAULT '',
            from_json TEXT NOT NULL DEFAULT '[]',
            to_json TEXT NOT NULL DEFAULT '[]',
            cc_json TEXT NOT NULL DEFAULT '[]',
            bcc_json TEXT NOT NULL DEFAULT '[]',
            reply_to_json TEXT NOT NULL DEFAULT '[]',
            keywords_json TEXT NOT NULL DEFAULT '{}',
            body_text TEXT,
            body_html TEXT,
            updated_at INTEGER NOT NULL
        );

        CREATE INDEX IF NOT EXISTS idx_emails_received_at
            ON emails(received_at DESC);
        CREATE INDEX IF NOT EXISTS idx_emails_thread_id
            ON emails(thread_id);

        CREATE TABLE IF NOT EXISTS email_mailboxes (
            email_id TEXT NOT NULL,
            mailbox_id TEXT NOT NULL,
            PRIMARY KEY (email_id, mailbox_id),
            FOREIGN KEY (email_id) REFERENCES emails(id) ON DELETE CASCADE
        );

        CREATE INDEX IF NOT EXISTS idx_email_mailboxes_mailbox
            ON email_mailboxes(mailbox_id, email_id);

        CREATE TABLE IF NOT EXISTS sync_state (
            type_name TEXT NOT NULL PRIMARY KEY,
            state_token TEXT NOT NULL,
            last_synced_at INTEGER NOT NULL
        );

        CREATE TABLE IF NOT EXISTS pending_actions (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            kind TEXT NOT NULL,
            target_id TEXT NOT NULL,
            payload_json TEXT NOT NULL,
            enqueued_at INTEGER NOT NULL,
            attempts INTEGER NOT NULL DEFAULT 0,
            last_error TEXT
        );

        CREATE INDEX IF NOT EXISTS idx_pending_actions_enqueued
            ON pending_actions(enqueued_at);

        -- `fetched_at` and `last_accessed_at` are milliseconds since
        -- the Unix epoch (i.e. `chrono::Utc::now().timestamp_millis()`,
        -- via `cache::now_ms()`). Whole-second precision would let
        -- two back-to-back `put`s share a `last_accessed_at`, making
        -- the LRU eviction order in `cache::AttachmentCache::put`
        -- depend on SQLite's implementation-defined row order on ties.
        CREATE TABLE IF NOT EXISTS blob_cache (
            blob_id TEXT NOT NULL PRIMARY KEY,
            content_type TEXT,
            size INTEGER NOT NULL,
            fetched_at INTEGER NOT NULL,
            last_accessed_at INTEGER NOT NULL,
            payload BLOB NOT NULL
        );

        CREATE INDEX IF NOT EXISTS idx_blob_cache_lru
            ON blob_cache(last_accessed_at);
        ",
)];

/// Run every migration whose version is greater than the current
/// `schema_version`. Returns the new schema version.
pub(crate) fn migrate(conn: &mut Connection) -> Result<u32> {
    conn.execute_batch(CONNECTION_PRAGMAS)?;
    // Bootstrap: the very first migration creates `schema_version`,
    // so we need a probing query that survives the table not
    // existing yet. SQLite returns SQLITE_ERROR with the "no such
    // table" message; treat that as "version 0".
    let current: u32 = match conn.query_row(
        "SELECT COALESCE(MAX(version), 0) FROM schema_version",
        [],
        |row| row.get::<_, i64>(0),
    ) {
        Ok(v) => {
            u32::try_from(v).map_err(|e| Error::Store(format!("schema version overflow: {e}")))?
        }
        Err(rusqlite::Error::SqliteFailure(_, Some(msg))) if msg.contains("no such table") => 0,
        Err(rusqlite::Error::SqliteFailure(_, _)) => 0,
        Err(other) => return Err(Error::Store(format!("schema probe failed: {other}"))),
    };

    let tx = conn.transaction()?;
    let mut latest = current;
    for (version, sql) in MIGRATIONS {
        if *version > current {
            tx.execute_batch(sql)?;
            tx.execute(
                "INSERT INTO schema_version (version, applied_at) VALUES (?1, strftime('%s','now'))",
                rusqlite::params![version],
            )?;
            latest = *version;
        }
    }
    tx.commit()?;
    Ok(latest)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fresh_database_applies_all_migrations() {
        let mut conn = Connection::open_in_memory().unwrap();
        let v = migrate(&mut conn).unwrap();
        assert_eq!(v, MIGRATIONS.last().unwrap().0);
        // Re-applying is a no-op.
        let v2 = migrate(&mut conn).unwrap();
        assert_eq!(v2, v);

        // Every expected table exists.
        for name in [
            "schema_version",
            "mailboxes",
            "emails",
            "email_mailboxes",
            "sync_state",
            "pending_actions",
            "blob_cache",
        ] {
            let count: i64 = conn
                .query_row(
                    "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?1",
                    [name],
                    |r| r.get(0),
                )
                .unwrap();
            assert_eq!(count, 1, "table {name} missing");
        }
    }
}
