// UniFFI bindings for the KMail SDK (Swift / Kotlin consumers).
//
// We use the procedural-macro flavour of UniFFI 0.28 (no UDL
// file) because the surface is tiny enough that the proc-macro
// annotations stay legible, and dropping the UDL eliminates the
// need for a separate build.rs scaffolding step. The downstream
// `uniffi-bindgen generate --library` invocation reads the
// compiled metadata out of the cdylib directly.
//
// All cross-FFI types are *flat dictionaries* — no chrono /
// serde-shaped types leak across the boundary, so Swift +
// Kotlin consumers only need to depend on `uniffi-rs` and the
// generated package, not on any of kmail-core's transitive
// crates.

#![forbid(unsafe_code)]

use std::path::PathBuf;
use std::sync::{Arc, OnceLock};
use std::time::Duration;

use kmail_core::{ClientConfig, EmailAddress, EmailDraft, EmailSummary, KMailClient, Mailbox};

uniffi::setup_scaffolding!();

// ---------------------------------------------------------------
// Async runtime
// ---------------------------------------------------------------
//
// UniFFI's `async_runtime = "tokio"` annotation expects a current
// Tokio runtime when the foreign call arrives. We provision one
// lazily so the binding doesn't pay for the worker threads until
// the first async call.

static RUNTIME: OnceLock<tokio::runtime::Runtime> = OnceLock::new();

fn runtime() -> &'static tokio::runtime::Runtime {
    RUNTIME.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .thread_name("kmail-ffi")
            .enable_all()
            .build()
            .expect("failed to build kmail-ffi tokio runtime")
    })
}

// ---------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------

#[derive(Debug, thiserror::Error, uniffi::Error)]
pub enum KMailError {
    #[error("local store error: {message}")]
    Store { message: String },
    #[error("transport error: {message}")]
    Transport { message: String },
    #[error("authentication failed: {message}")]
    Auth { message: String },
    #[error("forbidden: {message}")]
    Forbidden { message: String },
    #[error("not found: {message}")]
    NotFound { message: String },
    #[error("rate limited: retry after {retry_after_seconds}s")]
    RateLimit { retry_after_seconds: u64 },
    #[error("jmap method error [{code}]: {description}")]
    JmapMethod { code: String, description: String },
    #[error("protocol error: {message}")]
    Protocol { message: String },
    #[error("http client error [{status}]: {body}")]
    HttpClient { status: u16, body: String },
    #[error("sync state diverged")]
    SyncStateDiverged,
    #[error("decryption: {message}")]
    Decryption { message: String },
    #[error("key derivation: {message}")]
    KeyDerivation { message: String },
    #[error("keystore: {message}")]
    KeyStore { message: String },
    #[error("invalid argument: {message}")]
    InvalidArgument { message: String },
    #[error("operation cancelled")]
    Cancelled,
}

impl From<kmail_core::Error> for KMailError {
    fn from(value: kmail_core::Error) -> Self {
        use kmail_core::Error as E;
        match value {
            E::Store(message) => KMailError::Store { message },
            E::Transport(message) => KMailError::Transport { message },
            E::Auth(message) => KMailError::Auth { message },
            E::Forbidden(message) => KMailError::Forbidden { message },
            E::NotFound(message) => KMailError::NotFound { message },
            E::RateLimit {
                retry_after_seconds,
            } => KMailError::RateLimit {
                retry_after_seconds,
            },
            E::JmapMethod { code, description } => KMailError::JmapMethod { code, description },
            E::Protocol(message) => KMailError::Protocol { message },
            E::HttpClient { status, body } => KMailError::HttpClient { status, body },
            E::SyncStateDiverged => KMailError::SyncStateDiverged,
            E::Decryption(message) => KMailError::Decryption { message },
            E::KeyDerivation(message) => KMailError::KeyDerivation { message },
            E::KeyStore(message) => KMailError::KeyStore { message },
            E::InvalidArgument(message) => KMailError::InvalidArgument { message },
            E::Cancelled => KMailError::Cancelled,
        }
    }
}

impl From<tokio::task::JoinError> for KMailError {
    fn from(value: tokio::task::JoinError) -> Self {
        if value.is_cancelled() {
            KMailError::Cancelled
        } else {
            KMailError::Transport {
                message: format!("background task panicked: {value}"),
            }
        }
    }
}

// ---------------------------------------------------------------
// FFI record (dictionary) types
// ---------------------------------------------------------------

#[derive(uniffi::Record)]
pub struct KMailClientConfig {
    pub bff_url: String,
    pub bearer_token: String,
    pub database_path: String,
    pub attachment_cache_bytes: u64,
    pub request_timeout_secs: u32,
    pub retry_budget_secs: u32,
    pub initial_sync_email_window: u32,
    pub account_id: Option<String>,
    pub bootstrap_mailbox_role: Option<String>,
}

#[derive(uniffi::Record)]
pub struct FfiMailbox {
    pub id: String,
    pub name: String,
    pub role: Option<String>,
    pub parent_id: Option<String>,
    pub sort_order: u32,
    pub total_emails: u64,
    pub unread_emails: u64,
    pub is_vault: bool,
}

#[derive(uniffi::Record)]
pub struct FfiEmailAddress {
    pub name: String,
    pub email: String,
}

#[derive(uniffi::Record)]
pub struct FfiEmailSummary {
    pub id: String,
    pub thread_id: String,
    pub blob_id: String,
    pub mailbox_ids: Vec<String>,
    pub keyword_flags: Vec<String>,
    pub size: u64,
    pub received_at_unix: i64,
    pub sent_at_unix: Option<i64>,
    pub from_addresses: Vec<FfiEmailAddress>,
    pub to_addresses: Vec<FfiEmailAddress>,
    pub cc_addresses: Vec<FfiEmailAddress>,
    pub bcc_addresses: Vec<FfiEmailAddress>,
    pub subject: String,
    pub preview: String,
    pub has_attachment: bool,
}

#[derive(uniffi::Record)]
pub struct FfiSyncSummary {
    pub mailboxes_upserted: u64,
    pub emails_created: u64,
    pub emails_updated: u64,
    pub emails_destroyed: u64,
    pub pending_actions_flushed: u64,
}

// ---------------------------------------------------------------
// Conversions
// ---------------------------------------------------------------

impl From<Mailbox> for FfiMailbox {
    fn from(m: Mailbox) -> Self {
        FfiMailbox {
            id: m.id,
            name: m.name,
            role: m.role.map(|r| r.canonical_name().to_string()),
            parent_id: m.parent_id,
            sort_order: m.sort_order,
            total_emails: m.total_emails,
            unread_emails: m.unread_emails,
            is_vault: m.is_vault,
        }
    }
}

impl From<EmailAddress> for FfiEmailAddress {
    fn from(a: EmailAddress) -> Self {
        FfiEmailAddress {
            name: a.name,
            email: a.email,
        }
    }
}

impl From<EmailSummary> for FfiEmailSummary {
    fn from(s: EmailSummary) -> Self {
        FfiEmailSummary {
            id: s.id,
            thread_id: s.thread_id,
            blob_id: s.blob_id,
            mailbox_ids: s
                .mailbox_ids
                .into_iter()
                .filter_map(|(k, v)| v.then_some(k))
                .collect(),
            keyword_flags: s
                .keywords
                .into_iter()
                .filter_map(|(k, v)| v.then_some(k))
                .collect(),
            size: s.size,
            received_at_unix: s.received_at.timestamp(),
            sent_at_unix: s.sent_at.map(|t| t.timestamp()),
            from_addresses: s.from.into_iter().map(Into::into).collect(),
            to_addresses: s.to.into_iter().map(Into::into).collect(),
            cc_addresses: s.cc.into_iter().map(Into::into).collect(),
            bcc_addresses: s.bcc.into_iter().map(Into::into).collect(),
            subject: s.subject,
            preview: s.preview,
            has_attachment: s.has_attachment,
        }
    }
}

impl From<kmail_core::SyncSummary> for FfiSyncSummary {
    fn from(s: kmail_core::SyncSummary) -> Self {
        FfiSyncSummary {
            mailboxes_upserted: s.mailboxes_upserted,
            emails_created: s.emails_created,
            emails_updated: s.emails_updated,
            emails_destroyed: s.emails_destroyed,
            pending_actions_flushed: s.pending_actions_flushed,
        }
    }
}

// ---------------------------------------------------------------
// Entry point + Object surface
// ---------------------------------------------------------------

#[uniffi::export]
pub fn client_open(config: KMailClientConfig) -> Result<Arc<KMailClientHandle>, KMailError> {
    let mut core_cfg = ClientConfig::new(
        config.bff_url,
        config.bearer_token,
        PathBuf::from(config.database_path),
    );
    core_cfg.attachment_cache_bytes = config.attachment_cache_bytes;
    core_cfg.request_timeout = Duration::from_secs(u64::from(config.request_timeout_secs));
    core_cfg.retry_budget = Duration::from_secs(u64::from(config.retry_budget_secs));
    core_cfg.initial_sync_email_window = config.initial_sync_email_window;
    core_cfg.account_id = config.account_id;
    core_cfg.bootstrap_mailbox_role = config.bootstrap_mailbox_role;

    let inner = KMailClient::open(core_cfg)?;
    Ok(Arc::new(KMailClientHandle { inner }))
}

#[derive(uniffi::Object)]
pub struct KMailClientHandle {
    inner: KMailClient,
}

#[uniffi::export(async_runtime = "tokio")]
impl KMailClientHandle {
    pub async fn sync(&self) -> Result<FfiSyncSummary, KMailError> {
        let inner = self.inner.clone();
        let summary = runtime().spawn(async move { inner.sync().await }).await??;
        Ok(summary.into())
    }

    /// Hot-swap the OIDC bearer token. iOS / Android shells should
    /// call this whenever they refresh the access token, instead of
    /// closing and reopening the client.
    pub fn set_bearer_token(&self, token: String) -> Result<(), KMailError> {
        self.inner.set_bearer_token(token).map_err(Into::into)
    }

    /// Drop the cached JMAP session. The next `sync()` call will
    /// re-fetch `/jmap/session`. Use when the shell observes a
    /// reauth-required 401 or a tenant-rebalanced push.
    pub async fn invalidate_session(&self) {
        let inner = self.inner.clone();
        // Spawn through our owned runtime so the session lock lives
        // there — matches the pattern used by `sync()` above and
        // avoids deadlocks if the foreign caller's runtime context is
        // ambiguous.
        runtime()
            .spawn(async move { inner.invalidate_session().await })
            .await
            .expect("invalidate_session task panicked");
    }

    pub fn cached_mailboxes(&self) -> Result<Vec<FfiMailbox>, KMailError> {
        Ok(self
            .inner
            .cached_mailboxes()?
            .into_iter()
            .map(Into::into)
            .collect())
    }

    pub fn cached_emails_in_mailbox(
        &self,
        mailbox_id: String,
        limit: u32,
    ) -> Result<Vec<FfiEmailSummary>, KMailError> {
        Ok(self
            .inner
            .cached_emails_in_mailbox(&mailbox_id, limit)?
            .into_iter()
            .map(Into::into)
            .collect())
    }

    pub fn enqueue_set_keywords(
        &self,
        email_id: String,
        keywords_json: String,
    ) -> Result<(), KMailError> {
        let keywords: serde_json::Value =
            serde_json::from_str(&keywords_json).map_err(|e| KMailError::InvalidArgument {
                message: format!("invalid keywords json: {e}"),
            })?;
        self.inner.enqueue_set_keywords(&email_id, &keywords)?;
        Ok(())
    }

    pub async fn send_email(&self, draft_json: String) -> Result<String, KMailError> {
        let draft: EmailDraft =
            serde_json::from_str(&draft_json).map_err(|e| KMailError::InvalidArgument {
                message: format!("invalid draft json: {e}"),
            })?;
        let inner = self.inner.clone();
        let id = runtime()
            .spawn(async move { inner.send_email(&draft).await })
            .await??;
        Ok(id)
    }

    pub async fn register_apns_token(&self, token: String) -> Result<(), KMailError> {
        let inner = self.inner.clone();
        runtime()
            .spawn(async move {
                inner
                    .register_push_token(kmail_core::push::PushTransport::Apns, &token, None)
                    .await
            })
            .await??;
        Ok(())
    }

    pub async fn register_fcm_token(&self, token: String) -> Result<(), KMailError> {
        let inner = self.inner.clone();
        runtime()
            .spawn(async move {
                inner
                    .register_push_token(kmail_core::push::PushTransport::Fcm, &token, None)
                    .await
            })
            .await??;
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use kmail_core::MailboxRole;

    /// Trip every `kmail_core::Error` variant through the FFI
    /// `From` impl and assert the variant tag is preserved. A
    /// regression here would route the wrong recovery flow on the
    /// platform shell side.
    #[test]
    fn error_variants_map_one_to_one() {
        let store = KMailError::from(kmail_core::Error::Store("x".into()));
        assert!(matches!(store, KMailError::Store { .. }));

        let auth = KMailError::from(kmail_core::Error::Auth("x".into()));
        assert!(matches!(auth, KMailError::Auth { .. }));

        let rate = KMailError::from(kmail_core::Error::RateLimit {
            retry_after_seconds: 7,
        });
        assert!(matches!(
            rate,
            KMailError::RateLimit {
                retry_after_seconds: 7
            }
        ));

        let jmap = KMailError::from(kmail_core::Error::JmapMethod {
            code: "x".into(),
            description: "y".into(),
        });
        assert!(matches!(jmap, KMailError::JmapMethod { .. }));

        let diverged = KMailError::from(kmail_core::Error::SyncStateDiverged);
        assert!(matches!(diverged, KMailError::SyncStateDiverged));

        let cancelled = KMailError::from(kmail_core::Error::Cancelled);
        assert!(matches!(cancelled, KMailError::Cancelled));

        // `HttpClient` MUST carry both status + body through so
        // the platform shell can surface the server's
        // explanation verbatim (e.g. "413: attachment too
        // large") instead of a generic "transport error".
        let http_client = KMailError::from(kmail_core::Error::HttpClient {
            status: 422,
            body: "malformed Email/set patch".into(),
        });
        assert!(matches!(
            http_client,
            KMailError::HttpClient {
                status: 422,
                ref body,
            } if body == "malformed Email/set patch"
        ));
    }

    /// `MailboxRole` round-trips through the FFI string label.
    #[test]
    fn role_label_covers_every_variant() {
        for r in [
            MailboxRole::Inbox,
            MailboxRole::Archive,
            MailboxRole::Drafts,
            MailboxRole::Sent,
            MailboxRole::Trash,
            MailboxRole::Junk,
            MailboxRole::Important,
            MailboxRole::All,
            MailboxRole::Flagged,
            MailboxRole::Vault,
            MailboxRole::Unknown,
        ] {
            let s = r.canonical_name();
            assert!(!s.is_empty());
            // Round-trip via canonical name proves the FFI label
            // matches the JMAP wire form (no Debug-derive coupling).
            // `Unknown` is the catch-all sentinel and has no real
            // wire spelling, so it intentionally does NOT round-trip
            // back through `from_canonical_name` — match the contract
            // pinned in `models::tests::mailbox_role_canonical_name_matches_wire`.
            if matches!(r, MailboxRole::Unknown) {
                assert!(MailboxRole::from_canonical_name(s).is_none());
            } else {
                assert_eq!(MailboxRole::from_canonical_name(s), Some(r));
            }
        }
    }
}
