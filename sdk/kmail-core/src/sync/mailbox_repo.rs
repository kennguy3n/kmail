// Typed repository over the `mailboxes` table.
//
// Keeps SQL out of the JMAP client — the JMAP layer hands raw
// `Mailbox` structs to this repo and the repo owns the storage
// shape (column names, JSON columns, the "rights" subobject
// serialisation).

use crate::error::{Error, Result};
use crate::models::{Mailbox, MailboxRights, MailboxRole};
use crate::sync::store::Store;
use chrono::Utc;
use rusqlite::OptionalExtension;

#[derive(Clone)]
pub struct MailboxRepo {
    store: Store,
}

impl MailboxRepo {
    pub fn new(store: Store) -> Self {
        Self { store }
    }

    /// Insert or update a single mailbox.
    pub fn upsert(&self, mbx: &Mailbox) -> Result<()> {
        let role_str = mbx.role.map(role_to_str);
        let rights_json = match mbx.my_rights {
            Some(ref r) => Some(serde_json::to_string(r)?),
            None => None,
        };
        self.store.with_conn(|c| {
            c.execute(
                "INSERT INTO mailboxes (
                    id, name, role, parent_id, sort_order,
                    total_emails, unread_emails, total_threads, unread_threads,
                    is_vault, rights_json, updated_at
                 ) VALUES (
                    ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12
                 )
                 ON CONFLICT(id) DO UPDATE SET
                    name = excluded.name,
                    role = excluded.role,
                    parent_id = excluded.parent_id,
                    sort_order = excluded.sort_order,
                    total_emails = excluded.total_emails,
                    unread_emails = excluded.unread_emails,
                    total_threads = excluded.total_threads,
                    unread_threads = excluded.unread_threads,
                    is_vault = excluded.is_vault,
                    rights_json = excluded.rights_json,
                    updated_at = excluded.updated_at",
                rusqlite::params![
                    mbx.id,
                    mbx.name,
                    role_str,
                    mbx.parent_id,
                    mbx.sort_order as i64,
                    mbx.total_emails as i64,
                    mbx.unread_emails as i64,
                    mbx.total_threads as i64,
                    mbx.unread_threads as i64,
                    if mbx.is_vault { 1i64 } else { 0i64 },
                    rights_json,
                    Utc::now().timestamp(),
                ],
            )?;
            Ok(())
        })
    }

    /// Bulk insert. Used by the initial bootstrap path.
    pub fn upsert_many(&self, mailboxes: &[Mailbox]) -> Result<()> {
        for m in mailboxes {
            self.upsert(m)?;
        }
        Ok(())
    }

    /// Remove a mailbox by ID. Cascades to `email_mailboxes`.
    pub fn delete(&self, id: &str) -> Result<()> {
        self.store.with_conn(|c| {
            c.execute("DELETE FROM mailboxes WHERE id = ?1", [id])?;
            c.execute("DELETE FROM email_mailboxes WHERE mailbox_id = ?1", [id])?;
            Ok(())
        })
    }

    pub fn list(&self) -> Result<Vec<Mailbox>> {
        self.store.with_conn(|c| {
            let mut stmt = c.prepare(
                "SELECT id, name, role, parent_id, sort_order,
                        total_emails, unread_emails, total_threads, unread_threads,
                        is_vault, rights_json
                   FROM mailboxes
               ORDER BY sort_order ASC, name ASC",
            )?;
            let rows = stmt.query_map([], row_to_mailbox)?;
            let mut out = Vec::new();
            for r in rows {
                out.push(r?);
            }
            Ok(out)
        })
    }

    pub fn get(&self, id: &str) -> Result<Option<Mailbox>> {
        self.store.with_conn(|c| {
            let opt = c
                .query_row(
                    "SELECT id, name, role, parent_id, sort_order,
                            total_emails, unread_emails, total_threads, unread_threads,
                            is_vault, rights_json
                       FROM mailboxes WHERE id = ?1",
                    [id],
                    row_to_mailbox,
                )
                .optional()?;
            Ok(opt)
        })
    }

    pub fn count(&self) -> Result<u64> {
        self.store.with_conn(|c| {
            let n: i64 = c.query_row("SELECT COUNT(*) FROM mailboxes", [], |r| r.get(0))?;
            Ok(n.max(0) as u64)
        })
    }
}

fn role_to_str(r: MailboxRole) -> &'static str {
    match r {
        MailboxRole::Inbox => "inbox",
        MailboxRole::Archive => "archive",
        MailboxRole::Drafts => "drafts",
        MailboxRole::Sent => "sent",
        MailboxRole::Trash => "trash",
        MailboxRole::Junk => "junk",
        MailboxRole::Important => "important",
        MailboxRole::All => "all",
        MailboxRole::Flagged => "flagged",
        MailboxRole::Vault => "vault",
        MailboxRole::Unknown => "unknown",
    }
}

fn role_from_str(s: &str) -> MailboxRole {
    match s {
        "inbox" => MailboxRole::Inbox,
        "archive" => MailboxRole::Archive,
        "drafts" => MailboxRole::Drafts,
        "sent" => MailboxRole::Sent,
        "trash" => MailboxRole::Trash,
        "junk" => MailboxRole::Junk,
        "important" => MailboxRole::Important,
        "all" => MailboxRole::All,
        "flagged" => MailboxRole::Flagged,
        "vault" => MailboxRole::Vault,
        _ => MailboxRole::Unknown,
    }
}

fn row_to_mailbox(row: &rusqlite::Row<'_>) -> rusqlite::Result<Mailbox> {
    let role_s: Option<String> = row.get(2)?;
    let role = role_s.map(|s| role_from_str(&s));
    let rights_json: Option<String> = row.get(10)?;
    let rights = rights_json
        .map(|s| serde_json::from_str::<MailboxRights>(&s))
        .transpose()
        .map_err(|e| {
            // Surface JSON errors as rusqlite InvalidColumnType so
            // the rusqlite::Result-typed callback can carry them.
            rusqlite::Error::FromSqlConversionFailure(
                10,
                rusqlite::types::Type::Text,
                Box::new(SerdeWrap(e.to_string())),
            )
        })?;
    Ok(Mailbox {
        id: row.get(0)?,
        name: row.get(1)?,
        role,
        parent_id: row.get(3)?,
        sort_order: row.get::<_, i64>(4)? as u32,
        total_emails: row.get::<_, i64>(5)? as u64,
        unread_emails: row.get::<_, i64>(6)? as u64,
        total_threads: row.get::<_, i64>(7)? as u64,
        unread_threads: row.get::<_, i64>(8)? as u64,
        is_vault: row.get::<_, i64>(9)? != 0,
        my_rights: rights,
    })
}

#[derive(Debug)]
struct SerdeWrap(String);

impl std::fmt::Display for SerdeWrap {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl std::error::Error for SerdeWrap {}

// Eliminate the unused import warning when no caller pulls Error
// directly (it's used transitively via the public `Result` alias).
#[allow(dead_code)]
fn _force_use_error() -> Error {
    Error::Store("unused".into())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    fn sample(id: &str, name: &str, role: Option<MailboxRole>) -> Mailbox {
        Mailbox {
            id: id.into(),
            name: name.into(),
            role,
            parent_id: None,
            sort_order: 0,
            total_emails: 0,
            unread_emails: 0,
            total_threads: 0,
            unread_threads: 0,
            is_vault: matches!(role, Some(MailboxRole::Vault)),
            my_rights: Some(MailboxRights {
                may_read_items: true,
                may_set_seen: true,
                ..Default::default()
            }),
        }
    }

    #[test]
    fn upsert_list_get_roundtrip() {
        let store = Store::open_in_memory().unwrap();
        let repo = MailboxRepo::new(store);
        let inbox = sample("mbx-1", "Inbox", Some(MailboxRole::Inbox));
        let vault = sample("mbx-2", "Confidential", Some(MailboxRole::Vault));
        repo.upsert_many(&[inbox.clone(), vault.clone()]).unwrap();

        let all = repo.list().unwrap();
        assert_eq!(all.len(), 2);
        let v = repo.get("mbx-2").unwrap().unwrap();
        assert_eq!(v.role, Some(MailboxRole::Vault));
        assert!(v.is_vault);
        assert!(v.my_rights.unwrap().may_read_items);

        // Upsert overwrites.
        let mut renamed = inbox.clone();
        renamed.name = "Inbox (renamed)".into();
        repo.upsert(&renamed).unwrap();
        assert_eq!(repo.get("mbx-1").unwrap().unwrap().name, "Inbox (renamed)");

        // Delete cascades.
        repo.delete("mbx-1").unwrap();
        assert!(repo.get("mbx-1").unwrap().is_none());
        assert_eq!(repo.count().unwrap(), 1);
        let _ = BTreeMap::<String, bool>::new(); // suppress unused-import in future edits
    }
}
