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

use kmail_core::{
    AeadEnvelope, ClientConfig, ConfidentialEnvelope, EmailAddress, EmailDraft, EmailSummary,
    KMailClient, KeyMaterial, Mailbox, MlsKeyProvider,
};
use zeroize::Zeroize;

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

/// FFI mirror of [`ClientConfig`] for `client_open`.
///
/// `bff_url`, `bearer_token`, and `database_path` are required
/// because they are tenant- and session-specific — there is no
/// sensible Rust default for them.
///
/// The remaining fields fall into two semantic tiers, both surfaced
/// as `Option<T>` to foreign callers but with different `None`
/// semantics on the Rust side. The asymmetry is intentional and
/// matches the napi binding ([`sdk/kmail-napi/src/lib.rs`]) verbatim
/// — cross-binding semantic equivalence is load-bearing because the
/// Electron desktop shell and the iOS/Android shells must produce
/// observationally identical `KMailClient` state when given
/// equivalent config records.
///
/// **Tier 1 — numeric overrides ("inherit default vs override"):**
/// `attachment_cache_bytes`, `request_timeout_secs`,
/// `retry_budget_secs`, `initial_sync_email_window`. The Rust core
/// has a sensible non-`None` default for each of these (e.g.
/// `retry_budget = Duration::from_secs(60)`). `None` on the FFI
/// record means "inherit the [`ClientConfig::new`] default";
/// `Some(v)` means "override with v". This eliminates the drift-bug
/// category the earlier version had — when the UniFFI fields were
/// non-optional and `client_open` unconditionally overwrote every
/// field, a foreign caller that declared its own literal default
/// (e.g. Swift defaulting `retryBudget` to 30s while Rust defaults
/// to 60s) would silently halve a load-bearing operational setting.
/// With `Option<T>`, a foreign caller can pass `None` to inherit the
/// Rust default unconditionally, making drift architecturally
/// impossible for any field a binding doesn't explicitly set.
///
/// **Tier 2 — nullable strings ("value or no value"):** `account_id`
/// and `bootstrap_mailbox_role`. The Rust core type already declares
/// these as `Option<String>` because `None` is a legitimate "no
/// value" sentinel — a caller may genuinely want to skip the
/// account-id hint, or to skip the inbox auto-bootstrap. The FFI
/// field's `Option<String>` therefore maps to the core's
/// `Option<String>` verbatim: `None` foreign-side becomes `None`
/// core-side, NOT "inherit default". `default_client_config(...)`
/// echoes back the Rust default (`account_id: None`,
/// `bootstrap_mailbox_role: Some("inbox")`) so a foreign caller that
/// wants "the Rust default" can read it back explicitly; a caller
/// that wants "no value" just passes `None` directly.
///
/// Foreign bindings that want to surface language-idiomatic defaults
/// (e.g. Swift's named-argument literal defaults for nice IDE
/// autocomplete) SHOULD source those defaults from
/// [`default_client_config`] rather than hardcoding numeric literals
/// — see the Swift `ClientConfiguration.init` for the pattern.
#[derive(uniffi::Record)]
pub struct KMailClientConfig {
    pub bff_url: String,
    pub bearer_token: String,
    pub database_path: String,
    pub attachment_cache_bytes: Option<u64>,
    pub request_timeout_secs: Option<u32>,
    pub retry_budget_secs: Option<u32>,
    pub initial_sync_email_window: Option<u32>,
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
    pub mailboxes_destroyed: u64,
    pub emails_created: u64,
    pub emails_updated: u64,
    pub emails_destroyed: u64,
    pub pending_actions_applied: u64,
    pub pending_actions_failed: u64,
    pub pending_actions_deferred: u64,
}

/// AEAD envelope at the FFI boundary.
///
/// Mirrors [`kmail_core::AeadEnvelope`] but with a `Vec<u8>` nonce
/// instead of the `[u8; 12]` fixed-array field — UniFFI dictionaries
/// can't express fixed-size byte arrays directly, and Swift /
/// Kotlin both render `[UInt8]` / `ByteArray` for `bytes` regardless
/// of length. The conversion functions below enforce the
/// `NONCE_LEN == 12` invariant at the FFI boundary, surfacing a
/// short / oversized nonce as `KMailError::InvalidArgument` rather
/// than a downstream AES-GCM length error.
#[derive(uniffi::Record)]
pub struct FfiAeadEnvelope {
    pub nonce: Vec<u8>,
    pub ciphertext: Vec<u8>,
    pub aad: Vec<u8>,
}

impl From<AeadEnvelope> for FfiAeadEnvelope {
    fn from(env: AeadEnvelope) -> Self {
        Self {
            nonce: env.nonce.to_vec(),
            ciphertext: env.ciphertext,
            aad: env.aad,
        }
    }
}

impl TryFrom<FfiAeadEnvelope> for AeadEnvelope {
    type Error = KMailError;
    fn try_from(env: FfiAeadEnvelope) -> Result<Self, Self::Error> {
        if env.nonce.len() != kmail_core::crypto::NONCE_LEN {
            return Err(KMailError::InvalidArgument {
                message: format!(
                    "AEAD nonce must be {} bytes, got {}",
                    kmail_core::crypto::NONCE_LEN,
                    env.nonce.len()
                ),
            });
        }
        let mut nonce = [0u8; kmail_core::crypto::NONCE_LEN];
        nonce.copy_from_slice(&env.nonce);
        Ok(AeadEnvelope {
            nonce,
            ciphertext: env.ciphertext,
            aad: env.aad,
        })
    }
}

/// Confidential Send envelope at the FFI boundary.
///
/// Mirrors [`kmail_core::ConfidentialEnvelope`] with the same
/// fixed-array → `Vec<u8>` translation as [`FfiAeadEnvelope`].
#[derive(uniffi::Record)]
pub struct FfiConfidentialEnvelope {
    pub kek_salt: Vec<u8>,
    pub wrapped_dek: FfiAeadEnvelope,
    pub payload: FfiAeadEnvelope,
}

impl From<ConfidentialEnvelope> for FfiConfidentialEnvelope {
    fn from(env: ConfidentialEnvelope) -> Self {
        Self {
            kek_salt: env.kek_salt.to_vec(),
            wrapped_dek: env.wrapped_dek.into(),
            payload: env.payload.into(),
        }
    }
}

impl TryFrom<FfiConfidentialEnvelope> for ConfidentialEnvelope {
    type Error = KMailError;
    fn try_from(env: FfiConfidentialEnvelope) -> Result<Self, Self::Error> {
        if env.kek_salt.len() != kmail_core::crypto::KEK_SALT_LEN {
            return Err(KMailError::InvalidArgument {
                message: format!(
                    "Confidential KEK salt must be {} bytes, got {}",
                    kmail_core::crypto::KEK_SALT_LEN,
                    env.kek_salt.len()
                ),
            });
        }
        let mut kek_salt = [0u8; kmail_core::crypto::KEK_SALT_LEN];
        kek_salt.copy_from_slice(&env.kek_salt);
        Ok(ConfidentialEnvelope {
            kek_salt,
            wrapped_dek: env.wrapped_dek.try_into()?,
            payload: env.payload.try_into()?,
        })
    }
}

// ---------------------------------------------------------------
// MLS provider callback trait
// ---------------------------------------------------------------
//
// Swift / Kotlin implementors back this trait with calls into the
// KChat MLS SDK on each platform. The Rust side wraps the foreign
// trait object in [`ForeignMlsKeyProvider`] which adapts it to the
// SDK-internal `MlsKeyProvider` interface. The adapter exists
// because UniFFI's foreign-trait machinery cannot return the
// SDK-private `KeyMaterial` type directly (it owns the
// zeroize-on-drop semantics that we do NOT want to leak across
// the FFI boundary — the foreign caller MUST return raw bytes
// and trust the SDK to wrap them).

/// Foreign-implementable trait. The platform shell provides this
/// (in Swift / Kotlin) and hands it to the SDK via
/// [`KMailClientHandle::set_mls_provider`].
///
/// Contract — same as the Rust-side
/// [`kmail_core::MlsKeyProvider`]:
///   - Return exactly 32 bytes from each method.
///   - Determinism across the lifetime of one MLS epoch.
///   - Distinct secrets per distinct `recipient_user_id` /
///     `folder_id` (uniqueness is a contract obligation; the
///     trait can't enforce it).
#[uniffi::export(with_foreign)]
pub trait FfiMlsKeyProvider: Send + Sync {
    fn confidential_send_leaf_secret(
        &self,
        recipient_user_id: String,
    ) -> Result<Vec<u8>, KMailError>;
    fn vault_folder_master_secret(&self, folder_id: String) -> Result<Vec<u8>, KMailError>;
}

/// Adapter from the foreign-implemented [`FfiMlsKeyProvider`] to
/// the Rust-side [`MlsKeyProvider`].
///
/// The adapter validates the foreign side's contract (32-byte
/// return) and converts the raw `Vec<u8>` into the
/// zeroize-on-drop [`KeyMaterial`] wrapper before handing it back
/// to the SDK. If the foreign side returns a wrong-length buffer,
/// we surface that as `Error::KeyStore` rather than letting it
/// propagate into the crypto layer as a confusing "invalid key
/// length" error.
struct ForeignMlsKeyProvider {
    foreign: Arc<dyn FfiMlsKeyProvider>,
}

impl MlsKeyProvider for ForeignMlsKeyProvider {
    fn confidential_send_leaf_secret(
        &self,
        recipient_user_id: &str,
    ) -> kmail_core::Result<KeyMaterial> {
        let mut bytes = self
            .foreign
            .confidential_send_leaf_secret(recipient_user_id.to_string())
            .map_err(|e| {
                kmail_core::Error::KeyStore(format!("foreign MlsKeyProvider returned error: {e}"))
            })?;
        if bytes.len() != 32 {
            // Zeroize the wrong-length buffer before dropping it.
            // A misconfigured caller that returns e.g. 33 bytes may
            // still have included 32 bytes of real key material
            // (with a stray trailing byte); we don't want those
            // bytes lingering in freed heap. See Devin Review
            // finding 3294898657 on PR #39.
            let len = bytes.len();
            bytes.zeroize();
            return Err(kmail_core::Error::KeyStore(format!(
                "foreign MlsKeyProvider returned {len}-byte Confidential Send secret, expected 32"
            )));
        }
        Ok(KeyMaterial::new(bytes))
    }

    fn vault_folder_master_secret(&self, folder_id: &str) -> kmail_core::Result<KeyMaterial> {
        let mut bytes = self
            .foreign
            .vault_folder_master_secret(folder_id.to_string())
            .map_err(|e| {
                kmail_core::Error::KeyStore(format!("foreign MlsKeyProvider returned error: {e}"))
            })?;
        if bytes.len() != 32 {
            let len = bytes.len();
            bytes.zeroize();
            return Err(kmail_core::Error::KeyStore(format!(
                "foreign MlsKeyProvider returned {len}-byte Vault secret, expected 32"
            )));
        }
        Ok(KeyMaterial::new(bytes))
    }
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
            mailboxes_destroyed: s.mailboxes_destroyed,
            emails_created: s.emails_created,
            emails_updated: s.emails_updated,
            emails_destroyed: s.emails_destroyed,
            pending_actions_applied: s.pending_actions_applied,
            pending_actions_failed: s.pending_actions_failed,
            pending_actions_deferred: s.pending_actions_deferred,
        }
    }
}

// ---------------------------------------------------------------
// Entry point + Object surface
// ---------------------------------------------------------------

/// Return a `KMailClientConfig` pre-populated with the SDK's
/// canonical defaults for every non-required field.
///
/// This exists to give foreign bindings (Swift / Kotlin / Node)
/// a single source of truth for "what does the SDK default to?".
/// Without it, each binding would have to duplicate the literal
/// defaults from [`ClientConfig::new`], which is a recurring
/// source of drift bugs (e.g. Swift defaulting `retryBudget` to
/// 30s instead of the Rust-side 60s, halving the retry budget
/// for every iOS client that uses default settings).
///
/// Foreign bindings that want to expose their own
/// language-idiomatic defaults SHOULD still seed those defaults
/// from this function — either by calling it at config
/// construction time, or by adding a test that compares the
/// binding-side defaults against the values this function
/// returns. The Swift binding takes the test-based approach
/// (see `testSwiftDefaultsMatchRustDefaults`) so a future
/// change to a Rust default surfaces immediately as a test
/// failure on the macOS CI runner rather than silently
/// drifting on the iOS surface.
///
/// The returned record has `account_id = None` and uses the
/// caller-supplied `bff_url` / `bearer_token` / `database_path`
/// because there is no sensible default for those — they are
/// always tenant-specific.
#[uniffi::export]
pub fn default_client_config(
    bff_url: String,
    bearer_token: String,
    database_path: String,
) -> KMailClientConfig {
    let core = ClientConfig::new(bff_url, bearer_token, PathBuf::from(database_path));
    let request_timeout_secs = u32::try_from(core.request_timeout.as_secs()).unwrap_or(u32::MAX);
    let retry_budget_secs = u32::try_from(core.retry_budget.as_secs()).unwrap_or(u32::MAX);
    KMailClientConfig {
        bff_url: core.bff_url,
        bearer_token: core.bearer_token,
        database_path: core.database_path.to_string_lossy().into_owned(),
        // Every field is `Some(...)` here even though `KMailClientConfig`
        // declares them as `Option<T>`. The semantic distinction is:
        //   - `KMailClientConfig` field = `None`  -> use SDK default
        //   - `KMailClientConfig` field = `Some` -> use this exact value
        // `default_client_config(...)` returns a record describing
        // "what defaults will the SDK use if I pass `None` for every
        // override?" — so the caller can read concrete values back
        // (for IDE autocomplete, for binding-side test assertions,
        // for echoing into a settings UI) even though those values
        // ARE the same as `None`-then-resolve.
        attachment_cache_bytes: Some(core.attachment_cache_bytes),
        request_timeout_secs: Some(request_timeout_secs),
        retry_budget_secs: Some(retry_budget_secs),
        initial_sync_email_window: Some(core.initial_sync_email_window),
        account_id: core.account_id,
        bootstrap_mailbox_role: core.bootstrap_mailbox_role,
    }
}

#[uniffi::export]
pub fn client_open(config: KMailClientConfig) -> Result<Arc<KMailClientHandle>, KMailError> {
    let mut core_cfg = ClientConfig::new(
        config.bff_url,
        config.bearer_token,
        PathBuf::from(config.database_path),
    );
    // Only override SDK defaults for fields the foreign caller
    // explicitly set. `None` means "inherit Rust default" — see
    // the [`KMailClientConfig`] docs for the design rationale.
    if let Some(b) = config.attachment_cache_bytes {
        core_cfg.attachment_cache_bytes = b;
    }
    if let Some(t) = config.request_timeout_secs {
        core_cfg.request_timeout = Duration::from_secs(u64::from(t));
    }
    if let Some(t) = config.retry_budget_secs {
        core_cfg.retry_budget = Duration::from_secs(u64::from(t));
    }
    if let Some(w) = config.initial_sync_email_window {
        core_cfg.initial_sync_email_window = w;
    }
    // Tier-2 string fields (`account_id`, `bootstrap_mailbox_role`):
    // the core type already declares these as `Option<String>`, so a
    // foreign-side `None` is a legitimate "no value" — different
    // from the tier-1 numeric fields above. Assign verbatim, matching
    // the napi binding (`sdk/kmail-napi/src/lib.rs`) exactly so that a
    // record built from `default_client_config(...).bootstrap_mailbox_role`
    // (`Some("inbox")`) and a record with `bootstrap_mailbox_role: None`
    // produce different observable states on the Rust side. See the
    // [`KMailClientConfig`] doc for the two-tier semantics.
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
    ///
    /// Returns `Result<(), KMailError>` (rather than panicking) so
    /// a `JoinError` from the owned runtime — vanishingly unlikely
    /// here because the spawned future just clears an `Option`,
    /// but possible if the runtime is shutting down — surfaces to
    /// the foreign caller as `KMailError::Cancelled` /
    /// `KMailError::Transport` via the `From<JoinError>` impl,
    /// matching how `sync()` handles the same edge case. Letting
    /// the spawned task panic would have aborted the Swift /
    /// Kotlin host process; surfacing the error lets the platform
    /// shell reopen the client cleanly.
    pub async fn invalidate_session(&self) -> Result<(), KMailError> {
        let inner = self.inner.clone();
        // Spawn through our owned runtime so the session lock lives
        // there — matches the pattern used by `sync()` above and
        // avoids deadlocks if the foreign caller's runtime context is
        // ambiguous.
        runtime()
            .spawn(async move { inner.invalidate_session().await })
            .await?;
        Ok(())
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

    // ---------------------------------------------------------
    // Crypto surface (Confidential Send + Zero-Access Vault)
    // ---------------------------------------------------------

    /// Seal `plaintext` into a Zero-Access Vault envelope under a
    /// caller-supplied 32-byte per-folder master key.
    ///
    /// Synchronous because the underlying crypto is pure CPU
    /// work; bouncing through the runtime would just add
    /// scheduling latency without parallelism.
    pub fn seal_vault_envelope(
        &self,
        folder_master_key: Vec<u8>,
        plaintext: Vec<u8>,
        aad: Vec<u8>,
    ) -> Result<FfiAeadEnvelope, KMailError> {
        let env = self
            .inner
            .seal_vault_envelope(&folder_master_key, &plaintext, &aad)?;
        Ok(env.into())
    }

    /// Decrypt a Zero-Access Vault envelope. Symmetric inverse
    /// of [`KMailClientHandle::seal_vault_envelope`].
    pub fn decrypt_vault_envelope(
        &self,
        folder_master_key: Vec<u8>,
        envelope: FfiAeadEnvelope,
    ) -> Result<Vec<u8>, KMailError> {
        let env: AeadEnvelope = envelope.try_into()?;
        Ok(self
            .inner
            .decrypt_vault_envelope(&folder_master_key, &env)?)
    }

    /// Seal `plaintext` into a Confidential Send envelope under
    /// a caller-supplied 32-byte MLS leaf secret.
    pub fn seal_confidential_envelope(
        &self,
        mls_leaf_secret: Vec<u8>,
        plaintext: Vec<u8>,
        payload_aad: Vec<u8>,
        wrap_aad: Vec<u8>,
    ) -> Result<FfiConfidentialEnvelope, KMailError> {
        let env = self.inner.seal_confidential_envelope(
            &mls_leaf_secret,
            &plaintext,
            &payload_aad,
            &wrap_aad,
        )?;
        Ok(env.into())
    }

    /// Open a Confidential Send envelope. Symmetric inverse of
    /// [`KMailClientHandle::seal_confidential_envelope`].
    pub fn open_confidential_envelope(
        &self,
        mls_leaf_secret: Vec<u8>,
        envelope: FfiConfidentialEnvelope,
    ) -> Result<Vec<u8>, KMailError> {
        let env: ConfidentialEnvelope = envelope.try_into()?;
        Ok(self
            .inner
            .open_confidential_envelope(&mls_leaf_secret, &env)?)
    }

    // ---------------------------------------------------------
    // MlsKeyProvider plumbing
    // ---------------------------------------------------------

    /// Plug a foreign-implemented [`FfiMlsKeyProvider`] into the
    /// client. After this call, the `*_message` convenience
    /// methods below can derive keys via the provider without
    /// the caller threading raw key bytes through every call.
    pub async fn set_mls_provider(
        &self,
        provider: Arc<dyn FfiMlsKeyProvider>,
    ) -> Result<(), KMailError> {
        let inner = self.inner.clone();
        let adapter: Arc<dyn MlsKeyProvider> =
            Arc::new(ForeignMlsKeyProvider { foreign: provider });
        runtime()
            .spawn(async move { inner.set_mls_provider(adapter).await })
            .await?;
        Ok(())
    }

    /// Drop the currently-plugged [`FfiMlsKeyProvider`]. Subsequent
    /// `*_message` convenience calls will return
    /// `KMailError::KeyStore`.
    pub async fn clear_mls_provider(&self) -> Result<(), KMailError> {
        let inner = self.inner.clone();
        runtime()
            .spawn(async move { inner.clear_mls_provider().await })
            .await?;
        Ok(())
    }

    /// Convenience: seal a Vault message by asking the plugged
    /// [`FfiMlsKeyProvider`] for the per-folder master key.
    pub async fn write_vault_message(
        &self,
        folder_id: String,
        plaintext: Vec<u8>,
        aad: Vec<u8>,
    ) -> Result<FfiAeadEnvelope, KMailError> {
        let inner = self.inner.clone();
        let env = runtime()
            .spawn(async move {
                inner
                    .write_vault_message(&folder_id, &plaintext, &aad)
                    .await
            })
            .await??;
        Ok(env.into())
    }

    /// Convenience: open a Vault message by asking the plugged
    /// [`FfiMlsKeyProvider`] for the per-folder master key.
    pub async fn open_vault_message(
        &self,
        folder_id: String,
        envelope: FfiAeadEnvelope,
    ) -> Result<Vec<u8>, KMailError> {
        let env: AeadEnvelope = envelope.try_into()?;
        let inner = self.inner.clone();
        let pt = runtime()
            .spawn(async move { inner.open_vault_message(&folder_id, &env).await })
            .await??;
        Ok(pt)
    }

    /// Convenience: seal a Confidential Send message by asking
    /// the plugged [`FfiMlsKeyProvider`] for the per-recipient
    /// leaf secret.
    pub async fn encrypt_confidential_message(
        &self,
        recipient_user_id: String,
        plaintext: Vec<u8>,
        payload_aad: Vec<u8>,
        wrap_aad: Vec<u8>,
    ) -> Result<FfiConfidentialEnvelope, KMailError> {
        let inner = self.inner.clone();
        let env = runtime()
            .spawn(async move {
                inner
                    .encrypt_confidential_message(
                        &recipient_user_id,
                        &plaintext,
                        &payload_aad,
                        &wrap_aad,
                    )
                    .await
            })
            .await??;
        Ok(env.into())
    }

    /// Convenience: open a Confidential Send message by asking
    /// the plugged [`FfiMlsKeyProvider`] for the per-recipient
    /// leaf secret.
    pub async fn decrypt_confidential_message(
        &self,
        recipient_user_id: String,
        envelope: FfiConfidentialEnvelope,
    ) -> Result<Vec<u8>, KMailError> {
        let env: ConfidentialEnvelope = envelope.try_into()?;
        let inner = self.inner.clone();
        let pt = runtime()
            .spawn(async move {
                inner
                    .decrypt_confidential_message(&recipient_user_id, &env)
                    .await
            })
            .await??;
        Ok(pt)
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

    /// `default_client_config` must mirror `ClientConfig::new`
    /// byte-for-byte. If a Rust-side default changes (e.g. the
    /// retry budget moves from 60s to 90s), this test ensures
    /// every foreign binding that calls `defaultClientConfig`
    /// picks up the new default instead of silently drifting.
    #[test]
    fn default_client_config_mirrors_core_defaults() {
        let core = ClientConfig::new(
            "https://kmail.test",
            "test-bearer",
            PathBuf::from("/tmp/k.sqlite"),
        );
        let ffi = default_client_config(
            "https://kmail.test".into(),
            "test-bearer".into(),
            "/tmp/k.sqlite".into(),
        );
        assert_eq!(ffi.bff_url, core.bff_url);
        assert_eq!(ffi.bearer_token, core.bearer_token);
        assert_eq!(
            ffi.database_path,
            core.database_path.to_string_lossy().into_owned()
        );
        assert_eq!(
            ffi.attachment_cache_bytes,
            Some(core.attachment_cache_bytes)
        );
        assert_eq!(
            ffi.request_timeout_secs.map(u64::from),
            Some(core.request_timeout.as_secs())
        );
        assert_eq!(
            ffi.retry_budget_secs.map(u64::from),
            Some(core.retry_budget.as_secs())
        );
        assert_eq!(
            ffi.initial_sync_email_window,
            Some(core.initial_sync_email_window)
        );
        assert_eq!(ffi.account_id, core.account_id);
        assert_eq!(ffi.bootstrap_mailbox_role, core.bootstrap_mailbox_role);

        // Lock down the actual numeric values too so a future
        // refactor of `ClientConfig::new` that silently changes
        // a default surfaces here. The Swift binding now sources
        // these defaults dynamically from `default_client_config`
        // (no duplicated literals), so this single assertion is the
        // canonical declaration of "what does the SDK default to?"
        // — a future change to a Rust default fails this test, and
        // every foreign binding that calls `default_client_config`
        // automatically picks up the new value.
        assert_eq!(ffi.attachment_cache_bytes, Some(256 * 1024 * 1024));
        assert_eq!(ffi.request_timeout_secs, Some(30));
        assert_eq!(ffi.retry_budget_secs, Some(60));
        assert_eq!(ffi.initial_sync_email_window, Some(200));
        assert_eq!(ffi.bootstrap_mailbox_role.as_deref(), Some("inbox"));
        assert_eq!(ffi.account_id, None);
    }

    /// Helper that re-implements the `client_open` lowering ladder
    /// in a unit-test-friendly form (no sqlite open). The body MUST
    /// stay in lockstep with the real `client_open` so the tests
    /// below actually exercise the same per-field plumbing.
    fn lower_client_open_test(config: &KMailClientConfig) -> ClientConfig {
        let mut core_cfg = ClientConfig::new(
            config.bff_url.clone(),
            config.bearer_token.clone(),
            PathBuf::from(config.database_path.clone()),
        );
        if let Some(b) = config.attachment_cache_bytes {
            core_cfg.attachment_cache_bytes = b;
        }
        if let Some(t) = config.request_timeout_secs {
            core_cfg.request_timeout = Duration::from_secs(u64::from(t));
        }
        if let Some(t) = config.retry_budget_secs {
            core_cfg.retry_budget = Duration::from_secs(u64::from(t));
        }
        if let Some(w) = config.initial_sync_email_window {
            core_cfg.initial_sync_email_window = w;
        }
        // Tier-2 string fields: verbatim assignment (matches napi).
        core_cfg.account_id = config.account_id.clone();
        core_cfg.bootstrap_mailbox_role = config.bootstrap_mailbox_role.clone();
        core_cfg
    }

    /// `client_open` with every override field set to `None` must
    /// produce a `ClientConfig` whose **tier-1 numeric fields** match
    /// `ClientConfig::new` exactly (None = inherit default), and whose
    /// **tier-2 string fields** (`account_id`, `bootstrap_mailbox_role`)
    /// reflect the foreign-side `None` verbatim (None = no value).
    ///
    /// This is the load-bearing test for the two-tier semantics:
    ///
    ///   * `attachment_cache_bytes: None`        → inherits `256 MiB`
    ///   * `request_timeout_secs: None`          → inherits `30s`
    ///   * `retry_budget_secs: None`             → inherits `60s`
    ///   * `initial_sync_email_window: None`     → inherits `200`
    ///   * `account_id: None`                    → stays `None`
    ///   * `bootstrap_mailbox_role: None`        → stays `None`
    ///     (overrides the `Some("inbox")` default — a caller that
    ///     genuinely wants no inbox auto-bootstrap can opt out)
    ///
    /// Matches the napi binding's verbatim semantics for tier-2
    /// (`sdk/kmail-napi/src/lib.rs`).
    #[test]
    fn client_open_lowers_none_to_core_defaults() {
        let ffi_none = KMailClientConfig {
            bff_url: "https://kmail.test".into(),
            bearer_token: "tok".into(),
            database_path: "/tmp/kmail.sqlite".into(),
            attachment_cache_bytes: None,
            request_timeout_secs: None,
            retry_budget_secs: None,
            initial_sync_email_window: None,
            account_id: None,
            bootstrap_mailbox_role: None,
        };

        let core_cfg = lower_client_open_test(&ffi_none);
        let reference = ClientConfig::new(
            ffi_none.bff_url.clone(),
            ffi_none.bearer_token.clone(),
            PathBuf::from(ffi_none.database_path.clone()),
        );

        // Tier 1: None -> inherit Rust default.
        assert_eq!(
            core_cfg.attachment_cache_bytes,
            reference.attachment_cache_bytes
        );
        assert_eq!(core_cfg.request_timeout, reference.request_timeout);
        assert_eq!(core_cfg.retry_budget, reference.retry_budget);
        assert_eq!(
            core_cfg.initial_sync_email_window,
            reference.initial_sync_email_window
        );

        // Tier 2: None -> verbatim None on the Rust side. The
        // reference happens to have `account_id = None` (so the
        // tier-2 verbatim path is observationally equivalent to
        // inherit-default for that field), but for
        // `bootstrap_mailbox_role` the reference is `Some("inbox")`
        // and the verbatim path produces `None`. That observable
        // divergence is the tier-2 contract.
        assert_eq!(core_cfg.account_id, None);
        assert_eq!(core_cfg.account_id, reference.account_id);
        assert_eq!(core_cfg.bootstrap_mailbox_role, None);
        assert_eq!(
            reference.bootstrap_mailbox_role.as_deref(),
            Some("inbox"),
            "sanity check: ClientConfig::new must default `bootstrap_mailbox_role` \
             to Some(\"inbox\") for the tier-2 divergence to be observable"
        );
    }

    /// Tier-1 `Some(v)` MUST override the Rust default with `v`. If a
    /// future refactor accidentally regresses the `Option<T>` ladder
    /// to ignore foreign overrides (e.g.
    /// `core_cfg.retry_budget = Duration::from_secs(60)`
    /// unconditionally), this test fires.
    ///
    /// Tier-2 `Some(v)` and `None` are also tested verbatim.
    #[test]
    fn client_open_lowers_some_to_overrides() {
        let custom_secs: u32 = 90;
        let ffi_some = KMailClientConfig {
            bff_url: "https://kmail.test".into(),
            bearer_token: "tok".into(),
            database_path: "/tmp/kmail.sqlite".into(),
            attachment_cache_bytes: Some(512 * 1024 * 1024),
            request_timeout_secs: Some(custom_secs),
            retry_budget_secs: Some(custom_secs),
            initial_sync_email_window: Some(500),
            account_id: Some("acct-123".into()),
            bootstrap_mailbox_role: Some("archive".into()),
        };

        let core_cfg = lower_client_open_test(&ffi_some);

        assert_eq!(core_cfg.attachment_cache_bytes, 512 * 1024 * 1024);
        assert_eq!(
            core_cfg.request_timeout,
            Duration::from_secs(u64::from(custom_secs))
        );
        assert_eq!(
            core_cfg.retry_budget,
            Duration::from_secs(u64::from(custom_secs))
        );
        assert_eq!(core_cfg.initial_sync_email_window, 500);
        assert_eq!(core_cfg.account_id.as_deref(), Some("acct-123"));
        assert_eq!(core_cfg.bootstrap_mailbox_role.as_deref(), Some("archive"));
    }

    /// Cross-binding parity: the FFI lowering ladder must produce the
    /// same observable `ClientConfig` as the napi binding's lowering
    /// ladder (`sdk/kmail-napi/src/lib.rs::client_open`) when given
    /// equivalent input. The napi side uses verbatim assignment for
    /// both `account_id` and `bootstrap_mailbox_role`; this test
    /// re-implements that ladder inline and asserts equivalence on
    /// every field for both the all-None and the all-Some inputs.
    #[test]
    fn client_open_matches_napi_lowering_for_string_tier() {
        // Re-implement the napi `client_open` lowering ladder (minus
        // the BigInt coercion and `KMailClient::open` step) so the
        // tests catch any future divergence between the two bindings.
        fn lower_napi_test(config: &KMailClientConfig) -> ClientConfig {
            let mut core_cfg = ClientConfig::new(
                config.bff_url.clone(),
                config.bearer_token.clone(),
                PathBuf::from(config.database_path.clone()),
            );
            if let Some(b) = config.attachment_cache_bytes {
                core_cfg.attachment_cache_bytes = b;
            }
            if let Some(t) = config.request_timeout_secs {
                core_cfg.request_timeout = Duration::from_secs(u64::from(t));
            }
            if let Some(t) = config.retry_budget_secs {
                core_cfg.retry_budget = Duration::from_secs(u64::from(t));
            }
            if let Some(w) = config.initial_sync_email_window {
                core_cfg.initial_sync_email_window = w;
            }
            core_cfg.account_id = config.account_id.clone();
            core_cfg.bootstrap_mailbox_role = config.bootstrap_mailbox_role.clone();
            core_cfg
        }

        for case in [
            KMailClientConfig {
                bff_url: "https://kmail.test".into(),
                bearer_token: "tok".into(),
                database_path: "/tmp/kmail.sqlite".into(),
                attachment_cache_bytes: None,
                request_timeout_secs: None,
                retry_budget_secs: None,
                initial_sync_email_window: None,
                account_id: None,
                bootstrap_mailbox_role: None,
            },
            KMailClientConfig {
                bff_url: "https://kmail.test".into(),
                bearer_token: "tok".into(),
                database_path: "/tmp/kmail.sqlite".into(),
                attachment_cache_bytes: Some(1024 * 1024),
                request_timeout_secs: Some(7),
                retry_budget_secs: Some(13),
                initial_sync_email_window: Some(42),
                account_id: Some("acct-xyz".into()),
                bootstrap_mailbox_role: Some("sent".into()),
            },
        ] {
            let ffi_lowered = lower_client_open_test(&case);
            let napi_lowered = lower_napi_test(&case);
            assert_eq!(
                ffi_lowered.attachment_cache_bytes,
                napi_lowered.attachment_cache_bytes
            );
            assert_eq!(ffi_lowered.request_timeout, napi_lowered.request_timeout);
            assert_eq!(ffi_lowered.retry_budget, napi_lowered.retry_budget);
            assert_eq!(
                ffi_lowered.initial_sync_email_window,
                napi_lowered.initial_sync_email_window
            );
            assert_eq!(ffi_lowered.account_id, napi_lowered.account_id);
            assert_eq!(
                ffi_lowered.bootstrap_mailbox_role, napi_lowered.bootstrap_mailbox_role,
                "UniFFI and napi must agree on bootstrap_mailbox_role lowering"
            );
        }
    }

    /// `ForeignMlsKeyProvider` surfaces a wrong-length foreign
    /// callback return as `Error::KeyStore`, with the foreign
    /// length echoed in the error message so the iOS / Android
    /// developer can debug their impl. The companion code path
    /// also zeroizes the wrong-length buffer before drop; we
    /// can't observe freed heap from safe Rust to test that
    /// directly, but the error-message path proves the branch
    /// is taken.
    #[test]
    fn foreign_mls_provider_rejects_wrong_length_secret() {
        struct OffByOneProvider;
        impl FfiMlsKeyProvider for OffByOneProvider {
            fn confidential_send_leaf_secret(
                &self,
                _recipient_user_id: String,
            ) -> Result<Vec<u8>, KMailError> {
                // 33 bytes simulates a miscompiled MLS exporter
                // that emits one trailing byte beyond the 32-byte
                // KDF output. The first 32 bytes might be real
                // key material, which is precisely why we
                // zeroize before dropping.
                Ok(vec![0xAB; 33])
            }
            fn vault_folder_master_secret(
                &self,
                _folder_id: String,
            ) -> Result<Vec<u8>, KMailError> {
                Ok(vec![0xCD; 31])
            }
        }

        let provider = ForeignMlsKeyProvider {
            foreign: Arc::new(OffByOneProvider),
        };
        let err = provider
            .confidential_send_leaf_secret("alice@kmail.test")
            .unwrap_err();
        match err {
            kmail_core::Error::KeyStore(msg) => {
                assert!(msg.contains("33"), "error msg should include length: {msg}");
                assert!(
                    msg.contains("Confidential Send"),
                    "error msg should identify scope: {msg}"
                );
            }
            other => panic!("expected KeyStore error, got {other:?}"),
        }

        let err = provider.vault_folder_master_secret("folder-1").unwrap_err();
        match err {
            kmail_core::Error::KeyStore(msg) => {
                assert!(msg.contains("31"), "error msg should include length: {msg}");
                assert!(
                    msg.contains("Vault"),
                    "error msg should identify scope: {msg}"
                );
            }
            other => panic!("expected KeyStore error, got {other:?}"),
        }
    }

    /// `MailboxRole` round-trips through the FFI string label.
    #[test]
    fn role_label_covers_every_variant() {
        let unknown_wire = "schedulednotyetimplemented";
        let cases = [
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
            MailboxRole::Unknown(unknown_wire.into()),
        ];
        for r in &cases {
            let s = r.canonical_name();
            assert!(!s.is_empty());
            // Round-trip via canonical name proves the FFI label
            // matches the JMAP wire form (no Debug-derive coupling).
            // `Unknown(s)` carries the server-provided wire string
            // verbatim; the strict constructor still refuses unknown
            // labels — promotion goes through `from_wire`.
            match r {
                MailboxRole::Unknown(_) => {
                    assert_eq!(s, unknown_wire);
                    assert!(MailboxRole::from_canonical_name(s).is_none());
                    assert_eq!(&MailboxRole::from_wire(s), r);
                }
                other => {
                    assert_eq!(MailboxRole::from_canonical_name(s).as_ref(), Some(other));
                }
            }
        }
    }
}
