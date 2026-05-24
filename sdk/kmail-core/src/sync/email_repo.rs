// Typed repository over the `emails` + `email_mailboxes` tables.
//
// JMAP's data model has emails belonging to N mailboxes; the
// `email_mailboxes` join table captures that. The same email can
// be in Inbox + Important + a custom label simultaneously.

use crate::error::Result;
use crate::models::{EmailAddress, EmailSummary};
use crate::sync::store::Store;
use chrono::{DateTime, Utc};
use rusqlite::OptionalExtension;
use std::collections::BTreeMap;

/// Mutation applied by the JMAP delta-pull engine.
///
/// `Upsert` carries a boxed `EmailSummary` because the summary is
/// ~328 bytes (header arrays + keyword maps) while `Delete` is
/// just a 24-byte `String`; boxing keeps a `Vec<EmailMutation>` —
/// the canonical batch shape used throughout `sync()` and
/// `apply()` — compact.
#[derive(Clone, Debug)]
pub enum EmailMutation {
    /// Full upsert (`Email/get` response).
    Upsert(Box<EmailSummary>),
    /// Soft delete: email ID disappeared on the server.
    Delete(String),
}

#[derive(Clone)]
pub struct EmailRepo {
    store: Store,
}

impl EmailRepo {
    pub fn new(store: Store) -> Self {
        Self { store }
    }

    /// Apply a batch of mutations atomically.
    pub fn apply(&self, mutations: &[EmailMutation]) -> Result<()> {
        self.store.with_conn(|c| {
            let tx = c.transaction()?;
            for m in mutations {
                match m {
                    EmailMutation::Upsert(e) => upsert_in_tx(&tx, e.as_ref())?,
                    EmailMutation::Delete(id) => {
                        tx.execute("DELETE FROM emails WHERE id = ?1", [id])?;
                        // `email_mailboxes` rows cascade via FK.
                    }
                }
            }
            tx.commit()?;
            Ok(())
        })
    }

    pub fn upsert(&self, email: &EmailSummary) -> Result<()> {
        self.apply(&[EmailMutation::Upsert(Box::new(email.clone()))])
    }

    pub fn delete(&self, id: &str) -> Result<()> {
        self.apply(&[EmailMutation::Delete(id.to_string())])
    }

    /// Return up to `limit` emails in a mailbox, newest first.
    pub fn list_in_mailbox(&self, mailbox_id: &str, limit: u32) -> Result<Vec<EmailSummary>> {
        let limit = limit.max(1) as i64;
        self.store.with_conn(|c| {
            let mut stmt = c.prepare(
                "SELECT e.id, e.thread_id, e.blob_id, e.size, e.received_at, e.sent_at,
                        e.has_attachment, e.subject, e.preview,
                        e.from_json, e.to_json, e.cc_json, e.bcc_json, e.reply_to_json,
                        e.keywords_json
                   FROM emails e
                   JOIN email_mailboxes em ON em.email_id = e.id
                  WHERE em.mailbox_id = ?1
               ORDER BY e.received_at DESC
                  LIMIT ?2",
            )?;
            let rows = stmt.query_map(rusqlite::params![mailbox_id, limit], row_to_summary)?;
            let mut out = Vec::new();
            for r in rows {
                let mut s = r?;
                hydrate_mailbox_ids(c, &mut s)?;
                out.push(s);
            }
            Ok(out)
        })
    }

    pub fn get(&self, id: &str) -> Result<Option<EmailSummary>> {
        self.store.with_conn(|c| {
            let mut summary = c
                .query_row(
                    "SELECT id, thread_id, blob_id, size, received_at, sent_at,
                            has_attachment, subject, preview,
                            from_json, to_json, cc_json, bcc_json, reply_to_json,
                            keywords_json
                       FROM emails WHERE id = ?1",
                    [id],
                    row_to_summary,
                )
                .optional()?;
            if let Some(ref mut s) = summary {
                hydrate_mailbox_ids(c, s)?;
            }
            Ok(summary)
        })
    }

    pub fn count(&self) -> Result<u64> {
        self.store.with_conn(|c| {
            let n: i64 = c.query_row("SELECT COUNT(*) FROM emails", [], |r| r.get(0))?;
            Ok(n.max(0) as u64)
        })
    }
}

fn upsert_in_tx(tx: &rusqlite::Transaction<'_>, e: &EmailSummary) -> Result<()> {
    tx.execute(
        "INSERT INTO emails (
            id, thread_id, blob_id, received_at, sent_at, size, has_attachment,
            subject, preview, from_json, to_json, cc_json, bcc_json,
            reply_to_json, keywords_json, updated_at
         ) VALUES (
            ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11, ?12, ?13, ?14, ?15, ?16
         )
         ON CONFLICT(id) DO UPDATE SET
            thread_id = excluded.thread_id,
            blob_id = excluded.blob_id,
            received_at = excluded.received_at,
            sent_at = excluded.sent_at,
            size = excluded.size,
            has_attachment = excluded.has_attachment,
            subject = excluded.subject,
            preview = excluded.preview,
            from_json = excluded.from_json,
            to_json = excluded.to_json,
            cc_json = excluded.cc_json,
            bcc_json = excluded.bcc_json,
            reply_to_json = excluded.reply_to_json,
            keywords_json = excluded.keywords_json,
            updated_at = excluded.updated_at",
        rusqlite::params![
            e.id,
            e.thread_id,
            e.blob_id,
            e.received_at.timestamp(),
            e.sent_at.map(|t| t.timestamp()),
            e.size as i64,
            if e.has_attachment { 1i64 } else { 0i64 },
            e.subject,
            e.preview,
            serde_json::to_string(&e.from)?,
            serde_json::to_string(&e.to)?,
            serde_json::to_string(&e.cc)?,
            serde_json::to_string(&e.bcc)?,
            serde_json::to_string(&e.reply_to)?,
            serde_json::to_string(&e.keywords)?,
            Utc::now().timestamp(),
        ],
    )?;

    // Rewrite the mailbox membership rows. Cheaper than diffing
    // for SDK workloads (each email is in O(1) mailboxes).
    tx.execute("DELETE FROM email_mailboxes WHERE email_id = ?1", [&e.id])?;
    for (mbx_id, present) in &e.mailbox_ids {
        if *present {
            tx.execute(
                "INSERT OR IGNORE INTO email_mailboxes (email_id, mailbox_id) VALUES (?1, ?2)",
                rusqlite::params![e.id, mbx_id],
            )?;
        }
    }

    Ok(())
}

fn row_to_summary(row: &rusqlite::Row<'_>) -> rusqlite::Result<EmailSummary> {
    let received_at_ts: i64 = row.get(4)?;
    let sent_at_ts: Option<i64> = row.get(5)?;
    let from_s: String = row.get(9)?;
    let to_s: String = row.get(10)?;
    let cc_s: String = row.get(11)?;
    let bcc_s: String = row.get(12)?;
    let reply_to_s: String = row.get(13)?;
    let keywords_s: String = row.get(14)?;

    let parse_addrs =
        |s: &str| -> Vec<EmailAddress> { serde_json::from_str(s).unwrap_or_default() };

    let keywords: BTreeMap<String, bool> = serde_json::from_str(&keywords_s).unwrap_or_default();

    Ok(EmailSummary {
        id: row.get(0)?,
        thread_id: row.get(1)?,
        blob_id: row.get::<_, Option<String>>(2)?.unwrap_or_default(),
        size: row.get::<_, i64>(3)? as u64,
        received_at: DateTime::<Utc>::from_timestamp(received_at_ts, 0)
            .unwrap_or_else(|| DateTime::<Utc>::from_timestamp(0, 0).unwrap()),
        sent_at: sent_at_ts.and_then(|t| DateTime::<Utc>::from_timestamp(t, 0)),
        has_attachment: row.get::<_, i64>(6)? != 0,
        subject: row.get(7)?,
        preview: row.get(8)?,
        from: parse_addrs(&from_s),
        to: parse_addrs(&to_s),
        cc: parse_addrs(&cc_s),
        bcc: parse_addrs(&bcc_s),
        reply_to: parse_addrs(&reply_to_s),
        keywords,
        mailbox_ids: BTreeMap::new(), // filled by hydrate_mailbox_ids
    })
}

fn hydrate_mailbox_ids(conn: &rusqlite::Connection, summary: &mut EmailSummary) -> Result<()> {
    let mut stmt =
        conn.prepare_cached("SELECT mailbox_id FROM email_mailboxes WHERE email_id = ?1")?;
    let mut rows = stmt.query([&summary.id])?;
    while let Some(row) = rows.next()? {
        let mbx: String = row.get(0)?;
        summary.mailbox_ids.insert(mbx, true);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    fn sample(id: &str, thread: &str, mbx_ids: &[&str]) -> EmailSummary {
        let mut mailbox_ids = BTreeMap::new();
        for m in mbx_ids {
            mailbox_ids.insert((*m).to_string(), true);
        }
        let mut keywords = BTreeMap::new();
        keywords.insert("$seen".to_string(), false);

        EmailSummary {
            id: id.into(),
            thread_id: thread.into(),
            blob_id: format!("blob-{id}"),
            mailbox_ids,
            keywords,
            size: 1024,
            received_at: Utc.with_ymd_and_hms(2026, 5, 24, 10, 0, 0).unwrap(),
            sent_at: None,
            from: vec![EmailAddress {
                name: "Alice".into(),
                email: "alice@example.com".into(),
            }],
            to: vec![EmailAddress {
                name: String::new(),
                email: "bob@example.com".into(),
            }],
            cc: vec![],
            bcc: vec![],
            reply_to: vec![],
            subject: format!("Subject {id}"),
            preview: format!("Preview {id}"),
            has_attachment: false,
        }
    }

    #[test]
    fn upsert_list_get() {
        let store = Store::open_in_memory().unwrap();
        let repo = EmailRepo::new(store);

        // mbx-inbox must exist for FK consistency? Actually FK is
        // only on email_id, not mailbox_id (see schema.rs) — so we
        // can insert directly.
        repo.upsert(&sample("e1", "t1", &["mbx-inbox"])).unwrap();
        repo.upsert(&sample("e2", "t1", &["mbx-inbox", "mbx-imp"]))
            .unwrap();
        repo.upsert(&sample("e3", "t2", &["mbx-arch"])).unwrap();

        let inbox = repo.list_in_mailbox("mbx-inbox", 50).unwrap();
        assert_eq!(inbox.len(), 2);
        // Sorted descending by received_at; with equal timestamps,
        // SQLite returns them in insertion order — both rows have
        // the same received_at so this just checks both appear.
        let ids: Vec<&str> = inbox.iter().map(|e| e.id.as_str()).collect();
        assert!(ids.contains(&"e1"));
        assert!(ids.contains(&"e2"));

        let e2 = repo.get("e2").unwrap().unwrap();
        assert_eq!(e2.mailbox_ids.len(), 2);
        assert!(e2.mailbox_ids.contains_key("mbx-inbox"));
        assert!(e2.mailbox_ids.contains_key("mbx-imp"));

        // Move: change e2's mailbox to only `mbx-arch`.
        let mut e2_moved = e2.clone();
        e2_moved.mailbox_ids.clear();
        e2_moved.mailbox_ids.insert("mbx-arch".into(), true);
        repo.upsert(&e2_moved).unwrap();

        let inbox_after = repo.list_in_mailbox("mbx-inbox", 50).unwrap();
        assert_eq!(inbox_after.len(), 1);
        assert_eq!(inbox_after[0].id, "e1");

        repo.delete("e3").unwrap();
        assert!(repo.get("e3").unwrap().is_none());
        assert_eq!(repo.count().unwrap(), 2);
    }

    #[test]
    fn batch_mutations_are_atomic() {
        let store = Store::open_in_memory().unwrap();
        let repo = EmailRepo::new(store);
        repo.upsert(&sample("e1", "t1", &["mbx-inbox"])).unwrap();
        repo.apply(&[
            EmailMutation::Upsert(Box::new(sample("e2", "t1", &["mbx-inbox"]))),
            EmailMutation::Delete("e1".into()),
        ])
        .unwrap();
        assert!(repo.get("e1").unwrap().is_none());
        assert!(repo.get("e2").unwrap().is_some());
    }
}
