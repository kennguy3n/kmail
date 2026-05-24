// Outbound action queue.
//
// JMAP writes (move, flag, send) get enqueued here when the device
// is offline. The reconnect loop drains the queue oldest-first and
// replays each action via `KMailClient::flush_pending_actions()`.
//
// Conflict policy follows docs/ARCHITECTURE.md §5: server wins for
// flags/moves, client wins for sends (the user explicitly intended
// to send, so we always retry until the BFF accepts or 4xxes).

use crate::error::{Error, Result};
use crate::sync::store::Store;
use chrono::Utc;
use serde::{Deserialize, Serialize};

/// Discriminator for queued offline actions.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PendingActionKind {
    /// `Email/set update` with new `keywords/$seen` etc.
    SetKeywords,
    /// `Email/set update` with new `mailboxIds`.
    MoveEmail,
    /// `Email/set destroy`.
    DeleteEmail,
    /// `EmailSubmission/set create`.
    SendEmail,
}

impl PendingActionKind {
    pub fn as_str(self) -> &'static str {
        match self {
            PendingActionKind::SetKeywords => "set_keywords",
            PendingActionKind::MoveEmail => "move_email",
            PendingActionKind::DeleteEmail => "delete_email",
            PendingActionKind::SendEmail => "send_email",
        }
    }

    pub fn from_db_str(s: &str) -> Result<Self> {
        Ok(match s {
            "set_keywords" => PendingActionKind::SetKeywords,
            "move_email" => PendingActionKind::MoveEmail,
            "delete_email" => PendingActionKind::DeleteEmail,
            "send_email" => PendingActionKind::SendEmail,
            other => {
                return Err(Error::Store(format!(
                    "unknown pending_action kind: {other}"
                )))
            }
        })
    }
}

/// Newtype around the autoincrement primary key.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct PendingActionId(pub i64);

/// A single queued action.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct PendingAction {
    pub id: PendingActionId,
    pub kind: PendingActionKind,
    /// JMAP object ID this action targets (Email ID or draft UUID).
    pub target_id: String,
    /// Action-specific payload. Schema is owned by each kind:
    ///   - `SetKeywords`: a JMAP path-style PatchObject —
    ///     `{"keywords/$seen": true, "keywords/$flagged": null, ...}`.
    ///     Per RFC 8620 §3.3, each `keywords/<name>` entry patches
    ///     the keyword in-place rather than replacing the whole
    ///     `keywords` property, so keywords absent from the payload
    ///     are preserved on the server. `KMailClient::enqueue_set_keywords`
    ///     is the only path that should produce this shape.
    ///   - `MoveEmail`:   a PatchObject —
    ///     `{"mailboxIds/<id>": true, ...}` (same path-style
    ///     semantics so existing membership is preserved).
    ///   - `DeleteEmail`: `{}` (target_id is sufficient).
    ///   - `SendEmail`:   the full `EmailDraft` serialised to JSON.
    pub payload: serde_json::Value,
    pub enqueued_at: i64,
    pub attempts: i64,
    pub last_error: Option<String>,
}

/// Repo over the `pending_actions` table.
pub struct ActionsRepo {
    store: Store,
}

impl ActionsRepo {
    pub fn new(store: Store) -> Self {
        Self { store }
    }

    /// Enqueue a new action. Returns the assigned ID.
    pub fn enqueue(
        &self,
        kind: PendingActionKind,
        target_id: &str,
        payload: &serde_json::Value,
    ) -> Result<PendingActionId> {
        if target_id.is_empty() {
            return Err(Error::InvalidArgument("target_id is empty".into()));
        }
        let payload_text = serde_json::to_string(payload)?;
        self.store.with_conn(|c| {
            c.execute(
                "INSERT INTO pending_actions (kind, target_id, payload_json, enqueued_at)
                 VALUES (?1, ?2, ?3, ?4)",
                rusqlite::params![
                    kind.as_str(),
                    target_id,
                    payload_text,
                    Utc::now().timestamp(),
                ],
            )?;
            let id = c.last_insert_rowid();
            Ok(PendingActionId(id))
        })
    }

    /// Return the oldest `limit` pending actions, oldest first.
    pub fn next_batch(&self, limit: u32) -> Result<Vec<PendingAction>> {
        let limit = limit.max(1) as i64;
        self.store.with_conn(|c| {
            let mut stmt = c.prepare(
                "SELECT id, kind, target_id, payload_json, enqueued_at, attempts, last_error
                 FROM pending_actions
                 ORDER BY enqueued_at ASC, id ASC
                 LIMIT ?1",
            )?;
            let rows = stmt.query_map([limit], |row| {
                let kind_s: String = row.get(1)?;
                let payload_s: String = row.get(3)?;
                Ok((
                    PendingActionId(row.get(0)?),
                    kind_s,
                    row.get::<_, String>(2)?,
                    payload_s,
                    row.get::<_, i64>(4)?,
                    row.get::<_, i64>(5)?,
                    row.get::<_, Option<String>>(6)?,
                ))
            })?;

            let mut out = Vec::new();
            for r in rows {
                let (id, kind_s, target, payload_s, enqueued_at, attempts, last_error) = r?;
                let kind = PendingActionKind::from_db_str(&kind_s)?;
                let payload = serde_json::from_str(&payload_s)?;
                out.push(PendingAction {
                    id,
                    kind,
                    target_id: target,
                    payload,
                    enqueued_at,
                    attempts,
                    last_error,
                });
            }
            Ok(out)
        })
    }

    /// Remove an action once the BFF has accepted it.
    pub fn complete(&self, id: PendingActionId) -> Result<()> {
        self.store.with_conn(|c| {
            c.execute("DELETE FROM pending_actions WHERE id = ?1", [id.0])?;
            Ok(())
        })
    }

    /// Record a transient failure: bump `attempts`, store the
    /// last error. Caller retries later.
    pub fn record_failure(&self, id: PendingActionId, err: &str) -> Result<()> {
        self.store.with_conn(|c| {
            c.execute(
                "UPDATE pending_actions
                    SET attempts = attempts + 1,
                        last_error = ?1
                  WHERE id = ?2",
                rusqlite::params![err, id.0],
            )?;
            Ok(())
        })
    }

    /// Count queued actions. Surfaced in the desktop status bar.
    pub fn count(&self) -> Result<u64> {
        self.store.with_conn(|c| {
            let n: i64 = c.query_row("SELECT COUNT(*) FROM pending_actions", [], |r| r.get(0))?;
            Ok(n.max(0) as u64)
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn enqueue_drain_cycle() {
        let store = Store::open_in_memory().unwrap();
        let repo = ActionsRepo::new(store);

        let id1 = repo
            .enqueue(
                PendingActionKind::SetKeywords,
                "email-1",
                &serde_json::json!({"keywords": {"$seen": true}}),
            )
            .unwrap();
        let id2 = repo
            .enqueue(
                PendingActionKind::MoveEmail,
                "email-1",
                &serde_json::json!({"mailboxIds": {"mbx-archive": true}}),
            )
            .unwrap();
        assert_eq!(repo.count().unwrap(), 2);

        let batch = repo.next_batch(10).unwrap();
        assert_eq!(batch.len(), 2);
        assert_eq!(batch[0].id, id1);
        assert_eq!(batch[1].id, id2);
        assert_eq!(batch[0].kind, PendingActionKind::SetKeywords);

        repo.complete(id1).unwrap();
        assert_eq!(repo.count().unwrap(), 1);

        repo.record_failure(id2, "503 from BFF").unwrap();
        let remaining = repo.next_batch(10).unwrap();
        assert_eq!(remaining.len(), 1);
        assert_eq!(remaining[0].attempts, 1);
        assert_eq!(remaining[0].last_error.as_deref(), Some("503 from BFF"));
    }

    #[test]
    fn empty_target_is_rejected() {
        let store = Store::open_in_memory().unwrap();
        let repo = ActionsRepo::new(store);
        assert!(matches!(
            repo.enqueue(PendingActionKind::DeleteEmail, "", &serde_json::Value::Null),
            Err(Error::InvalidArgument(_))
        ));
    }
}
