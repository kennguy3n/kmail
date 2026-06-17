// Pure-Rust core for the KMail cross-platform SDK.
//
// Layers (each module owns a single responsibility, no cycles):
//
//   models  — serde-typed domain objects shared by every layer.
//   jmap    — RFC 8620 + 8621 JMAP client speaking to the Go BFF.
//   sync    — SQLite-backed offline store + delta-pull engine.
//   crypto  — RustCrypto AEAD / KDF primitives + KeyStore trait.
//   push    — APNs / FCM / Web Push payload parsing + token wire.
//   notification — push payload → renderable LocalNotification.
//   cache   — Attachment LRU disk cache.
//   client  — `KMailClient` façade composed from the layers above.
//
// The public surface is intentionally narrow — `KMailClient` plus
// the serde types under `models` — so the UniFFI and napi
// wrappers can mirror it 1:1 without exposing reqwest, rusqlite,
// or tokio handles across the FFI boundary.

#![forbid(unsafe_code)]
#![warn(clippy::all)]
// Pedantic lints are NOT promoted to warnings — they trip on
// stylistic preferences (doc_markdown, duration_suboptimal_units,
// must_use_candidate, etc.) that would otherwise force the source
// into a non-idiomatic shape just to silence the linter. CI still
// promotes `clippy::all` warnings to errors via `-D warnings`.
#![allow(
    clippy::module_name_repetitions,
    clippy::missing_errors_doc,
    clippy::missing_panics_doc,
    clippy::must_use_candidate
)]

pub mod cache;
pub mod client;
pub mod crypto;
pub mod error;
pub mod jmap;
pub mod models;
pub mod notification;
pub mod push;
pub mod sync;

pub use client::{BackgroundSyncHandle, ClientConfig, KMailClient, PushIngestOutcome, SyncSummary};
pub use crypto::{
    AeadEnvelope, ConfidentialEnvelope, KeyMaterial, MlsKeyProvider, StaticMlsKeyProvider,
};
pub use error::{Error, Result};
pub use models::{
    Email, EmailAddress, EmailDraft, EmailSummary, JmapAccount, JmapSession, Mailbox, MailboxRole,
};
pub use notification::LocalNotification;
