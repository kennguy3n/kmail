// N-API bindings for the KMail SDK (Electron / Node.js).
//
// The surface mirrors `kmail-ffi` (UniFFI). napi-rs handles
// async/await translation via the `tokio_rt` feature — every
// `async fn` exposed below becomes a Promise on the JS side.
//
// Type bridge:
//   * napi-rs's `#[napi]` macros emit BigInt for u64/i64 to avoid
//     JS's 53-bit precision loss; the Electron renderer converts
//     to Number where appropriate.
//   * Vec<EmailAddress>, BTreeMap<String, bool>, etc. are
//     translated to JS arrays / plain objects.
//   * Errors flatten to JS-friendly { code, message } strings —
//     napi::Error doesn't expose Rust enum tags so we encode the
//     variant tag in the error message prefix, parsed by the
//     Electron-side wrapper into a typed exception class.

#![deny(clippy::all)]

use std::collections::{BTreeMap, HashMap};
use std::path::PathBuf;
use std::sync::Arc;
#[cfg(test)]
use std::time::Duration;

use napi::bindgen_prelude::*;
use napi_derive::napi;

use kmail_core::{AeadEnvelope, ClientConfig, ConfidentialEnvelope, EmailDraft, KMailClient};

// ---------------------------------------------------------------
// Error mapping
// ---------------------------------------------------------------

fn napi_err(e: kmail_core::Error) -> Error {
    let (status, msg) = match &e {
        kmail_core::Error::Store(_) => (Status::GenericFailure, format!("[STORE] {e}")),
        kmail_core::Error::Transport(_) => (Status::GenericFailure, format!("[TRANSPORT] {e}")),
        kmail_core::Error::Auth(_) => (Status::GenericFailure, format!("[AUTH] {e}")),
        kmail_core::Error::Forbidden(_) => (Status::GenericFailure, format!("[FORBIDDEN] {e}")),
        kmail_core::Error::NotFound(_) => (Status::GenericFailure, format!("[NOT_FOUND] {e}")),
        kmail_core::Error::RateLimit { .. } => {
            (Status::GenericFailure, format!("[RATE_LIMIT] {e}"))
        }
        kmail_core::Error::JmapMethod { .. } => (Status::GenericFailure, format!("[JMAP] {e}")),
        kmail_core::Error::Protocol(_) => (Status::GenericFailure, format!("[PROTOCOL] {e}")),
        kmail_core::Error::HttpClient { .. } => {
            (Status::GenericFailure, format!("[HTTP_CLIENT] {e}"))
        }
        kmail_core::Error::SyncStateDiverged => {
            (Status::GenericFailure, format!("[SYNC_DIVERGED] {e}"))
        }
        kmail_core::Error::Decryption(_) => (Status::GenericFailure, format!("[DECRYPTION] {e}")),
        kmail_core::Error::KeyDerivation(_) => (Status::GenericFailure, format!("[KDF] {e}")),
        kmail_core::Error::KeyStore(_) => (Status::GenericFailure, format!("[KEYSTORE] {e}")),
        kmail_core::Error::InvalidArgument(_) => (Status::InvalidArg, format!("[ARG] {e}")),
        kmail_core::Error::Cancelled => (Status::Cancelled, format!("[CANCELLED] {e}")),
    };
    Error::new(status, msg)
}

// ---------------------------------------------------------------
// JS-shaped records
// ---------------------------------------------------------------

#[napi(object)]
pub struct JsClientConfig {
    pub bff_url: String,
    pub bearer_token: String,
    pub database_path: String,
    /// Soft cap on the attachment cache, in bytes. Default 256MiB.
    pub attachment_cache_bytes: Option<BigInt>,
    pub request_timeout_secs: Option<u32>,
    pub retry_budget_secs: Option<u32>,
    pub initial_sync_email_window: Option<u32>,
    pub account_id: Option<String>,
    pub bootstrap_mailbox_role: Option<String>,
}

#[napi(object)]
pub struct JsMailbox {
    pub id: String,
    pub name: String,
    pub role: Option<String>,
    pub parent_id: Option<String>,
    pub sort_order: u32,
    pub total_emails: BigInt,
    pub unread_emails: BigInt,
    pub is_vault: bool,
}

#[napi(object)]
pub struct JsEmailAddress {
    pub name: String,
    pub email: String,
}

#[napi(object)]
pub struct JsEmailSummary {
    pub id: String,
    pub thread_id: String,
    pub blob_id: String,
    pub mailbox_ids: Vec<String>,
    pub keyword_flags: Vec<String>,
    pub size: BigInt,
    pub received_at_unix: i64,
    pub sent_at_unix: Option<i64>,
    pub from_addresses: Vec<JsEmailAddress>,
    pub to_addresses: Vec<JsEmailAddress>,
    pub cc_addresses: Vec<JsEmailAddress>,
    pub bcc_addresses: Vec<JsEmailAddress>,
    pub subject: String,
    pub preview: String,
    pub has_attachment: bool,
}

#[napi(object)]
pub struct JsSyncSummary {
    pub mailboxes_upserted: BigInt,
    pub mailboxes_destroyed: BigInt,
    pub emails_created: BigInt,
    pub emails_updated: BigInt,
    pub emails_destroyed: BigInt,
    pub pending_actions_applied: BigInt,
    pub pending_actions_failed: BigInt,
    pub pending_actions_deferred: BigInt,
}

/// Renderable local notification, mirroring
/// [`kmail_core::LocalNotification`]. The Electron main process maps
/// this onto an OS-level `Notification`.
#[napi(object)]
pub struct JsLocalNotification {
    pub title: String,
    pub body: String,
    pub tag: String,
    pub account_id: Option<String>,
    pub email_id: Option<String>,
    pub mailbox_id: Option<String>,
    pub thread_id: Option<String>,
    pub received_at_unix: Option<i64>,
    pub has_attachment: bool,
}

/// Outcome of [`KMailClientJs::ingest_push_delivery`], mirroring
/// [`kmail_core::PushIngestOutcome`].
#[napi(object)]
pub struct JsPushIngestOutcome {
    pub notification: Option<JsLocalNotification>,
    pub email_cached: bool,
    pub needs_delta_sync: bool,
}

/// AEAD envelope at the napi boundary.
///
/// JS-side fields use `Buffer` for byte arrays. `nonce` is a
/// variable-length `Vec<u8>` because napi-rs cannot represent
/// fixed-size byte arrays directly; the `try_into` impl below
/// enforces the `NONCE_LEN == 12` invariant, surfacing a wrong-
/// length nonce as a `[ARG]`-tagged error.
#[napi(object)]
pub struct JsAeadEnvelope {
    pub nonce: Buffer,
    pub ciphertext: Buffer,
    pub aad: Buffer,
}

impl From<AeadEnvelope> for JsAeadEnvelope {
    fn from(env: AeadEnvelope) -> Self {
        Self {
            nonce: env.nonce.to_vec().into(),
            ciphertext: env.ciphertext.into(),
            aad: env.aad.into(),
        }
    }
}

impl TryFrom<JsAeadEnvelope> for AeadEnvelope {
    type Error = Error;
    fn try_from(env: JsAeadEnvelope) -> Result<Self> {
        let nonce_vec = env.nonce.to_vec();
        if nonce_vec.len() != kmail_core::crypto::NONCE_LEN {
            return Err(Error::new(
                Status::InvalidArg,
                format!(
                    "[ARG] AEAD nonce must be {} bytes, got {}",
                    kmail_core::crypto::NONCE_LEN,
                    nonce_vec.len()
                ),
            ));
        }
        let mut nonce = [0u8; kmail_core::crypto::NONCE_LEN];
        nonce.copy_from_slice(&nonce_vec);
        Ok(AeadEnvelope {
            nonce,
            ciphertext: env.ciphertext.to_vec(),
            aad: env.aad.to_vec(),
        })
    }
}

/// Confidential Send envelope at the napi boundary.
#[napi(object)]
pub struct JsConfidentialEnvelope {
    pub kek_salt: Buffer,
    pub wrapped_dek: JsAeadEnvelope,
    pub payload: JsAeadEnvelope,
}

impl From<ConfidentialEnvelope> for JsConfidentialEnvelope {
    fn from(env: ConfidentialEnvelope) -> Self {
        Self {
            kek_salt: env.kek_salt.to_vec().into(),
            wrapped_dek: env.wrapped_dek.into(),
            payload: env.payload.into(),
        }
    }
}

impl TryFrom<JsConfidentialEnvelope> for ConfidentialEnvelope {
    type Error = Error;
    fn try_from(env: JsConfidentialEnvelope) -> Result<Self> {
        let salt_vec = env.kek_salt.to_vec();
        if salt_vec.len() != kmail_core::crypto::KEK_SALT_LEN {
            return Err(Error::new(
                Status::InvalidArg,
                format!(
                    "[ARG] Confidential KEK salt must be {} bytes, got {}",
                    kmail_core::crypto::KEK_SALT_LEN,
                    salt_vec.len()
                ),
            ));
        }
        let mut kek_salt = [0u8; kmail_core::crypto::KEK_SALT_LEN];
        kek_salt.copy_from_slice(&salt_vec);
        Ok(ConfidentialEnvelope {
            kek_salt,
            wrapped_dek: env.wrapped_dek.try_into()?,
            payload: env.payload.try_into()?,
        })
    }
}

// ---------------------------------------------------------------
// MLS provider plumbing — napi surface
// ---------------------------------------------------------------
//
// The Electron renderer manages its own provider in JS-land:
// the renderer looks up the relevant MLS-derived secret via the
// KChat MLS SDK and threads it through the raw-key surface
// (`seal_vault_envelope` / `decrypt_vault_envelope` /
// `seal_confidential_envelope` / `open_confidential_envelope`).
// This keeps the napi binding free of the sync-to-async bridge
// (`ThreadsafeFunction` + `block_in_place`) that would otherwise
// be required to surface the sync `MlsKeyProvider` trait through
// an async JS callback.
//
// The iOS / Android shells get the richer `set_mls_provider` /
// `*_message` convenience surface via the UniFFI binding
// (`kmail-ffi`) because UniFFI's `#[uniffi::export(with_foreign)]`
// handles foreign-implemented sync traits natively. The full
// provider bridge can be added to napi in a follow-up once we
// make the SDK-internal `MlsKeyProvider` trait async (which is
// a wider change touching the FFI binding and every consumer
// inside `KMailClient`).

// ---------------------------------------------------------------
// Conversions
// ---------------------------------------------------------------

fn bigint_u64(v: u64) -> BigInt {
    BigInt::from(v)
}

fn mailbox_to_js(m: kmail_core::Mailbox) -> JsMailbox {
    JsMailbox {
        id: m.id,
        name: m.name,
        role: m.role.map(|r| r.canonical_name().to_string()),
        parent_id: m.parent_id,
        sort_order: m.sort_order,
        total_emails: bigint_u64(m.total_emails),
        unread_emails: bigint_u64(m.unread_emails),
        is_vault: m.is_vault,
    }
}

fn addr_to_js(a: kmail_core::EmailAddress) -> JsEmailAddress {
    JsEmailAddress {
        name: a.name,
        email: a.email,
    }
}

fn summary_to_js(s: kmail_core::EmailSummary) -> JsEmailSummary {
    JsEmailSummary {
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
        size: bigint_u64(s.size),
        received_at_unix: s.received_at.timestamp(),
        sent_at_unix: s.sent_at.map(|t| t.timestamp()),
        from_addresses: s.from.into_iter().map(addr_to_js).collect(),
        to_addresses: s.to.into_iter().map(addr_to_js).collect(),
        cc_addresses: s.cc.into_iter().map(addr_to_js).collect(),
        bcc_addresses: s.bcc.into_iter().map(addr_to_js).collect(),
        subject: s.subject,
        preview: s.preview,
        has_attachment: s.has_attachment,
    }
}

fn sync_summary_to_js(s: kmail_core::SyncSummary) -> JsSyncSummary {
    JsSyncSummary {
        mailboxes_upserted: bigint_u64(s.mailboxes_upserted),
        mailboxes_destroyed: bigint_u64(s.mailboxes_destroyed),
        emails_created: bigint_u64(s.emails_created),
        emails_updated: bigint_u64(s.emails_updated),
        emails_destroyed: bigint_u64(s.emails_destroyed),
        pending_actions_applied: bigint_u64(s.pending_actions_applied),
        pending_actions_failed: bigint_u64(s.pending_actions_failed),
        pending_actions_deferred: bigint_u64(s.pending_actions_deferred),
    }
}

fn notification_to_js(n: kmail_core::LocalNotification) -> JsLocalNotification {
    JsLocalNotification {
        title: n.title,
        body: n.body,
        tag: n.tag,
        account_id: n.account_id,
        email_id: n.email_id,
        mailbox_id: n.mailbox_id,
        thread_id: n.thread_id,
        received_at_unix: n.received_at_unix,
        has_attachment: n.has_attachment,
    }
}

fn push_outcome_to_js(o: kmail_core::PushIngestOutcome) -> JsPushIngestOutcome {
    JsPushIngestOutcome {
        notification: o.notification.map(notification_to_js),
        email_cached: o.email_cached,
        needs_delta_sync: o.needs_delta_sync,
    }
}

// ---------------------------------------------------------------
// Defaults helper
// ---------------------------------------------------------------

/// Return a `JsClientConfig` pre-populated with the SDK's canonical
/// defaults for every non-required field, mirroring the UniFFI
/// binding's `default_client_config(...)` (see
/// `sdk/kmail-ffi/src/lib.rs::default_client_config`).
///
/// This exists so Electron / Node callers can read the SDK's
/// defaults programmatically rather than hard-coding them in JS
/// (which would replicate the drift bug the UniFFI version was
/// designed to eliminate). A future change to `ClientConfig::new`
/// in `kmail-core` automatically flows into every JS consumer on
/// the next `.node` rebuild — no JS-side update required.
///
/// The returned object has `accountId = null` and uses the
/// caller-supplied `bffUrl` / `bearerToken` / `databasePath`
/// because there is no sensible default for those — they are
/// always tenant-specific.
///
/// `attachmentCacheBytes` is emitted as a `BigInt` to match the
/// `JsClientConfig` field shape (the underlying Rust type is
/// `u64`, which JS `Number` cannot losslessly represent).
///
/// All Duration defaults are converted to `u32` via `as_secs()`,
/// truncating any sub-second component. `ClientConfig::new`
/// currently uses whole-second defaults (30s / 60s), and the
/// `default_client_config_mirrors_core_defaults` test in
/// `sdk/kmail-ffi/src/lib.rs` compares at millisecond precision
/// to catch any future fractional-second drift.
#[napi]
pub fn default_client_config(
    bff_url: String,
    bearer_token: String,
    database_path: String,
) -> JsClientConfig {
    let core = ClientConfig::new(bff_url, bearer_token, PathBuf::from(database_path));
    let request_timeout_secs = u32::try_from(core.request_timeout.as_secs()).unwrap_or(u32::MAX);
    let retry_budget_secs = u32::try_from(core.retry_budget.as_secs()).unwrap_or(u32::MAX);
    JsClientConfig {
        bff_url: core.bff_url,
        bearer_token: core.bearer_token,
        database_path: core.database_path.to_string_lossy().into_owned(),
        attachment_cache_bytes: Some(BigInt::from(core.attachment_cache_bytes)),
        request_timeout_secs: Some(request_timeout_secs),
        retry_budget_secs: Some(retry_budget_secs),
        initial_sync_email_window: Some(core.initial_sync_email_window),
        account_id: core.account_id,
        bootstrap_mailbox_role: core.bootstrap_mailbox_role,
    }
}

// ---------------------------------------------------------------
// Top-level: open() returning a class wrapper
// ---------------------------------------------------------------

#[napi]
pub struct KMailClientJs {
    inner: Arc<KMailClient>,
}

#[napi]
impl KMailClientJs {
    /// Open the SDK. Synchronous because the heavy lifting (schema
    /// migrations) is fast enough to not warrant a Promise.
    #[napi(factory)]
    pub fn open(config: JsClientConfig) -> Result<KMailClientJs> {
        let mut core_cfg = ClientConfig::new(
            config.bff_url,
            config.bearer_token,
            PathBuf::from(config.database_path),
        );

        // BigInt → u64 lowering happens here, before the shared
        // `apply_optional_overrides` ladder, because the validation
        // failure surface is napi-specific (a JS caller passing
        // `-1n` should see `Status::InvalidArg`, not a Rust-side
        // panic or a silent absolute-value coercion). napi-rs's
        // `BigInt::get_u64()` returns `(signed, value, lossless)`;
        // we reject signed BigInts and overflow-on-truncate cases
        // up-front. By the time we reach the shared ladder, the
        // attachment cache override is an ordinary `Option<u64>`.
        let attachment_cache_bytes = if let Some(b) = config.attachment_cache_bytes {
            let (signed, value, lossless) = b.get_u64();
            if signed {
                return Err(Error::new(
                    Status::InvalidArg,
                    "attachment_cache_bytes must be a non-negative BigInt",
                ));
            }
            if !lossless {
                return Err(Error::new(
                    Status::InvalidArg,
                    "attachment_cache_bytes overflows u64",
                ));
            }
            Some(value)
        } else {
            None
        };

        // Delegate the two-tier optional-override lowering to the
        // shared helper in `kmail-core`. Identical call shape to
        // `kmail-ffi::client_open`, by design: the two bindings
        // must lower the same way so a foreign caller's `None`
        // produces the same observable `ClientConfig` regardless
        // of whether they reach the SDK via UniFFI or napi.
        core_cfg.apply_optional_overrides(
            attachment_cache_bytes,
            config.request_timeout_secs,
            config.retry_budget_secs,
            config.initial_sync_email_window,
            config.account_id,
            config.bootstrap_mailbox_role,
        );

        let inner = KMailClient::open(core_cfg).map_err(napi_err)?;
        Ok(KMailClientJs {
            inner: Arc::new(inner),
        })
    }

    #[napi]
    pub async fn sync(&self) -> Result<JsSyncSummary> {
        let inner = (*self.inner).clone();
        let summary = inner.sync().await.map_err(napi_err)?;
        Ok(sync_summary_to_js(summary))
    }

    /// Hot-swap the OIDC bearer token. Electron should call this from
    /// whatever refresh-token flow it owns, rather than recreating the
    /// SDK instance.
    #[napi]
    pub fn set_bearer_token(&self, token: String) -> Result<()> {
        self.inner.set_bearer_token(token).map_err(napi_err)
    }

    /// Drop the cached JMAP session so the next sync re-fetches
    /// `/jmap/session`. Call after a reauth-required 401, tenant
    /// resharding webhook, or plan-upgrade event.
    #[napi]
    pub async fn invalidate_session(&self) {
        let inner = (*self.inner).clone();
        inner.invalidate_session().await
    }

    #[napi]
    pub fn cached_mailboxes(&self) -> Result<Vec<JsMailbox>> {
        let v = self.inner.cached_mailboxes().map_err(napi_err)?;
        Ok(v.into_iter().map(mailbox_to_js).collect())
    }

    #[napi]
    pub fn cached_emails_in_mailbox(
        &self,
        mailbox_id: String,
        limit: u32,
    ) -> Result<Vec<JsEmailSummary>> {
        let v = self
            .inner
            .cached_emails_in_mailbox(&mailbox_id, limit)
            .map_err(napi_err)?;
        Ok(v.into_iter().map(summary_to_js).collect())
    }

    #[napi]
    pub fn enqueue_set_keywords(&self, email_id: String, keywords_json: String) -> Result<()> {
        let keywords: serde_json::Value = serde_json::from_str(&keywords_json).map_err(|e| {
            Error::new(
                Status::InvalidArg,
                format!("[ARG] invalid keywords json: {e}"),
            )
        })?;
        self.inner
            .enqueue_set_keywords(&email_id, &keywords)
            .map_err(napi_err)
    }

    #[napi]
    pub async fn send_email(&self, draft_json: String) -> Result<String> {
        let draft: EmailDraft = serde_json::from_str(&draft_json).map_err(|e| {
            Error::new(Status::InvalidArg, format!("[ARG] invalid draft json: {e}"))
        })?;
        let inner = (*self.inner).clone();
        inner.send_email(&draft).await.map_err(napi_err)
    }

    #[napi]
    pub async fn register_apns_token(&self, token: String) -> Result<()> {
        let inner = (*self.inner).clone();
        inner
            .register_push_token(kmail_core::push::PushTransport::Apns, &token, None)
            .await
            .map_err(napi_err)
    }

    #[napi]
    pub async fn register_fcm_token(&self, token: String) -> Result<()> {
        let inner = (*self.inner).clone();
        inner
            .register_push_token(kmail_core::push::PushTransport::Fcm, &token, None)
            .await
            .map_err(napi_err)
    }

    /// Ingest a transport-level push payload (the raw notification
    /// `data` map) and return the parsed notification plus sync
    /// flags.
    ///
    /// Synchronous (a parse + one local SQLite upsert). Background
    /// sync on desktop is driven from the Electron main process with
    /// `setInterval(() => client.sync(), …)` rather than a Rust-side
    /// worker — `setInterval` integrates with Electron's lifecycle
    /// (pause on `powerMonitor` suspend, clear on `before-quit`),
    /// which a detached Rust task cannot observe.
    #[napi]
    pub fn ingest_push_delivery(
        &self,
        data: HashMap<String, String>,
    ) -> Result<JsPushIngestOutcome> {
        let data: BTreeMap<String, String> = data.into_iter().collect();
        let outcome = self.inner.ingest_push_delivery(&data).map_err(napi_err)?;
        Ok(push_outcome_to_js(outcome))
    }

    // ---------------------------------------------------------
    // Crypto surface (Confidential Send + Zero-Access Vault)
    // ---------------------------------------------------------

    /// Seal `plaintext` into a Zero-Access Vault envelope under a
    /// caller-supplied 32-byte per-folder master key.
    #[napi]
    pub fn seal_vault_envelope(
        &self,
        folder_master_key: Buffer,
        plaintext: Buffer,
        aad: Buffer,
    ) -> Result<JsAeadEnvelope> {
        let env = self
            .inner
            .seal_vault_envelope(&folder_master_key, &plaintext, &aad)
            .map_err(napi_err)?;
        Ok(env.into())
    }

    /// Decrypt a Zero-Access Vault envelope. Symmetric inverse
    /// of [`seal_vault_envelope`].
    #[napi]
    pub fn decrypt_vault_envelope(
        &self,
        folder_master_key: Buffer,
        envelope: JsAeadEnvelope,
    ) -> Result<Buffer> {
        let env: AeadEnvelope = envelope.try_into()?;
        let pt = self
            .inner
            .decrypt_vault_envelope(&folder_master_key, &env)
            .map_err(napi_err)?;
        Ok(pt.into())
    }

    /// Seal `plaintext` into a Confidential Send envelope under
    /// a caller-supplied 32-byte MLS leaf secret.
    #[napi]
    pub fn seal_confidential_envelope(
        &self,
        mls_leaf_secret: Buffer,
        plaintext: Buffer,
        payload_aad: Buffer,
        wrap_aad: Buffer,
    ) -> Result<JsConfidentialEnvelope> {
        let env = self
            .inner
            .seal_confidential_envelope(&mls_leaf_secret, &plaintext, &payload_aad, &wrap_aad)
            .map_err(napi_err)?;
        Ok(env.into())
    }

    /// Open a Confidential Send envelope. Symmetric inverse of
    /// [`seal_confidential_envelope`].
    #[napi]
    pub fn open_confidential_envelope(
        &self,
        mls_leaf_secret: Buffer,
        envelope: JsConfidentialEnvelope,
    ) -> Result<Buffer> {
        let env: ConfidentialEnvelope = envelope.try_into()?;
        let pt = self
            .inner
            .open_confidential_envelope(&mls_leaf_secret, &env)
            .map_err(napi_err)?;
        Ok(pt.into())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Every error prefix the Electron-side wrapper parses MUST
    /// remain stable. A regression here breaks the typed
    /// exception class on the JS side.
    #[test]
    fn error_prefixes_are_stable() {
        for (src, prefix) in [
            (kmail_core::Error::Store("x".into()), "[STORE]"),
            (kmail_core::Error::Transport("x".into()), "[TRANSPORT]"),
            (kmail_core::Error::Auth("x".into()), "[AUTH]"),
            (kmail_core::Error::Forbidden("x".into()), "[FORBIDDEN]"),
            (kmail_core::Error::NotFound("x".into()), "[NOT_FOUND]"),
            (
                kmail_core::Error::RateLimit {
                    retry_after_seconds: 1,
                },
                "[RATE_LIMIT]",
            ),
            (
                kmail_core::Error::JmapMethod {
                    code: "x".into(),
                    description: "y".into(),
                },
                "[JMAP]",
            ),
            (kmail_core::Error::Protocol("x".into()), "[PROTOCOL]"),
            (
                kmail_core::Error::HttpClient {
                    status: 422,
                    body: "malformed".into(),
                },
                "[HTTP_CLIENT]",
            ),
            (kmail_core::Error::SyncStateDiverged, "[SYNC_DIVERGED]"),
            (kmail_core::Error::Decryption("x".into()), "[DECRYPTION]"),
            (kmail_core::Error::KeyDerivation("x".into()), "[KDF]"),
            (kmail_core::Error::KeyStore("x".into()), "[KEYSTORE]"),
            (kmail_core::Error::InvalidArgument("x".into()), "[ARG]"),
            (kmail_core::Error::Cancelled, "[CANCELLED]"),
        ] {
            let err = napi_err(src);
            assert!(
                err.reason.starts_with(prefix),
                "expected reason to start with {prefix}, got: {}",
                err.reason
            );
        }
    }

    /// Cross-binding parity: the napi `KMailClientJs::open` lowering
    /// ladder MUST go through the shared
    /// `ClientConfig::apply_optional_overrides` helper in
    /// `kmail-core`, matching the UniFFI binding
    /// (`sdk/kmail-ffi/src/lib.rs::client_open`) exactly. If a
    /// future refactor inlines per-field assignment back into
    /// `KMailClientJs::open`, the two bindings could drift again
    /// (the original ANALYSIS_pr-review-job ANALYSIS_0006 finding).
    ///
    /// This test rebuilds the lowering result here by calling
    /// `apply_optional_overrides` directly and asserts every field
    /// matches what the shared helper produces. It does NOT exercise
    /// the BigInt validation branch (`attachment_cache_bytes` is
    /// produced as `Option<u64>` after that branch runs) — that's
    /// tested separately in the napi-side integration tests.
    #[test]
    fn napi_open_lowering_matches_shared_helper() {
        let bff_url = "https://kmail.test".to_string();
        let bearer_token = "tok".to_string();
        let database_path = "/tmp/kmail.sqlite".to_string();

        for (
            attachment_cache_bytes,
            request_timeout_secs,
            retry_budget_secs,
            initial_sync_email_window,
            account_id,
            bootstrap_mailbox_role,
        ) in [
            (None, None, None, None, None, None),
            (
                Some(1024u64 * 1024),
                Some(7u32),
                Some(13u32),
                Some(42u32),
                Some("acct-xyz".to_string()),
                Some("sent".to_string()),
            ),
        ] {
            let mut via_shared = ClientConfig::new(
                bff_url.clone(),
                bearer_token.clone(),
                PathBuf::from(database_path.clone()),
            );
            via_shared.apply_optional_overrides(
                attachment_cache_bytes,
                request_timeout_secs,
                retry_budget_secs,
                initial_sync_email_window,
                account_id.clone(),
                bootstrap_mailbox_role.clone(),
            );

            // Tier-1 numeric: `None` means inherit ClientConfig::new
            // default; `Some(v)` overrides.
            let reference = ClientConfig::new(
                bff_url.clone(),
                bearer_token.clone(),
                PathBuf::from(database_path.clone()),
            );
            assert_eq!(
                via_shared.attachment_cache_bytes,
                attachment_cache_bytes.unwrap_or(reference.attachment_cache_bytes)
            );
            assert_eq!(
                via_shared.request_timeout,
                Duration::from_secs(u64::from(request_timeout_secs.unwrap_or_else(|| {
                    u32::try_from(reference.request_timeout.as_secs()).unwrap()
                })))
            );
            assert_eq!(
                via_shared.retry_budget,
                Duration::from_secs(u64::from(retry_budget_secs.unwrap_or_else(|| {
                    u32::try_from(reference.retry_budget.as_secs()).unwrap()
                })))
            );
            assert_eq!(
                via_shared.initial_sync_email_window,
                initial_sync_email_window.unwrap_or(reference.initial_sync_email_window)
            );

            // Tier-2 string: verbatim assignment.
            assert_eq!(via_shared.account_id, account_id);
            assert_eq!(via_shared.bootstrap_mailbox_role, bootstrap_mailbox_role);
        }
    }

    /// `default_client_config` (napi) MUST return the same defaults
    /// as `default_client_config` in `kmail-ffi`. Both bindings
    /// derive their values from a fresh `ClientConfig::new(...)`, so
    /// drift is structurally impossible — but a future refactor of
    /// either binding could regress, e.g. someone could mistakenly
    /// hard-code a literal into the napi version instead of sourcing
    /// it from `ClientConfig::new`. This test pins each field
    /// against a freshly-constructed `ClientConfig` to catch that.
    ///
    /// Mirrors the `default_client_config_mirrors_core_defaults`
    /// test in `sdk/kmail-ffi/src/lib.rs`; the per-binding pair of
    /// tests is the load-bearing drift guard. (The Electron-side
    /// integration test in `apps/desktop/src/kmail/client.test.ts`
    /// exercises this from JS-land to ensure the BigInt round-trip
    /// preserves the byte count.)
    #[test]
    fn default_client_config_mirrors_core_defaults() {
        let bff_url = "https://kmail.test".to_string();
        let bearer_token = "test-bearer".to_string();
        let database_path = "/tmp/k.sqlite".to_string();

        let core = ClientConfig::new(
            bff_url.clone(),
            bearer_token.clone(),
            PathBuf::from(database_path.clone()),
        );
        let js = default_client_config(bff_url, bearer_token, database_path);

        assert_eq!(js.bff_url, core.bff_url);
        assert_eq!(js.bearer_token, core.bearer_token);
        assert_eq!(
            js.database_path,
            core.database_path.to_string_lossy().into_owned()
        );

        // `attachment_cache_bytes` is a `BigInt` on the JS side. Use
        // `get_u64` to pull out the underlying u64 for comparison.
        let (signed, value, lossless) = js
            .attachment_cache_bytes
            .as_ref()
            .expect("default_client_config must emit Some(attachment_cache_bytes)")
            .get_u64();
        assert!(!signed, "attachment_cache_bytes must be non-negative");
        assert!(lossless, "attachment_cache_bytes must fit in u64");
        assert_eq!(value, core.attachment_cache_bytes);

        // Millisecond-precision Duration parity (same rationale as
        // the UniFFI binding's `default_client_config_mirrors_core_defaults`
        // test — `as_secs()` truncation would silently round down
        // any future fractional default).
        let js_request_ms = u128::from(
            js.request_timeout_secs
                .expect("default_client_config must emit Some(request_timeout_secs)"),
        ) * 1000;
        let js_retry_ms = u128::from(
            js.retry_budget_secs
                .expect("default_client_config must emit Some(retry_budget_secs)"),
        ) * 1000;
        assert_eq!(
            js_request_ms,
            core.request_timeout.as_millis(),
            "napi default_client_config request_timeout drifts: as_secs() would silently \
             truncate any sub-second component of ClientConfig::new's default. Either round \
             the Rust default back to whole seconds or migrate the napi field to milliseconds."
        );
        assert_eq!(
            js_retry_ms,
            core.retry_budget.as_millis(),
            "napi default_client_config retry_budget drifts: see request_timeout assertion."
        );

        assert_eq!(
            js.initial_sync_email_window,
            Some(core.initial_sync_email_window)
        );
        assert_eq!(js.account_id, core.account_id);
        assert_eq!(js.bootstrap_mailbox_role, core.bootstrap_mailbox_role);

        // Lock down the concrete defaults too — drift here means a
        // Rust-side default changed and every binding picks it up
        // automatically on the next rebuild, which is the desired
        // architecture but worth pinning as a contract for the
        // Electron-side wrapper to assert against.
        assert_eq!(
            js.attachment_cache_bytes.unwrap().get_u64().1,
            256 * 1024 * 1024
        );
        assert_eq!(js.request_timeout_secs, Some(30));
        assert_eq!(js.retry_budget_secs, Some(60));
        assert_eq!(js.initial_sync_email_window, Some(200));
        assert_eq!(js.bootstrap_mailbox_role.as_deref(), Some("inbox"));
        assert_eq!(js.account_id, None);
    }
}
