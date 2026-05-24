// Offline sync subsystem.
//
// Sub-modules:
//
//   schema   — SQL migrations applied at `Store::open`. Each
//              migration is a self-contained CREATE / ALTER stanza.
//              No-op on already-current databases.
//   store    — `Store` connection handle. Wraps a `rusqlite::Connection`
//              behind a `Mutex` so multiple async tasks can share
//              the same handle without re-opening the DB.
//   state    — JMAP state-token CRUD.
//   actions  — Pending-action queue for offline writes.
//   mailbox_repo / email_repo — typed upsert / query helpers.
//
// What's NOT here: the live JMAP delta-pull engine. That lives in
// `client.rs` since it needs the JMAP client + the store + the
// crypto layer composed together.

pub mod actions;
pub mod email_repo;
pub mod mailbox_repo;
pub mod schema;
pub mod state;
pub mod store;

pub use actions::{ActionsRepo, PendingAction, PendingActionId, PendingActionKind};
pub use email_repo::{EmailMutation, EmailRepo};
pub use mailbox_repo::MailboxRepo;
pub use state::{StateRepo, SyncTypeName};
pub use store::Store;
