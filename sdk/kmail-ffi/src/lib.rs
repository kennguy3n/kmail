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
