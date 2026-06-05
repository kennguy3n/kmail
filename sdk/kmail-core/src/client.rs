// `KMailClient` — public façade composed from the JMAP client and
// the offline-sync repositories.
//
// Lifecycle:
//
//   open(cfg) -> KMailClient
//   discover_session() -> JmapSession        # cached after first call
//   sync()                                    # delta-pull driven by sync_state
//   list_mailboxes() / list_emails() / ...    # served from SQLite
//   fetch_email(id, with_bodies=true)         # falls back to JMAP if absent locally
//   send_email(draft)
//   register_push_token(...)
//
// `sync()` is the load-bearing entry point. Sequence:
//
//   1. Fetch session if not already cached.
//   2. Pull all mailboxes via Mailbox/get and persist them.
//   3. If we have a saved `Email` state token, run Email/changes;
//      otherwise fetch a recent slice via Email/query.
//   4. Hydrate each created/updated ID with an Email/get batch and
//      apply the mutations to the local store.
//   5. Persist the new state tokens.

use crate::cache::AttachmentCache;
use crate::crypto::{self, AeadEnvelope, ConfidentialEnvelope, MlsKeyProvider};
use crate::error::{Error, Result};
use crate::jmap::transport::TransportConfig;
use crate::jmap::JmapClient;
use crate::models::{
    BootstrapRequest, BootstrapResponse, Email, EmailAddress, EmailDraft, EmailSummary,
    JmapSession, Mailbox,
};
use crate::notification::LocalNotification;
use crate::push::{EmailDeliveryHint, PushSubscriptionRequest, PushTransport, WebPushKeys};
use crate::sync::{
    ActionsRepo, EmailMutation, EmailRepo, MailboxRepo, PendingAction, PendingActionKind,
    StateRepo, Store, SyncTypeName,
};
use chrono::{DateTime, Utc};
use std::collections::BTreeMap;
use std::future::Future;
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::sync::{Notify, RwLock};

/// Upper bound on the number of `Email/changes` batches we will
/// drain in a single `sync()` call.
///
/// RFC 8620 §5.2 lets the server signal `hasMoreChanges: true` to
/// indicate that the returned change set is truncated and the
/// client should call `Foo/changes` again with the new state. At
/// 500 changes per batch (our `max_changes` argument), 64 batches
/// is 32k changes — more than enough headroom for typical first-
/// sync catch-up, and a hard ceiling so a buggy server that
/// always returns `hasMoreChanges: true` can't make `sync()` spin
/// forever.
const MAX_EMAIL_CHANGES_BATCHES_PER_SYNC: u32 = 64;

/// Upper bound on the number of `Mailbox/changes` batches we will
/// drain in a single `sync()` call.
///
/// Mirrors `MAX_EMAIL_CHANGES_BATCHES_PER_SYNC` in spirit: per
/// RFC 8620 §5.2 the server can paginate `Foo/changes` arbitrarily,
/// so we need a hard ceiling against a pathological server that
/// always returns `hasMoreChanges: true`. Mailbox sets are O(dozens)
/// in steady state, but the *transient* fan-out during a bulk
/// folder import or a label migration can spike well beyond that.
/// The ceiling has to be high enough that a legitimate bulk-import
/// catch-up converges in one `sync()` call rather than dripping
/// through over many user-visible round-trips — 32 batches at the
/// typical Stalwart page size of 128 buys us ~4k mailbox mutations
/// per sync, which is generous for any realistic deployment.
const MAX_MAILBOX_CHANGES_BATCHES_PER_SYNC: u32 = 32;

/// Cap on how many times an individual offline action will be
/// retried before the queue treats it as terminally stuck.
///
/// Each retryable failure bumps `pending_actions.attempts`. With a
/// typical reconnect cadence (every `sync()` call once the user
/// foregrounds the app, plus push-driven syncs on incoming mail),
/// 10 attempts is roughly an hour of wall-clock retries before
/// the queue gives up. The action is then completed off the queue
/// and counted under `SyncSummary::pending_actions_failed` so the
/// shell can surface a banner ("we couldn't send 3 messages")
/// rather than letting the queue accumulate forever.
///
/// 10 was chosen pragmatically: high enough that a multi-hour
/// network partition or a routine BFF restart doesn't drop user
/// writes, low enough that a permanently misconfigured account
/// (expired credentials, malformed payload that the server
/// somehow returns 5xx for) does not pin the queue. Configurable
/// per-deployment is a follow-up — the value lives in the SDK so
/// it ships with sensible defaults.
const MAX_PENDING_ACTION_ATTEMPTS: i64 = 10;

/// SDK configuration.
///
/// `Debug` is implemented by hand so that `bearer_token` is
/// **never** rendered verbatim, even via `tracing::debug!(?cfg, ...)`.
/// The struct still derives `Clone`; the redaction is purely a
/// defence-in-depth measure to keep OIDC tokens out of crash logs,
/// breadcrumbs, and telemetry pipelines.
#[derive(Clone)]
pub struct ClientConfig {
    /// Absolute BFF base URL (e.g. `https://kmail.example.com`).
    pub bff_url: String,
    /// OIDC bearer token. The platform shell refreshes it via
    /// [`KMailClient::set_bearer_token`]; the SDK does not run OAuth
    /// flows itself.
    pub bearer_token: String,
    /// Path to the per-account SQLite database.
    pub database_path: PathBuf,
    /// Optional override for the account ID. When `None`, the SDK
    /// uses the first principal account in the session.
    pub account_id: Option<String>,
    /// Soft cap on the attachment cache, in bytes. Default 256 MiB.
    pub attachment_cache_bytes: u64,
    /// Per-request HTTP timeout. Default 30s.
    pub request_timeout: Duration,
    /// Total retry budget per logical call. Default 60s.
    pub retry_budget: Duration,
    /// How many emails to fetch on the very first sync. Default 200.
    pub initial_sync_email_window: u32,
    /// Default mailbox to bootstrap (Inbox is the common case).
    pub bootstrap_mailbox_role: Option<String>,
}

impl ClientConfig {
    pub fn new(
        bff_url: impl Into<String>,
        bearer_token: impl Into<String>,
        database_path: PathBuf,
    ) -> Self {
        Self {
            bff_url: bff_url.into(),
            bearer_token: bearer_token.into(),
            database_path,
            account_id: None,
            attachment_cache_bytes: 256 * 1024 * 1024,
            request_timeout: Duration::from_secs(30),
            retry_budget: Duration::from_secs(60),
            initial_sync_email_window: 200,
            bootstrap_mailbox_role: Some("inbox".into()),
        }
    }

    /// Apply per-field optional overrides from a foreign-binding
    /// caller in-place. This is the canonical lowering ladder shared
    /// by the UniFFI (`kmail-ffi`) and napi (`kmail-napi`) bindings;
    /// both call this with the values their record type carries, so
    /// the two FFI surfaces cannot drift in their default-handling
    /// semantics. Add a new optional field here and update both
    /// `client_open` (UniFFI) and `KMailClientJs::open` (napi) to
    /// thread it through — the compiler will catch any forgotten
    /// call site because the parameter list grows.
    ///
    /// The two tiers of optionality are baked in:
    ///
    /// * **Tier 1 — numeric fields.** `None` means "inherit the
    ///   `ClientConfig::new` default", because the core type stores
    ///   the value as a non-optional primitive. The override only
    ///   fires on `Some(value)`. Used for `attachment_cache_bytes`,
    ///   `request_timeout_secs`, `retry_budget_secs`, and
    ///   `initial_sync_email_window`.
    /// * **Tier 2 — string fields.** `None` is a legitimate "no
    ///   value" because the core type already stores
    ///   `Option<String>`. Verbatim assignment — passing `None`
    ///   genuinely clears the field. Used for `account_id` and
    ///   `bootstrap_mailbox_role`.
    #[allow(clippy::too_many_arguments)]
    pub fn apply_optional_overrides(
        &mut self,
        attachment_cache_bytes: Option<u64>,
        request_timeout_secs: Option<u32>,
        retry_budget_secs: Option<u32>,
        initial_sync_email_window: Option<u32>,
        account_id: Option<String>,
        bootstrap_mailbox_role: Option<String>,
    ) {
        if let Some(b) = attachment_cache_bytes {
            self.attachment_cache_bytes = b;
        }
        if let Some(t) = request_timeout_secs {
            self.request_timeout = Duration::from_secs(u64::from(t));
        }
        if let Some(t) = retry_budget_secs {
            self.retry_budget = Duration::from_secs(u64::from(t));
        }
        if let Some(w) = initial_sync_email_window {
            self.initial_sync_email_window = w;
        }
        self.account_id = account_id;
        self.bootstrap_mailbox_role = bootstrap_mailbox_role;
    }
}

impl std::fmt::Debug for ClientConfig {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("ClientConfig")
            .field("bff_url", &self.bff_url)
            .field("bearer_token", &"<redacted>")
            .field("database_path", &self.database_path)
            .field("account_id", &self.account_id)
            .field("attachment_cache_bytes", &self.attachment_cache_bytes)
            .field("request_timeout", &self.request_timeout)
            .field("retry_budget", &self.retry_budget)
            .field("initial_sync_email_window", &self.initial_sync_email_window)
            .field("bootstrap_mailbox_role", &self.bootstrap_mailbox_role)
            .finish()
    }
}

/// Aggregated counts from a successful `sync()`.
///
/// The pending-action counters break out per-outcome so platform
/// shells can distinguish "queue is draining" from "queue is
/// permanently jammed on bad payloads". A purely successful batch
/// looks like `applied = N, failed = 0, deferred = 0`; a network
/// outage during flush surfaces as `applied = K, failed = 0,
/// deferred = N - K` (loop broke on the first retryable error,
/// remaining actions will be retried next sync); a corrupt payload
/// in the queue surfaces as `applied = K, failed = 1, deferred =
/// N - K - 1`. Conflating these into one "flushed" total — as the
/// initial implementation did — hides the third case from
/// observability, so a queue with three actions that all hit
/// `urn:ietf:params:jmap:error:invalidArguments` would silently
/// drain every sync with `flushed = 3` and no signal to the user
/// that their writes never landed.
#[derive(Clone, Debug, Default)]
pub struct SyncSummary {
    pub mailboxes_upserted: u64,
    pub mailboxes_destroyed: u64,
    pub emails_created: u64,
    pub emails_updated: u64,
    pub emails_destroyed: u64,
    /// Actions that committed successfully against the BFF.
    pub pending_actions_applied: u64,
    /// Actions that hit a terminal (non-retryable) error and were
    /// dropped from the queue.
    pub pending_actions_failed: u64,
    /// Actions left on the queue because the flush loop hit a
    /// retryable error (network blip, 5xx, etc.) and chose to bail
    /// rather than burn the retry budget on every remaining action.
    /// These will be retried on the next sync.
    pub pending_actions_deferred: u64,
}

/// Result of ingesting a transport-level push payload via
/// [`KMailClient::ingest_push_delivery`].
///
/// The shell uses this to drive two side effects: render
/// `notification` (when present) and, when `needs_delta_sync` is
/// set, kick a `sync()` so the local cache converges to the
/// canonical server state.
#[derive(Clone, Debug, Default)]
pub struct PushIngestOutcome {
    /// A ready-to-render notification, when the payload carried
    /// enough metadata to build one. `None` for a metadata-only
    /// `StateChange` push or a malformed payload.
    pub notification: Option<LocalNotification>,
    /// Whether a preview email row was written straight into the
    /// local cache from the push hint (instant inbox update, no
    /// network round-trip). The row is reconciled to the canonical
    /// `Email/get` shape on the next delta `sync()`.
    pub email_cached: bool,
    /// Whether the shell should run a delta `sync()` to converge.
    ///
    /// Always `true`: even when we cached the preview row, the push
    /// `email_state` token is a *snapshot identifier*, not a proven
    /// successor of our local `Email/changes` cursor — adopting it
    /// blindly could skip mutations that landed on other messages
    /// while the device was offline. The authoritative cursor only
    /// advances through the delta-pull path, so a follow-up sync is
    /// always required for correctness.
    pub needs_delta_sync: bool,
}

/// Internal per-outcome counts from a single
/// `flush_pending_actions` invocation. Promoted into
/// [`SyncSummary`] by `sync()`. Kept private so we can rename or
/// restructure without touching the public surface.
#[derive(Clone, Debug, Default)]
struct FlushOutcome {
    applied: u64,
    failed: u64,
    deferred: u64,
}

/// Handle to a running background sync worker.
///
/// Spawned by [`KMailClient::spawn_background_sync`]; the worker
/// calls `sync()` on a fixed cadence until [`stop`](Self::stop) is
/// invoked or the handle is dropped. The worker swallows
/// per-tick errors (logging them via `tracing`) so a transient
/// network failure doesn't kill the loop — a mail app that stops
/// syncing forever after one offline blip is worse than one that
/// retries on the next tick.
///
/// Drop semantics: dropping the handle signals cancellation (so a
/// shell that forgets to call `stop()` doesn't leak the task), but
/// does NOT block on the task finishing. Call
/// [`stop_and_join`](Self::stop_and_join) when you need to be sure
/// the in-flight `sync()` has fully wound down (e.g. before closing
/// the SQLite store on logout).
pub struct BackgroundSyncHandle {
    cancel: Arc<Notify>,
    join: Option<tokio::task::JoinHandle<()>>,
}

impl BackgroundSyncHandle {
    /// Signal the worker to stop after its current tick. Idempotent
    /// and non-blocking.
    ///
    /// Uses `notify_one`, not `notify_waiters`: `notify_one` stores a
    /// permit when the worker is not currently parked on
    /// `notified()` (e.g. it's mid-`sync()`), so the cancellation is
    /// observed on the worker's next `notified()` poll instead of
    /// being silently lost. `notify_waiters` has no such permit and
    /// would race — a stop signal sent while the worker is between
    /// selects would be dropped, hanging `stop_and_join` forever.
    pub fn stop(&self) {
        self.cancel.notify_one();
    }

    /// Signal cancellation and await the worker's clean shutdown.
    /// Returns once any in-flight `sync()` has returned and the
    /// task has exited.
    pub async fn stop_and_join(mut self) {
        self.cancel.notify_one();
        if let Some(join) = self.join.take() {
            // A `JoinError` here only means the task panicked or was
            // aborted; either way the worker is no longer running,
            // which is all `stop_and_join` promises.
            let _ = join.await;
        }
    }
}

impl Drop for BackgroundSyncHandle {
    fn drop(&mut self) {
        // Best-effort cancellation so a dropped handle doesn't leave
        // the periodic task running against a client whose store may
        // be about to close. We intentionally do NOT block on the
        // join here — `Drop` can run on any thread (including outside
        // a runtime), and blocking would risk a deadlock. Callers
        // that need synchronous teardown use `stop_and_join`.
        self.cancel.notify_one();
    }
}

/// Public SDK façade.
#[derive(Clone)]
pub struct KMailClient {
    config: ClientConfig,
    jmap: JmapClient,
    store: Store,
    mailbox_repo: MailboxRepo,
    email_repo: EmailRepo,
    state_repo: StateRepo,
    actions_repo: Arc<ActionsRepo>,
    cache: Arc<AttachmentCache>,
    /// Bridge into the KChat MLS SDK. Optional because the SDK can
    /// operate in Standard Private Mail mode (no MLS at all); only
    /// Confidential Send and Zero-Access Vault flows require it.
    ///
    /// `Arc<RwLock<...>>` rather than a plain `Arc<dyn ...>` because
    /// platform shells need to swap the provider at runtime — e.g.
    /// when the user signs out + back in with a different account,
    /// the MLS state changes and the old provider becomes stale.
    /// Calling [`KMailClient::set_mls_provider`] swaps the trait
    /// object behind every existing clone of the client without
    /// requiring a full close + reopen of the SQLite store.
    mls_provider: Arc<RwLock<Option<Arc<dyn MlsKeyProvider>>>>,
    /// Cached session resource — fetched on first `sync()` or
    /// `discover_session()` call.
    ///
    /// Held behind `Arc<RwLock<Option<_>>>` (rather than the simpler
    /// `OnceCell`) so [`invalidate_session`] can drop it. JMAP sessions
    /// are usually stable for the lifetime of an authenticated client,
    /// but they can change in three scenarios the SDK must be able to
    /// recover from without forcing the platform shell to close + reopen
    /// the client (which would re-run SQLite migrations and dump the
    /// attachment cache):
    ///   1. The user's tenant gets resharded — `apiUrl` rotates.
    ///   2. The user upgrades their plan — `accounts` / `capabilities`
    ///      grow new entries (Confidential Send, Vault).
    ///   3. The BFF returns a `Reauth-Required` 401 with a fresh
    ///      session document attached (future protocol extension).
    ///
    /// `set_bearer_token` deliberately does NOT auto-invalidate the
    /// session because OIDC refresh by itself never rotates `apiUrl` or
    /// account mapping. Platform shells call [`invalidate_session`]
    /// explicitly when they observe one of the above signals.
    session: Arc<RwLock<Option<JmapSession>>>,
    /// Memoised account ID. Resolved from config, or from the
    /// session's `primaryAccounts[urn:...:mail]` on first use.
    account_id: Arc<Mutex<Option<String>>>,
}

impl KMailClient {
    /// Open the SDK: run schema migrations, build the JMAP client,
    /// wire up the repos and the attachment cache.
    pub fn open(config: ClientConfig) -> Result<Self> {
        if config.bff_url.is_empty() {
            return Err(Error::InvalidArgument("bff_url is empty".into()));
        }
        if config.bearer_token.is_empty() {
            return Err(Error::InvalidArgument("bearer_token is empty".into()));
        }

        let store = Store::open(&config.database_path)?;
        let mailbox_repo = MailboxRepo::new(store.clone());
        let email_repo = EmailRepo::new(store.clone());
        let state_repo = StateRepo::new(store.clone());
        let actions_repo = Arc::new(ActionsRepo::new(store.clone()));
        let cache = Arc::new(AttachmentCache::new(
            store.clone(),
            config.attachment_cache_bytes,
        ));

        let mut transport = TransportConfig::new(&config.bff_url, &config.bearer_token);
        transport.request_timeout = config.request_timeout;
        transport.retry_budget = config.retry_budget;
        let jmap = JmapClient::new(transport)?;

        Ok(Self {
            config,
            jmap,
            store,
            mailbox_repo,
            email_repo,
            state_repo,
            actions_repo,
            cache,
            mls_provider: Arc::new(RwLock::new(None)),
            session: Arc::new(RwLock::new(None)),
            account_id: Arc::new(Mutex::new(None)),
        })
    }

    /// Plug a concrete [`MlsKeyProvider`] into the client.
    ///
    /// Until this is called, the Confidential Send and Vault
    /// convenience methods ([`encrypt_confidential_message`],
    /// [`decrypt_confidential_message`], [`write_vault_message`],
    /// [`open_vault_message`]) return [`Error::KeyStore`] because
    /// the SDK has no way to ask for the MLS exporter secrets they
    /// require.
    ///
    /// The lower-level entry points that accept raw key bytes
    /// directly ([`seal_vault_envelope`],
    /// [`decrypt_vault_envelope`], [`seal_confidential_envelope`],
    /// [`open_confidential_envelope`]) bypass this and remain
    /// usable without a provider — they're how the FFI / napi
    /// integration tests exercise the crypto without forcing the
    /// test scope to drag in a real MLS implementation.
    ///
    /// [`encrypt_confidential_message`]:
    ///     KMailClient::encrypt_confidential_message
    /// [`decrypt_confidential_message`]:
    ///     KMailClient::decrypt_confidential_message
    /// [`write_vault_message`]: KMailClient::write_vault_message
    /// [`open_vault_message`]: KMailClient::open_vault_message
    /// [`seal_vault_envelope`]: KMailClient::seal_vault_envelope
    /// [`decrypt_vault_envelope`]: KMailClient::decrypt_vault_envelope
    /// [`seal_confidential_envelope`]:
    ///     KMailClient::seal_confidential_envelope
    /// [`open_confidential_envelope`]:
    ///     KMailClient::open_confidential_envelope
    pub async fn set_mls_provider(&self, provider: Arc<dyn MlsKeyProvider>) {
        *self.mls_provider.write().await = Some(provider);
    }

    /// Drop the currently-plugged [`MlsKeyProvider`]. Subsequent
    /// Confidential Send / Vault calls that need it return
    /// `Error::KeyStore`. Useful on logout — destroys the in-process
    /// reference to whatever wrapped the MLS state so the platform
    /// shell's own logout teardown can proceed without dangling SDK
    /// references.
    pub async fn clear_mls_provider(&self) {
        *self.mls_provider.write().await = None;
    }

    /// Hot-swap the OIDC bearer token used for every future JMAP
    /// request, without rebuilding the SDK.
    ///
    /// OIDC access tokens typically expire after 5-60 minutes. The
    /// platform shell (iOS / Android / Electron) is responsible for
    /// running the refresh flow and pushing the new token down via
    /// this method. Refresh is observed by every existing clone of
    /// `KMailClient` — no reconnect, no in-flight request failure,
    /// and no need to close+reopen the SQLite store (which would
    /// otherwise mean re-running migrations and dropping the
    /// attachment cache).
    pub fn set_bearer_token(&self, token: impl Into<String>) -> Result<()> {
        self.jmap.set_bearer_token(token)
    }

    /// Fetch (or return the cached) JMAP session resource.
    ///
    /// Concurrency: we do a cheap read-lock probe first, then upgrade
    /// to a write lock and re-check before paying the HTTP cost so
    /// concurrent callers can't double-fetch the session. The HTTP
    /// request itself runs while we hold the write lock; that's fine
    /// here because (a) the session endpoint is small and fast, and (b)
    /// without holding the write lock through the HTTP call, two
    /// callers that both observe `None` would each fire their own GET,
    /// defeating the cache's purpose on the cold path.
    pub async fn discover_session(&self) -> Result<JmapSession> {
        if let Some(cached) = self.session.read().await.as_ref() {
            return Ok(cached.clone());
        }
        let mut guard = self.session.write().await;
        if let Some(cached) = guard.as_ref() {
            return Ok(cached.clone());
        }
        let fresh = self.jmap.session().await?;
        *guard = Some(fresh.clone());
        Ok(fresh)
    }

    /// Drop the cached JMAP session so the next `discover_session()`
    /// / `sync()` re-fetches it from `/jmap/session`.
    ///
    /// Platform shells should call this when they have a signal that
    /// the server-side session document has changed — e.g. after a
    /// 401 with `WWW-Authenticate: Reauth-Required`, after a plan
    /// upgrade webhook, or after a shard-migration push. Calling it on
    /// a never-fetched session is a no-op.
    pub async fn invalidate_session(&self) {
        let mut guard = self.session.write().await;
        *guard = None;
    }

    /// Account ID resolution policy:
    ///   1. `config.account_id` if explicitly set.
    ///   2. Memoised value from a previous call.
    ///   3. `session.primaryAccounts[urn:ietf:params:jmap:mail]`.
    ///   4. First key in `session.accounts`.
    pub async fn account_id(&self) -> Result<String> {
        if let Some(forced) = &self.config.account_id {
            return Ok(forced.clone());
        }
        {
            let g = self.account_id.lock().expect("account_id mutex poisoned");
            if let Some(v) = g.as_ref() {
                return Ok(v.clone());
            }
        }
        let session = self.discover_session().await?;
        let id = session
            .primary_accounts
            .get(crate::jmap::request::CAP_MAIL)
            .cloned()
            .or_else(|| session.accounts.keys().next().cloned())
            .ok_or_else(|| Error::Protocol("session exposes no accounts".into()))?;
        let mut g = self.account_id.lock().expect("account_id mutex poisoned");
        *g = Some(id.clone());
        Ok(id)
    }

    /// Delta-pull sync. See module-level doc-comment for the
    /// sequence of operations.
    ///
    /// `sync()` converges fully in one invocation — if the server
    /// returns `Email/changes` with `hasMoreChanges: true`, the
    /// loop iterates against the new state until all batches are
    /// drained. Bound by `MAX_EMAIL_CHANGES_BATCHES_PER_SYNC` as a
    /// safety valve against pathological servers that never settle.
    pub async fn sync(&self) -> Result<SyncSummary> {
        let session = self.discover_session().await?;
        let account_id = self.account_id().await?;
        let mut summary = SyncSummary::default();

        // 1. Mailboxes — incremental via Mailbox/changes if we
        //    already have a state token, full pull otherwise. The
        //    incremental path falls back to a full pull on
        //    SyncStateDiverged (mirroring the Email path) so a
        //    server that's evicted our cursor from its change log
        //    is handled by the same recovery semantics.
        self.sync_mailboxes(&session, &account_id, &mut summary)
            .await?;

        // 2. Emails — branch on whether we have a saved state
        //    token. The bootstrap path returns the canonical state
        //    captured atomically with the initial Email/query, so
        //    after persisting it we can skip the delta loop — an
        //    immediate `Email/changes` against a just-acquired
        //    state would always return empty and just waste a
        //    round-trip. Only the incremental path (saved_state
        //    present) drives the delta loop below.
        let saved_state = self.state_repo.get(SyncTypeName::Email)?;
        let mut current_state = match saved_state {
            Some(s) => s,
            None => {
                // First sync: atomic bootstrap (Email/query +
                // Email/get state probe in one JMAP request — see
                // `JmapClient::bootstrap_email_window`). Closes
                // the race window between query and state read.
                let (ids, state) = self
                    .bootstrap_initial_email_pull(&session, &account_id)
                    .await?;
                let hydrated = self
                    .fetch_email_mutations(&session, &account_id, &ids)
                    .await?;
                self.email_repo.apply_with_state(&hydrated, &state)?;
                summary.emails_created = ids.len() as u64;

                // 3. Flush queued offline actions and return.
                let flushed = self
                    .flush_pending_actions(&session, &account_id, 50)
                    .await?;
                summary.pending_actions_applied = flushed.applied;
                summary.pending_actions_failed = flushed.failed;
                summary.pending_actions_deferred = flushed.deferred;
                return Ok(summary);
            }
        };

        // 3. Delta loop — drain Email/changes until
        //    `has_more_changes == false`. Bounded so a buggy
        //    server can't spin us forever.
        let mut iterations = 0u32;
        loop {
            iterations += 1;
            if iterations > MAX_EMAIL_CHANGES_BATCHES_PER_SYNC {
                return Err(Error::Protocol(format!(
                    "Email/changes did not converge after {MAX_EMAIL_CHANGES_BATCHES_PER_SYNC} batches"
                )));
            }

            let changes = match self
                .jmap
                .email_changes(&session, &account_id, &current_state)
                .await
            {
                Ok(c) => c,
                Err(Error::SyncStateDiverged) => {
                    // Server can't catch us up — its change log has
                    // moved past our cursor. The local snapshot of
                    // emails is no longer trustworthy: any email
                    // destroyed on the server during the gap would
                    // never appear in a subsequent Email/changes
                    // (the next batch starts from a fresh state),
                    // so leaving the local row in place produces a
                    // ghost — visible in the UI, but referencing an
                    // ID the server has long since forgotten.
                    //
                    // The architecturally correct recovery is to
                    // discard the local snapshot wholesale and
                    // re-bootstrap from the canonical Email state
                    // returned by `bootstrap_email_window`'s atomic
                    // query+state probe. Older emails that fall
                    // outside the bootstrap window are re-fetched
                    // lazily when the user scrolls or searches via
                    // `query_emails_in_mailbox`; the alternative
                    // (selective reconciliation by walking the
                    // local cache against `Email/get` for every
                    // ID) would multiply round-trips by the cache
                    // size and still race against ongoing server
                    // mutation.
                    //
                    // `replace_all_with_state` performs the wipe +
                    // bootstrap-rehydrate + state-token commit in
                    // one SQLite transaction so observers can
                    // never see a half-purged cache.
                    let (ids, state) = self
                        .bootstrap_initial_email_pull(&session, &account_id)
                        .await?;
                    let hydrated = self
                        .fetch_email_mutations(&session, &account_id, &ids)
                        .await?;
                    let destroyed = self.email_repo.replace_all_with_state(&hydrated, &state)?;
                    summary.emails_destroyed += destroyed;
                    summary.emails_created += ids.len() as u64;
                    break;
                }
                Err(other) => return Err(other),
            };

            let created = changes.created;
            let updated = changes.updated;
            let destroyed = changes.destroyed;

            // Hydrate created + updated.
            let mut to_fetch = Vec::with_capacity(created.len() + updated.len());
            to_fetch.extend(created.iter().cloned());
            to_fetch.extend(updated.iter().cloned());
            let mut mutations = self
                .fetch_email_mutations(&session, &account_id, &to_fetch)
                .await?;

            // Append destroys to the same batch so the data
            // mutation + state-token commit happen in one
            // transaction — see `EmailRepo::apply_with_state`.
            for d in &destroyed {
                mutations.push(crate::sync::EmailMutation::Delete(d.clone()));
            }

            summary.emails_created += created.len() as u64;
            summary.emails_updated += updated.len() as u64;
            summary.emails_destroyed += destroyed.len() as u64;

            current_state = changes.new_state;
            self.email_repo
                .apply_with_state(&mutations, &current_state)?;

            if !changes.has_more_changes {
                break;
            }
        }

        // 4. Flush queued offline actions.
        let flushed = self
            .flush_pending_actions(&session, &account_id, 50)
            .await?;
        summary.pending_actions_applied = flushed.applied;
        summary.pending_actions_failed = flushed.failed;
        summary.pending_actions_deferred = flushed.deferred;

        Ok(summary)
    }

    /// Mailbox sync — incremental when we have a state token,
    /// full pull on first sync or when the server's
    /// `Mailbox/changes` returns `cannotCalculateChanges`.
    ///
    /// In both branches the data write and the state-token
    /// adoption happen in one SQLite transaction
    /// (`MailboxRepo::upsert_many_with_state`), so a crash in
    /// the middle can't leave the cache convinced it has
    /// observed a state cursor whose rows aren't actually in
    /// storage. That property is what makes the persisted
    /// `SyncTypeName::Mailbox` token a load-bearing input to
    /// the next sync — it really does describe what the local
    /// cache contains.
    async fn sync_mailboxes(
        &self,
        session: &JmapSession,
        account_id: &str,
        summary: &mut SyncSummary,
    ) -> Result<()> {
        if let Some(since) = self.state_repo.get(SyncTypeName::Mailbox)? {
            // Delta loop — drain `Mailbox/changes` until
            // `has_more_changes == false`. Mirrors the
            // `Email/changes` loop shape: bounded so a buggy
            // server can't spin us forever, and a
            // `SyncStateDiverged` raised at ANY iteration
            // (initial batch or continuation) falls through to
            // the full-pull below. Earlier revisions of this
            // function only issued one follow-up call, which
            // meant a server that paginated `Mailbox/changes`
            // across three or more batches would converge over
            // multiple user-visible `sync()` invocations; that
            // is no longer the case. They also propagated
            // `SyncStateDiverged` raised during the continuation
            // call, even though the same error on the *first*
            // call was handled by full-pull recovery — that
            // asymmetry would have left the local cache wedged
            // against a stale cursor whenever the server's
            // change-log eviction landed mid-pagination.
            let mut current = since;
            let mut iterations = 0u32;
            let diverged = loop {
                iterations += 1;
                if iterations > MAX_MAILBOX_CHANGES_BATCHES_PER_SYNC {
                    return Err(Error::Protocol(format!(
                        "Mailbox/changes did not converge after {MAX_MAILBOX_CHANGES_BATCHES_PER_SYNC} batches"
                    )));
                }

                let changes = match self
                    .jmap
                    .mailbox_changes(session, account_id, &current)
                    .await
                {
                    Ok(c) => c,
                    Err(Error::SyncStateDiverged) => break true,
                    Err(other) => return Err(other),
                };

                let mut ids_to_fetch =
                    Vec::with_capacity(changes.created.len() + changes.updated.len());
                ids_to_fetch.extend(changes.created.iter().cloned());
                ids_to_fetch.extend(changes.updated.iter().cloned());
                let mailboxes = if ids_to_fetch.is_empty() {
                    Vec::new()
                } else {
                    self.jmap
                        .get_mailboxes(session, account_id, &ids_to_fetch)
                        .await?
                        .mailboxes
                };
                self.mailbox_repo.upsert_many_with_state(
                    &mailboxes,
                    &changes.destroyed,
                    &changes.new_state,
                )?;
                summary.mailboxes_upserted += mailboxes.len() as u64;
                summary.mailboxes_destroyed += changes.destroyed.len() as u64;

                if !changes.has_more_changes {
                    break false;
                }
                current = changes.new_state;
            };
            if !diverged {
                return Ok(());
            }
            // Fall through to full-pull. The stale state token
            // is replaced when we co-commit the fresh one with
            // the full set, so there is no separate `forget`
            // call required.
        }

        // Full pull: first sync, or recovery from
        // `cannotCalculateChanges`. The whole `Mailbox/get`
        // response plus the state token commit atomically.
        let mailboxes = self.jmap.list_mailboxes(session, account_id).await?;
        // Compute the IDs of locally-cached mailboxes that the
        // server no longer reports, so the full-pull path also
        // reconciles destroys.
        let existing: std::collections::HashSet<String> = self
            .mailbox_repo
            .list()?
            .into_iter()
            .map(|m| m.id)
            .collect();
        let server_ids: std::collections::HashSet<String> =
            mailboxes.mailboxes.iter().map(|m| m.id.clone()).collect();
        let destroyed: Vec<String> = existing.difference(&server_ids).cloned().collect();
        self.mailbox_repo.upsert_many_with_state(
            &mailboxes.mailboxes,
            &destroyed,
            &mailboxes.state,
        )?;
        summary.mailboxes_upserted += mailboxes.mailboxes.len() as u64;
        summary.mailboxes_destroyed += destroyed.len() as u64;
        Ok(())
    }

    /// Fetch `Email/get` for the given IDs in chunks of 100
    /// (RFC 8620 §6.4 recommended ceiling) and collect the upsert
    /// mutations.
    ///
    /// Returns the mutations *without* applying them — the caller
    /// is expected to commit the result alongside a JMAP state
    /// token via `EmailRepo::apply_with_state` /
    /// `EmailRepo::replace_all_with_state`, so the row write and
    /// the state-token adoption happen in one SQLite transaction.
    /// This decouples the network round-trip from the persistence
    /// commit and is what closes the crash-window where rows would
    /// land in storage but the state token wouldn't (the next
    /// sync would then re-deliver those IDs, double-counting in
    /// the summary and racing against concurrent server mutations).
    async fn fetch_email_mutations(
        &self,
        session: &JmapSession,
        account_id: &str,
        ids: &[String],
    ) -> Result<Vec<crate::sync::EmailMutation>> {
        if ids.is_empty() {
            return Ok(Vec::new());
        }
        let mut out = Vec::with_capacity(ids.len());
        for chunk in ids.chunks(100) {
            let emails = self
                .jmap
                .get_emails(session, account_id, chunk, /* with_bodies */ false)
                .await?;
            out.extend(
                emails
                    .into_iter()
                    .map(|e| crate::sync::EmailMutation::Upsert(Box::new(e.summary))),
            );
        }
        Ok(out)
    }

    /// First-time email pull. Locates the bootstrap mailbox (by
    /// canonical role name — exact-match, never substring) and
    /// issues a single atomic JMAP request that combines
    /// `Email/query` with an `Email/get ids: []` state probe.
    /// RFC 8620 §3.4's same-request atomicity guarantee closes the
    /// race window where an email arriving between query and state
    /// read would be permanently missed from the local cache.
    ///
    /// Returns `(ids_to_hydrate, canonical_email_state)`. The
    /// caller is responsible for hydrating the IDs via
    /// `Email/get` (see `hydrate_email_ids`) and persisting the
    /// state token.
    async fn bootstrap_initial_email_pull(
        &self,
        session: &JmapSession,
        account_id: &str,
    ) -> Result<(Vec<String>, String)> {
        let inbox_role_raw = self
            .config
            .bootstrap_mailbox_role
            .as_deref()
            .unwrap_or("inbox");
        let inbox_role = crate::models::MailboxRole::from_canonical_name(inbox_role_raw)
            .ok_or_else(|| {
                Error::InvalidArgument(format!(
                    "bootstrap_mailbox_role={inbox_role_raw:?} is not a known MailboxRole"
                ))
            })?;
        let inbox = self
            .mailbox_repo
            .list()?
            .into_iter()
            .find(|m| m.role.as_ref() == Some(&inbox_role))
            .ok_or_else(|| {
                Error::Protocol(format!(
                    "no mailbox with role={role} after Mailbox/get",
                    role = inbox_role.canonical_name()
                ))
            })?;

        self.jmap
            .bootstrap_email_window(
                session,
                account_id,
                &inbox.id,
                self.config.initial_sync_email_window,
            )
            .await
    }

    /// Return cached mailboxes from the local store. Does NOT make
    /// a network call; callers wanting fresh data should `sync()`
    /// first.
    pub fn cached_mailboxes(&self) -> Result<Vec<Mailbox>> {
        self.mailbox_repo.list()
    }

    /// Return cached emails in a mailbox, newest first.
    pub fn cached_emails_in_mailbox(
        &self,
        mailbox_id: &str,
        limit: u32,
    ) -> Result<Vec<EmailSummary>> {
        self.email_repo.list_in_mailbox(mailbox_id, limit)
    }

    /// Fetch a full email by ID. Hits the local cache first; on a
    /// miss (or when bodies aren't cached) issues an `Email/get`
    /// with body values requested.
    pub async fn fetch_email(&self, id: &str, with_bodies: bool) -> Result<Email> {
        let session = self.discover_session().await?;
        let account_id = self.account_id().await?;
        let mut emails = self
            .jmap
            .get_emails(&session, &account_id, &[id.to_string()], with_bodies)
            .await?;
        emails
            .pop()
            .ok_or_else(|| Error::NotFound(format!("email {id} not found")))
    }

    /// Queue an offline keyword toggle. `keywords` is a JSON object
    /// of `{keyword_name: bool | null}` pairs:
    ///   * `true`  — set the keyword (e.g. mark `$seen`).
    ///   * `false` — clear the keyword (rarely useful — most callers
    ///     should send `null` for "remove").
    ///   * `null`  — remove the keyword entirely.
    ///
    /// Per JMAP / RFC 8620 §3.3 PatchObject semantics, each entry is
    /// serialised as a `keywords/<name>` path patch so the server
    /// merges it into the existing keyword set instead of replacing
    /// the whole property. A whole-property `{"keywords": {...}}`
    /// update would silently drop any keyword not present in
    /// `keywords` (e.g. `$flagged`, `$forwarded`, custom labels) —
    /// see `RFC 8620 §3.3` and `RFC 8621 §4.1.2`.
    ///
    /// The next `sync()` drains the queue against the BFF.
    pub fn enqueue_set_keywords(&self, email_id: &str, keywords: &serde_json::Value) -> Result<()> {
        let map = keywords.as_object().ok_or_else(|| {
            Error::InvalidArgument(
                "keywords must be a JSON object of {keyword: bool|null} pairs".into(),
            )
        })?;
        if map.is_empty() {
            return Err(Error::InvalidArgument(
                "keywords map is empty; nothing to patch".into(),
            ));
        }
        let mut patch = serde_json::Map::with_capacity(map.len());
        for (k, v) in map {
            // JMAP path components separate on `/` and escape `~`
            // (RFC 8620 §3.1.2). Reject keyword names that would
            // collide with the path grammar instead of silently
            // producing a malformed PatchObject the BFF would reject.
            if k.is_empty() || k.contains('/') || k.contains('~') {
                return Err(Error::InvalidArgument(format!(
                    "keyword name {k:?} contains a reserved JMAP path character"
                )));
            }
            if !v.is_boolean() && !v.is_null() {
                return Err(Error::InvalidArgument(format!(
                    "keyword {k:?} value must be true (set), false (clear), or null (remove); got {v}"
                )));
            }
            patch.insert(format!("keywords/{k}"), v.clone());
        }
        self.actions_repo
            .enqueue(
                PendingActionKind::SetKeywords,
                email_id,
                &serde_json::Value::Object(patch),
            )
            .map(|_| ())
    }

    /// Send an email. The draft is dispatched via `Email/set` +
    /// `EmailSubmission/set`. Returns the server-assigned email ID.
    pub async fn send_email(&self, draft: &EmailDraft) -> Result<String> {
        let session = self.discover_session().await?;
        let account_id = self.account_id().await?;
        self.jmap.send_email(&session, &account_id, draft).await
    }

    /// Register a push token with the BFF. The transport-specific
    /// payload shape mirrors `cmd/kmail-api/main.go` lines 698-748.
    ///
    /// Routes through the live `JmapClient::post_json`, which reuses
    /// the same `Arc<RwLock<String>>`-backed bearer token as every
    /// other JMAP request. That means a `set_bearer_token` refresh is
    /// observed here too — building a fresh `JmapTransport` from
    /// `self.config.bearer_token` would have ossified the original
    /// token captured at `open()` time and produced a 401 the moment
    /// the OIDC access token rotated (typically every 5–60 minutes).
    pub async fn register_push_token(
        &self,
        transport: PushTransport,
        token: &str,
        web_push_keys: Option<WebPushKeys>,
    ) -> Result<()> {
        // The wire `PushSubscriptionRequest` matches the BFF's
        // flat `Subscription` struct (internal/push/push.go:47-56),
        // so flatten the ergonomic `WebPushKeys { p256dh, auth }`
        // input into top-level `p256dh_key` / `auth_key` here.
        let (p256dh_key, auth_key) = match web_push_keys {
            Some(k) => (Some(k.p256dh), Some(k.auth)),
            None => (None, None),
        };
        let req = PushSubscriptionRequest {
            transport,
            token: token.to_string(),
            p256dh_key,
            auth_key,
        };
        let resp: serde_json::Value = self.jmap.post_json("/api/v1/push/subscribe", &req).await?;
        // BFF returns `{"id": "...", "transport": "..."}` on success;
        // we don't surface the subscription ID today — the SDK
        // re-registers on every reauth, so storing the ID is the
        // platform shell's choice.
        let _ = resp;
        Ok(())
    }

    /// Ingest a transport-level push `data` map (the APNs
    /// notification `data` dictionary / FCM `data` map / Web Push
    /// JSON object, already flattened to `String → String` by the
    /// platform shell).
    ///
    /// This is the single entry point a shell calls from its push
    /// handler. It does two things:
    ///   1. If the payload is a `new_email` delivery hint (has at
    ///      least `email_id`), it writes a *preview* email row into
    ///      the local cache so the inbox updates instantly, and
    ///      builds a [`LocalNotification`] for the shell to render.
    ///   2. It always asks the shell to run a delta `sync()` (via
    ///      `needs_delta_sync`) so the cache converges to the
    ///      canonical server state — see [`PushIngestOutcome`] for
    ///      why the push token alone is not a safe sync cursor.
    ///
    /// A malformed or metadata-only (`StateChange`) payload returns
    /// `notification: None, email_cached: false, needs_delta_sync:
    /// true` — the shell silently falls back to a full sync rather
    /// than guessing.
    pub fn ingest_push_delivery(&self, data: &BTreeMap<String, String>) -> Result<PushIngestOutcome> {
        let Some(hint) = EmailDeliveryHint::from_data(data) else {
            return Ok(PushIngestOutcome {
                notification: None,
                email_cached: false,
                needs_delta_sync: true,
            });
        };
        let notification = LocalNotification::from_email_delivery(&hint);
        let email_cached = self.cache_delivery_hint(&hint)?;
        Ok(PushIngestOutcome {
            notification,
            email_cached,
            needs_delta_sync: true,
        })
    }

    /// Write a preview email row from a push delivery hint.
    ///
    /// The hint is a *projection* — it carries enough to render an
    /// inbox row and a notification, but not the full header set. We
    /// upsert it so the row appears immediately; the next delta
    /// `sync()` (`Email/get`) overwrites it with the canonical,
    /// fully-hydrated row. The upsert is idempotent, so that later
    /// reconciliation is free of duplicates.
    ///
    /// Crucially this does NOT advance the `Email` state token. The
    /// `email_state` carried in the push is a snapshot identifier,
    /// not a guaranteed successor of our local cursor; adopting it
    /// here would let the next `Email/changes` start from the wrong
    /// place and skip mutations on other messages. Correctness is
    /// delegated to the delta-pull path; this method only buys an
    /// instant first paint.
    ///
    /// Returns `Ok(false)` when the hint has no email id (nothing to
    /// cache).
    fn cache_delivery_hint(&self, hint: &EmailDeliveryHint) -> Result<bool> {
        let Some(email_id) = hint.email_id.clone().filter(|s| !s.is_empty()) else {
            return Ok(false);
        };

        let mut mailbox_ids = BTreeMap::new();
        if let Some(mbox) = hint.mailbox_id.clone().filter(|s| !s.is_empty()) {
            // Only attach mailbox membership when the mailbox is
            // already cached. `email_mailboxes.mailbox_id` carries a
            // foreign key into `mailboxes` (ON DELETE CASCADE), so
            // inserting a membership row for a mailbox the device
            // hasn't synced yet — a brand-new server-side label, or
            // the very first push before any `Mailbox/get` — would
            // fail the whole upsert and lose the notification too. In
            // that case we still cache the email headers; the next
            // delta `sync()` pulls the mailbox and links membership.
            if self.mailbox_repo.get(&mbox)?.is_some() {
                mailbox_ids.insert(mbox, true);
            }
        }
        let keywords = hint.keywords.iter().map(|k| (k.clone(), true)).collect();
        let received_at = hint
            .received_at_unix
            .and_then(|ts| DateTime::<Utc>::from_timestamp(ts, 0))
            .unwrap_or_else(|| DateTime::<Utc>::from_timestamp(0, 0).expect("epoch in range"));
        let from = match hint.from.as_deref().map(str::trim).filter(|s| !s.is_empty()) {
            Some(display) => vec![parse_address_display(display)],
            None => Vec::new(),
        };

        let summary = EmailSummary {
            id: email_id,
            blob_id: String::new(),
            thread_id: hint.thread_id.clone().unwrap_or_default(),
            mailbox_ids,
            keywords,
            size: 0,
            received_at,
            sent_at: None,
            from,
            to: Vec::new(),
            cc: Vec::new(),
            bcc: Vec::new(),
            reply_to: Vec::new(),
            subject: hint.subject.clone().unwrap_or_default(),
            preview: hint.snippet.clone().unwrap_or_default(),
            has_attachment: hint.has_attachment.unwrap_or(false),
        };
        self.email_repo
            .apply(&[EmailMutation::Upsert(Box::new(summary))])?;
        Ok(true)
    }

    /// Spawn a background worker that calls [`sync()`](Self::sync) on
    /// a fixed `interval` until the returned handle is stopped or
    /// dropped.
    ///
    /// The first sync fires after `interval` (not immediately) — the
    /// shell is expected to drive the initial foreground sync itself;
    /// this worker is for steady-state delta pulls while the app is
    /// open. Errors are logged and swallowed so a transient failure
    /// doesn't tear down the loop.
    ///
    /// Must be called from within a Tokio runtime context (the FFI /
    /// napi layers provide one).
    pub fn spawn_background_sync(&self, interval: Duration) -> BackgroundSyncHandle {
        let client = self.clone();
        spawn_periodic(interval, move || {
            let client = client.clone();
            async move {
                if let Err(e) = client.sync().await {
                    tracing::warn!(error = %e, "background sync tick failed; will retry next interval");
                }
            }
        })
    }
}

/// Spawn a task that runs `tick` every `interval`, cancellable via
/// the returned handle. Factored out of [`KMailClient::spawn_background_sync`]
/// so the loop / cancellation semantics can be unit-tested without a
/// live network or SQLite store.
fn spawn_periodic<F, Fut>(interval: Duration, mut tick: F) -> BackgroundSyncHandle
where
    F: FnMut() -> Fut + Send + 'static,
    Fut: Future<Output = ()> + Send,
{
    let cancel = Arc::new(Notify::new());
    let worker_cancel = cancel.clone();
    let join = tokio::spawn(async move {
        let mut ticker = tokio::time::interval(interval);
        // If a `tick` runs longer than `interval`, don't try to
        // "catch up" by firing back-to-back — skip the missed ticks
        // and resume the cadence. Bursting would hammer the BFF after
        // a slow sync, which is exactly when we want to back off.
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        // Consume the immediate first tick so the first real sync
        // fires after `interval`, not at t=0.
        ticker.tick().await;
        loop {
            tokio::select! {
                // Bias toward cancellation so a stop signal that
                // arrives at the same time as a tick wins, and we
                // don't start one more sync after being told to stop.
                biased;
                () = worker_cancel.notified() => break,
                _ = ticker.tick() => tick().await,
            }
        }
    });
    BackgroundSyncHandle {
        cancel,
        join: Some(join),
    }
}

/// Parse an RFC 5322-ish address display string
/// (`"Alice Example <alice@example.com>"`, a bare address, or a
/// bare display name) into a structured [`EmailAddress`]. Used to
/// give push-preview rows a best-effort sender column until the
/// next sync hydrates the canonical address.
fn parse_address_display(s: &str) -> EmailAddress {
    let s = s.trim();
    if let (Some(lt), Some(gt)) = (s.rfind('<'), s.rfind('>')) {
        if lt < gt {
            let email = s[lt + 1..gt].trim().to_string();
            let name = s[..lt].trim().trim_matches('"').trim().to_string();
            return EmailAddress { name, email };
        }
    }
    if s.contains('@') {
        EmailAddress {
            name: String::new(),
            email: s.to_string(),
        }
    } else {
        EmailAddress {
            name: s.to_string(),
            email: String::new(),
        }
    }
}

impl KMailClient {
    /// One-shot bootstrap sync via the BFF's
    /// `POST /api/v1/sync/bootstrap` endpoint.
    ///
    /// Replaces the cold-start path of [`sync()`] (JMAP session
    /// discovery → `Mailbox/get` → atomic `Email/query`+`Email/get`
    /// → bulk hydration) with a single SDK-side HTTP round-trip.
    /// The BFF composes the same JMAP request internally and
    /// returns a flat envelope that the SDK persists in one
    /// transaction per type, so the local SQLite snapshot ends in
    /// exactly the same shape as a normal first-launch `sync()`
    /// would — minus two extra BFF↔Stalwart round-trips and the
    /// session-discovery hop.
    ///
    /// Use this on:
    ///   * First launch / device-restore (no local DB yet).
    ///   * Recovery after a long offline gap when the saved state
    ///     token is known to be stale.
    ///
    /// Steady-state delta syncs should keep using [`sync()`].
    ///
    /// The optional `mailbox_role` restricts the email window to
    /// the named mailbox (`"inbox"`, `"sent"`, ...). When `None`,
    /// the window is account-wide newest-first. The optional
    /// `limit` caps the returned email count; see
    /// `internal/sync/sync.go::MaxBootstrapLimit` for the BFF cap.
    pub async fn bootstrap_sync(
        &self,
        mailbox_role: Option<&str>,
        limit: Option<u32>,
    ) -> Result<SyncSummary> {
        let req = BootstrapRequest {
            limit: limit.unwrap_or(0),
            mailbox_role: mailbox_role.unwrap_or("").to_string(),
        };
        let resp: BootstrapResponse = self.jmap.post_json("/api/v1/sync/bootstrap", &req).await?;

        // Adopt the BFF-resolved account ID eagerly. The normal
        // `account_id()` path resolves it via the JMAP session
        // document, which would force a separate `GET /jmap/session`
        // round-trip on the cold-start path — defeating the point
        // of this endpoint. Caching it here means subsequent calls
        // within the session reuse the value without going back
        // out over the network.
        if let Ok(mut guard) = self.account_id.lock() {
            if guard.is_none() {
                *guard = Some(resp.account_id.clone());
            }
        }

        // Deserialize the JSON-typed mailboxes / emails into the
        // SDK's typed models. The BFF passes the JMAP wire shapes
        // through verbatim, so the same Serde deserialisers used
        // by the JMAP path apply here.
        let mut mailboxes: Vec<Mailbox> = Vec::with_capacity(resp.mailboxes.len());
        for raw in resp.mailboxes {
            let mb: Mailbox = serde_json::from_value(raw)
                .map_err(|e| Error::Protocol(format!("bootstrap: deserialise mailbox: {e}")))?;
            mailboxes.push(mb);
        }

        let mut email_mutations: Vec<EmailMutation> = Vec::with_capacity(resp.emails.len());
        for raw in resp.emails {
            let summary: EmailSummary = serde_json::from_value(raw)
                .map_err(|e| Error::Protocol(format!("bootstrap: deserialise email: {e}")))?;
            email_mutations.push(EmailMutation::Upsert(Box::new(summary)));
        }

        let mut summary = SyncSummary::default();
        // Mailboxes — the bootstrap response is by definition a
        // complete snapshot, so `upsert_many_with_state` with no
        // destroys is the correct write (we don't have a
        // `replace_all_with_state` on `MailboxRepo` because
        // mailbox deletions are rare and the upsert path already
        // covers the steady-state delta semantics).
        self.mailbox_repo
            .upsert_many_with_state(&mailboxes, &[], &resp.mailbox_state)?;
        summary.mailboxes_upserted = mailboxes.len() as u64;

        // Emails — the response is the canonical window, so stale
        // local rows outside it must be evicted. The
        // `replace_all_with_state` path co-commits the wipe +
        // upserts + state token in one SQLite transaction so
        // observers never see a half-purged cache. The repo
        // returns the *destroyed* row count (rows that existed
        // before the wipe); the *created* count is the length of
        // the mutation batch — same accounting as the
        // `SyncStateDiverged` branch of `sync()` at
        // `client.rs:528-530`.
        let destroyed = self
            .email_repo
            .replace_all_with_state(&email_mutations, &resp.email_state)?;
        summary.emails_destroyed = destroyed;
        summary.emails_created = email_mutations.len() as u64;

        Ok(summary)
    }

    /// Seal `plaintext` into a Zero-Access Vault envelope under
    /// a caller-supplied per-folder master key.
    ///
    /// Symmetric inverse of [`decrypt_vault_envelope`]. The body
    /// of the construction lives in [`crate::crypto::vault::seal`]
    /// — this method is a thin pass-through that exists so the
    /// FFI / napi bindings have a single entry point to expose
    /// on the `KMailClient` handle.
    ///
    /// The seal samples a fresh 12-byte nonce per call from
    /// `OsRng`. Callers MUST NOT pre-generate the nonce; doing so
    /// would defeat the construction's safety argument (see
    /// `crypto::vault` module header).
    pub fn seal_vault_envelope(
        &self,
        folder_master_key: &[u8],
        plaintext: &[u8],
        aad: &[u8],
    ) -> Result<AeadEnvelope> {
        crypto::vault::seal(folder_master_key, plaintext, aad)
    }

    /// Decrypt a Zero-Access Vault envelope.
    ///
    /// The flow is:
    ///   1. Caller supplies the per-folder master secret (32 bytes,
    ///      derived from MLS by the platform shell + KChat MLS SDK).
    ///   2. The SDK derives a per-message AES key via HKDF with the
    ///      Vault label, salted with the envelope's nonce so each
    ///      message uses a fresh key.
    ///   3. AES-256-GCM authenticated decryption with caller-supplied
    ///      AAD (BFF wraps Email ID + Mailbox ID + epoch as AAD per
    ///      docs/ARCHITECTURE.md §5).
    ///
    /// Symmetric inverse of [`seal_vault_envelope`]. The body of
    /// the construction lives in [`crate::crypto::vault::open`].
    pub fn decrypt_vault_envelope(
        &self,
        folder_master_key: &[u8],
        envelope: &AeadEnvelope,
    ) -> Result<Vec<u8>> {
        crypto::vault::open(folder_master_key, envelope)
    }

    /// Seal `plaintext` into a Confidential Send envelope under
    /// a caller-supplied MLS leaf secret (32 bytes).
    ///
    /// Symmetric inverse of [`open_confidential_envelope`]. The
    /// body of the two-layer construction (random DEK + KEK wrap)
    /// lives in [`crate::crypto::confidential::seal`].
    pub fn seal_confidential_envelope(
        &self,
        mls_leaf_secret: &[u8],
        plaintext: &[u8],
        payload_aad: &[u8],
        wrap_aad: &[u8],
    ) -> Result<ConfidentialEnvelope> {
        crypto::confidential::seal(mls_leaf_secret, plaintext, payload_aad, wrap_aad)
    }

    /// Open a Confidential Send envelope under a caller-supplied
    /// MLS leaf secret. Symmetric inverse of
    /// [`seal_confidential_envelope`].
    pub fn open_confidential_envelope(
        &self,
        mls_leaf_secret: &[u8],
        envelope: &ConfidentialEnvelope,
    ) -> Result<Vec<u8>> {
        crypto::confidential::open(mls_leaf_secret, envelope)
    }

    /// Convenience: seal a Vault message by asking the plugged
    /// [`MlsKeyProvider`] for the per-folder master key.
    ///
    /// Returns `Error::KeyStore` if no provider has been plugged
    /// in via [`set_mls_provider`], or if the provider does not
    /// have a secret seeded for the supplied folder.
    ///
    /// The lifecycle is: take a read lock on the provider slot →
    /// extract the trait object → release the lock → invoke the
    /// trait method outside the lock. Releasing the lock before
    /// the trait call is important because `MlsKeyProvider` impls
    /// on iOS / Android may need to talk to the platform keychain
    /// (which can itself await on user authentication via Face ID
    /// / fingerprint), and holding the SDK's `RwLock` across that
    /// await would block every concurrent caller for the duration
    /// of the user gesture.
    pub async fn write_vault_message(
        &self,
        folder_id: &str,
        plaintext: &[u8],
        aad: &[u8],
    ) -> Result<AeadEnvelope> {
        let provider = self.mls_provider_or_err().await?;
        let secret = provider.vault_folder_master_secret(folder_id)?;
        crypto::vault::seal(secret.as_slice(), plaintext, aad)
    }

    /// Convenience: open a Vault message by asking the plugged
    /// [`MlsKeyProvider`] for the per-folder master key.
    ///
    /// Returns `Error::KeyStore` if no provider has been plugged
    /// in, or if the provider does not have a secret seeded for
    /// the supplied folder.
    pub async fn open_vault_message(
        &self,
        folder_id: &str,
        envelope: &AeadEnvelope,
    ) -> Result<Vec<u8>> {
        let provider = self.mls_provider_or_err().await?;
        let secret = provider.vault_folder_master_secret(folder_id)?;
        crypto::vault::open(secret.as_slice(), envelope)
    }

    /// Convenience: seal a Confidential Send message by asking
    /// the plugged [`MlsKeyProvider`] for the per-recipient leaf
    /// secret.
    ///
    /// Same `Error::KeyStore` semantics as
    /// [`write_vault_message`].
    pub async fn encrypt_confidential_message(
        &self,
        recipient_user_id: &str,
        plaintext: &[u8],
        payload_aad: &[u8],
        wrap_aad: &[u8],
    ) -> Result<ConfidentialEnvelope> {
        let provider = self.mls_provider_or_err().await?;
        let secret = provider.confidential_send_leaf_secret(recipient_user_id)?;
        crypto::confidential::seal(secret.as_slice(), plaintext, payload_aad, wrap_aad)
    }

    /// Convenience: open a Confidential Send message by asking
    /// the plugged [`MlsKeyProvider`] for the per-recipient leaf
    /// secret.
    pub async fn decrypt_confidential_message(
        &self,
        recipient_user_id: &str,
        envelope: &ConfidentialEnvelope,
    ) -> Result<Vec<u8>> {
        let provider = self.mls_provider_or_err().await?;
        let secret = provider.confidential_send_leaf_secret(recipient_user_id)?;
        crypto::confidential::open(secret.as_slice(), envelope)
    }

    /// Read the currently-plugged [`MlsKeyProvider`] or surface a
    /// uniform `Error::KeyStore` so callers don't have to branch
    /// on `Option<_>` themselves.
    async fn mls_provider_or_err(&self) -> Result<Arc<dyn MlsKeyProvider>> {
        let g = self.mls_provider.read().await;
        g.as_ref()
            .cloned()
            .ok_or_else(|| Error::KeyStore("no MlsKeyProvider plugged in".into()))
    }

    /// Drain up to `limit` queued actions against the BFF.
    ///
    /// Reports per-outcome counts so callers (sync summary,
    /// telemetry) can distinguish "draining cleanly" from
    /// "permanently jammed on bad payloads" from "network blip".
    /// See [`SyncSummary`] for the full semantics.
    async fn flush_pending_actions(
        &self,
        session: &JmapSession,
        account_id: &str,
        limit: u32,
    ) -> Result<FlushOutcome> {
        let batch = self.actions_repo.next_batch(limit)?;
        let total = batch.len() as u64;
        let mut outcome = FlushOutcome::default();
        for action in batch {
            // Circuit breaker: every retryable failure bumps
            // `attempts`, so if a 5xx (or any other retryable
            // error) keeps coming back we eventually treat the
            // action as terminally stuck and drop it from the
            // queue. Without this guard a permanently
            // misconfigured BFF (or a single malformed payload
            // the server somehow returns 503 for) would let the
            // queue accumulate the same action forever, and the
            // `attempts` column would just keep counting.
            //
            // The decision is taken BEFORE the network call so
            // we don't burn another retry just to discover the
            // same failure. The action is then `complete()`d
            // with a synthesised `last_error` describing why
            // the queue gave up, and counted under `failed` so
            // the platform shell can surface a "we couldn't
            // send N messages" banner.
            if action.attempts >= MAX_PENDING_ACTION_ATTEMPTS {
                self.actions_repo.record_failure(
                    action.id,
                    &format!(
                        "exceeded {MAX_PENDING_ACTION_ATTEMPTS} retry attempts; \
                         last error was: {}",
                        action.last_error.as_deref().unwrap_or("<none>")
                    ),
                )?;
                self.actions_repo.complete(action.id)?;
                outcome.failed += 1;
                continue;
            }

            match self
                .apply_pending_action(session, account_id, &action)
                .await
            {
                Ok(()) => {
                    self.actions_repo.complete(action.id)?;
                    outcome.applied += 1;
                }
                Err(e) if e.is_retryable() => {
                    self.actions_repo
                        .record_failure(action.id, &e.to_string())?;
                    // Stop draining — the network's flapping; the
                    // remaining actions (this one included) will
                    // retry on the next sync. Account for everything
                    // not yet processed as deferred so callers see
                    // an accurate queue-state snapshot.
                    //
                    // `applied + failed` is the count of items the
                    // loop has already settled — those are off
                    // the queue and must NOT be counted as
                    // deferred. Everything else (the current
                    // action that just bailed, plus the items not
                    // yet looped) stays on the queue for retry;
                    // that's exactly `total - (applied + failed)`.
                    // We express the count via the outcome
                    // counters rather than a separate `processed`
                    // index because the counters are the
                    // load-bearing invariant the caller observes
                    // (`applied + failed + deferred == total`)
                    // and computing `deferred` from them removes
                    // the cross-coupling between counter
                    // increment position and the subtraction.
                    // `saturating_sub` is defensive only — the
                    // subtrahend is bounded above by `total`
                    // since each iteration increments at most one
                    // counter and we haven't iterated more than
                    // `total` times.
                    outcome.deferred = total.saturating_sub(outcome.applied + outcome.failed);
                    return Ok(outcome);
                }
                Err(e) => {
                    // Terminal failure (4xx-equivalent). Record the
                    // error and drop the action so the queue
                    // doesn't wedge forever.
                    self.actions_repo
                        .record_failure(action.id, &e.to_string())?;
                    self.actions_repo.complete(action.id)?;
                    outcome.failed += 1;
                }
            }
        }
        Ok(outcome)
    }

    async fn apply_pending_action(
        &self,
        session: &JmapSession,
        account_id: &str,
        action: &PendingAction,
    ) -> Result<()> {
        match action.kind {
            PendingActionKind::SetKeywords | PendingActionKind::MoveEmail => {
                let mut req = crate::jmap::request::JmapRequest::new(vec![
                    crate::jmap::request::CAP_CORE.into(),
                    crate::jmap::request::CAP_MAIL.into(),
                ]);
                let mut update = serde_json::Map::new();
                update.insert(action.target_id.clone(), action.payload.clone());
                let id = req.call(
                    "Email/set",
                    serde_json::json!({
                        "accountId": account_id,
                        "update": serde_json::Value::Object(update),
                    }),
                );
                let resp = self.jmap.dispatch(session, &req).await?;
                let r: serde_json::Value = resp.parse(&id)?;
                // `notUpdated` carrying our target ID is a terminal
                // error; surface it.
                if let Some(not_updated) = r
                    .get("notUpdated")
                    .and_then(|v| v.as_object())
                    .and_then(|o| o.get(&action.target_id))
                {
                    let code = not_updated
                        .get("type")
                        .and_then(|v| v.as_str())
                        .unwrap_or("urn:ietf:params:jmap:error:serverFail")
                        .to_string();
                    return Err(Error::JmapMethod {
                        code,
                        description: not_updated
                            .get("description")
                            .and_then(|v| v.as_str())
                            .unwrap_or_default()
                            .to_string(),
                    });
                }
                Ok(())
            }
            PendingActionKind::DeleteEmail => {
                let mut req = crate::jmap::request::JmapRequest::new(vec![
                    crate::jmap::request::CAP_CORE.into(),
                    crate::jmap::request::CAP_MAIL.into(),
                ]);
                let id = req.call(
                    "Email/set",
                    serde_json::json!({
                        "accountId": account_id,
                        "destroy": [action.target_id.clone()],
                    }),
                );
                let resp = self.jmap.dispatch(session, &req).await?;
                let r: serde_json::Value = resp.parse(&id)?;
                // RFC 8620 §5.3: `Email/set` returns `notDestroyed`
                // with a SetError per failed ID. If we don't surface
                // it the queued action gets `complete()`d (see
                // `flush_pending_actions`), so on the next `sync()`
                // `Email/changes` won't list the email as destroyed
                // (because it wasn't) and the locally-deleted email
                // pops back into the cache — a user-visible "I
                // deleted it and it came back" bug. Treat the same
                // way as `SetKeywords/MoveEmail` does for
                // `notUpdated`.
                if let Some(not_destroyed) = r
                    .get("notDestroyed")
                    .and_then(|v| v.as_object())
                    .and_then(|o| o.get(&action.target_id))
                {
                    let code = not_destroyed
                        .get("type")
                        .and_then(|v| v.as_str())
                        .unwrap_or("urn:ietf:params:jmap:error:serverFail")
                        .to_string();
                    return Err(Error::JmapMethod {
                        code,
                        description: not_destroyed
                            .get("description")
                            .and_then(|v| v.as_str())
                            .unwrap_or_default()
                            .to_string(),
                    });
                }
                Ok(())
            }
            PendingActionKind::SendEmail => {
                let draft: EmailDraft = serde_json::from_value(action.payload.clone())?;
                self.jmap
                    .send_email(session, account_id, &draft)
                    .await
                    .map(|_| ())
            }
        }
    }

    /// Path on disk where the SQLite store lives. Useful for
    /// "Show in Finder" affordances on desktop.
    pub fn database_path(&self) -> &std::path::Path {
        &self.config.database_path
    }

    /// Attach-only access to the embedded attachment cache.
    pub fn attachment_cache(&self) -> Arc<AttachmentCache> {
        Arc::clone(&self.cache)
    }

    /// For tests + the CLI: borrow the underlying store handle.
    pub fn store(&self) -> &Store {
        &self.store
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::jmap::request::{CAP_CORE, CAP_MAIL};
    use crate::models::{JmapAccount, MailboxRole};
    use std::collections::BTreeMap;
    use wiremock::matchers::{header, method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn mailbox_get_body() -> serde_json::Value {
        serde_json::json!({
            "sessionState": "s-1",
            "methodResponses": [
                ["Mailbox/get", {
                    "accountId": "acct-1",
                    "state": "mbx-1",
                    "list": [
                        {"id": "mbx-inbox", "name": "Inbox", "role": "inbox", "totalEmails": 2, "unreadEmails": 1},
                        {"id": "mbx-arch", "name": "Archive", "role": "archive"}
                    ],
                    "notFound": []
                }, "c0"]
            ]
        })
    }

    /// Combined bootstrap response. `Email/query` (call id `c0`)
    /// returns the newest-N IDs and `Email/get ids: []` (call id
    /// `c1`) returns the canonical Email state in the same JMAP
    /// request envelope — see `JmapClient::bootstrap_email_window`
    /// for the atomicity rationale.
    fn bootstrap_email_window_body() -> serde_json::Value {
        serde_json::json!({
            "sessionState": "s-1",
            "methodResponses": [
                ["Email/query", {
                    "accountId": "acct-1",
                    "queryState": "q-1",
                    "canCalculateChanges": true,
                    "position": 0,
                    "total": 2,
                    "ids": ["e-1", "e-2"]
                }, "c0"],
                ["Email/get", {
                    "accountId": "acct-1",
                    "state": "e-state-1",
                    "list": [],
                    "notFound": []
                }, "c1"]
            ]
        })
    }

    fn email_get_body() -> serde_json::Value {
        serde_json::json!({
            "sessionState": "s-1",
            "methodResponses": [
                ["Email/get", {
                    "accountId": "acct-1",
                    "state": "e-state-1",
                    "list": [
                        {
                            "id": "e-1",
                            "threadId": "t-1",
                            "blobId": "blob-1",
                            "mailboxIds": {"mbx-inbox": true},
                            "keywords": {"$seen": false},
                            "size": 1024,
                            "receivedAt": "2026-05-24T10:00:00Z",
                            "from": [{"name": "Alice", "email": "alice@example.com"}],
                            "to": [{"name": "", "email": "bob@example.com"}],
                            "subject": "Hello",
                            "preview": "Hi there"
                        },
                        {
                            "id": "e-2",
                            "threadId": "t-2",
                            "blobId": "blob-2",
                            "mailboxIds": {"mbx-inbox": true},
                            "keywords": {"$seen": true},
                            "size": 2048,
                            "receivedAt": "2026-05-24T09:00:00Z",
                            "from": [{"name": "Carol", "email": "carol@example.com"}],
                            "to": [{"name": "", "email": "bob@example.com"}],
                            "subject": "Re: status",
                            "preview": "Looks good"
                        }
                    ],
                    "notFound": []
                }, "c0"]
            ]
        })
    }

    async fn mount_session(server: &MockServer) {
        Mock::given(method("GET"))
            .and(path("/jmap/session"))
            .and(header("authorization", "Bearer test-token"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "apiUrl": format!("{}/jmap/api", server.uri()),
                "capabilities": {CAP_CORE: {}, CAP_MAIL: {}},
                "username": "alice@example.com",
                "accounts": {
                    "acct-1": {
                        "name": "Alice",
                        "isPersonal": true,
                        "isReadOnly": false,
                        "accountCapabilities": {}
                    }
                },
                "primaryAccounts": {CAP_MAIL: "acct-1"}
            })))
            .mount(server)
            .await;
    }

    /// Default `Mailbox/changes` stub: empty diff, advances state.
    ///
    /// The second-sync code path calls `Mailbox/changes` whenever a
    /// state token is persisted, so every test that runs `sync()`
    /// after a prior sync (or after pre-seeding a mailbox state
    /// token) must mount this stub. Tests that need a specific
    /// diff response should mount their own `Mailbox/changes`
    /// match BEFORE calling this helper (wiremock matchers are
    /// LIFO in match order).
    async fn mount_mailbox_changes_empty(server: &MockServer) {
        use wiremock::matchers::body_string_contains;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/changes\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-2",
                "methodResponses": [
                    ["Mailbox/changes", {
                        "accountId": "acct-1",
                        "oldState": "mbx-1",
                        "newState": "mbx-1",
                        "hasMoreChanges": false,
                        "created": [],
                        "updated": [],
                        "destroyed": []
                    }, "c0"]
                ]
            })))
            .mount(server)
            .await;
    }

    #[tokio::test]
    async fn full_initial_sync_persists_mailboxes_and_emails() {
        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");

        mount_session(&server).await;

        // Sequence of POSTs the SDK will issue:
        //   1) Mailbox/get      -> mailbox_get_body
        //   2) Email/query + Email/get(ids:[])  (one batched POST)
        //                       -> bootstrap_email_window_body
        //   3) Email/get(ids)   -> email_get_body  (hydration)
        // wiremock matchers are FIFO when same path is matched.
        // We use one-shot stubs (expect(1)) and disambiguate via
        // body_string_contains.
        use wiremock::matchers::body_string_contains;

        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/get\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(mailbox_get_body()))
            .expect(1)
            .mount(&server)
            .await;

        // The atomic bootstrap request batches `Email/query` and an
        // `Email/get ids: []` state probe into a single POST. The
        // body contains *both* strings, so we match on the unique
        // `"Email/query"` token to keep this stub from also
        // matching the hydration `Email/get` POST below.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/query\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(bootstrap_email_window_body()))
            .expect(1)
            .mount(&server)
            .await;

        // Hydration call: `Email/get` for the queried IDs (no
        // `Email/query` in the body, both `"e-1"` and `"e-2"`).
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .and(body_string_contains("\"e-1\""))
            .and(body_string_contains("\"e-2\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(email_get_body()))
            .expect(1)
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "test-token", db);
        let client = KMailClient::open(cfg).unwrap();
        let summary = client.sync().await.unwrap();
        assert_eq!(summary.mailboxes_upserted, 2);
        assert_eq!(summary.emails_created, 2);
        assert_eq!(summary.emails_destroyed, 0);

        let cached = client.cached_mailboxes().unwrap();
        assert_eq!(cached.len(), 2);
        let inbox = cached
            .iter()
            .find(|m| m.role == Some(MailboxRole::Inbox))
            .unwrap();
        assert_eq!(inbox.id, "mbx-inbox");

        let emails = client.cached_emails_in_mailbox("mbx-inbox", 50).unwrap();
        assert_eq!(emails.len(), 2);

        // State tokens persisted.
        assert_eq!(
            client
                .state_repo
                .get(SyncTypeName::Mailbox)
                .unwrap()
                .as_deref(),
            Some("mbx-1")
        );
        assert_eq!(
            client
                .state_repo
                .get(SyncTypeName::Email)
                .unwrap()
                .as_deref(),
            Some("e-state-1")
        );
        // Anchor the imports the test file pulls in but does not
        // otherwise reference at runtime.
        let _account_anchor = BTreeMap::<String, JmapAccount>::new();
    }

    #[tokio::test]
    async fn second_sync_uses_email_changes_with_saved_state() {
        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");

        mount_session(&server).await;
        // Second sync goes through `Mailbox/changes` because the
        // first sync persisted a mailbox state token; supply an
        // empty-diff stub so it returns successfully.
        mount_mailbox_changes_empty(&server).await;

        use wiremock::matchers::body_string_contains;

        // First sync setup: mailbox + atomic bootstrap (Email/query
        // + Email/get state probe in one POST) + hydration.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/get\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(mailbox_get_body()))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/query\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(bootstrap_email_window_body()))
            .expect(1)
            .mount(&server)
            .await;
        // First-sync hydration call: contains BOTH `"e-1"` and
        // `"e-2"`. The second-sync hydration call only contains
        // `"e-1"`, so checking for `"e-2"` cleanly partitions the
        // two stubs without relying on wiremock match ordering.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .and(body_string_contains("\"e-1\""))
            .and(body_string_contains("\"e-2\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(email_get_body()))
            .expect(1)
            .mount(&server)
            .await;

        // Second sync: Email/changes returns one update + one destroy.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/changes\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-2",
                "methodResponses": [
                    ["Email/changes", {
                        "accountId": "acct-1",
                        "oldState": "e-state-1",
                        "newState": "e-state-2",
                        "hasMoreChanges": false,
                        "created": [],
                        "updated": ["e-1"],
                        "destroyed": ["e-2"]
                    }, "c0"]
                ]
            })))
            .expect(1)
            .mount(&server)
            .await;

        // Second-sync hydration: payload has `"e-1"` but NOT
        // `"e-2"`. Matching on `"ids":[\"e-1\"]` keeps it
        // unambiguous against the first-sync hydration stub above.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .and(body_string_contains("\"ids\":[\"e-1\"]"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-2",
                "methodResponses": [
                    ["Email/get", {
                        "accountId": "acct-1",
                        "state": "e-state-2",
                        "list": [{
                            "id": "e-1",
                            "threadId": "t-1",
                            "blobId": "blob-1",
                            "mailboxIds": {"mbx-arch": true},
                            "keywords": {"$seen": true},
                            "size": 1024,
                            "receivedAt": "2026-05-24T10:00:00Z",
                            "from": [{"name": "Alice", "email": "alice@example.com"}],
                            "to": [{"name": "", "email": "bob@example.com"}],
                            "subject": "Hello",
                            "preview": "Hi there"
                        }],
                        "notFound": []
                    }, "c0"]
                ]
            })))
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "test-token", db);
        let client = KMailClient::open(cfg).unwrap();
        let _first = client.sync().await.unwrap();
        let second = client.sync().await.unwrap();
        assert_eq!(second.emails_created, 0);
        assert_eq!(second.emails_updated, 1);
        assert_eq!(second.emails_destroyed, 1);

        // After the second sync, e-2 must be gone and e-1's mailbox
        // must have moved to mbx-arch.
        let inbox = client.cached_emails_in_mailbox("mbx-inbox", 50).unwrap();
        assert!(inbox.is_empty(), "e-1 moved out, e-2 destroyed");
        let arch = client.cached_emails_in_mailbox("mbx-arch", 50).unwrap();
        assert_eq!(arch.len(), 1);
        assert_eq!(arch[0].id, "e-1");
    }

    /// `Error::SyncStateDiverged` triggers a re-bootstrap rather
    /// than propagating to the caller.
    #[tokio::test]
    async fn diverged_state_token_triggers_rebootstrap() {
        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");

        mount_session(&server).await;

        use wiremock::matchers::body_string_contains;

        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/get\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(mailbox_get_body()))
            .mount(&server)
            .await;

        // First call to Email/changes returns cannotCalculateChanges;
        // SDK must fall through to Email/query + Email/get.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/changes\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["error", {
                        "type": "urn:ietf:params:jmap:error:cannotCalculateChanges",
                        "description": "state too old"
                    }, "c0"]
                ]
            })))
            .expect(1)
            .mount(&server)
            .await;

        // Re-bootstrap path issues the atomic Email/query +
        // Email/get(ids:[]) batch in a single POST.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/query\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(bootstrap_email_window_body()))
            .expect(1)
            .mount(&server)
            .await;

        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .and(body_string_contains("\"e-1\""))
            .and(body_string_contains("\"e-2\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(email_get_body()))
            .expect(1)
            .mount(&server)
            .await;

        // Pre-seed a stale email state token so the SDK takes the
        // Email/changes path on the first sync().
        let cfg = ClientConfig::new(server.uri(), "test-token", db.clone());
        let client = KMailClient::open(cfg).unwrap();
        client
            .state_repo
            .put(SyncTypeName::Email, "stale-state")
            .unwrap();

        let summary = client.sync().await.unwrap();
        assert_eq!(summary.emails_created, 2, "must re-bootstrap on divergence");
    }

    /// `sync()` must drain `Email/changes` until the server reports
    /// `hasMoreChanges: false`. A single batch that returns
    /// `hasMoreChanges: true` would otherwise leave the local state
    /// behind by however many extra batches the server has queued
    /// up — the symptom in production would be a freshly-synced
    /// inbox missing emails until the next sync() call.
    #[tokio::test]
    async fn sync_drains_email_changes_until_has_more_is_false() {
        use std::sync::atomic::{AtomicU32, Ordering};
        use std::sync::Arc;
        use wiremock::matchers::body_string_contains;
        use wiremock::Respond;

        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");
        mount_session(&server).await;

        // Mailbox/get stub — same as the other tests.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/get\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(mailbox_get_body()))
            .mount(&server)
            .await;

        // Counter-driven Email/changes responder: batch 1 returns
        // `hasMoreChanges: true` with one `created`; batch 2
        // returns `hasMoreChanges: false` with a second `created`.
        // If the SDK respects the flag, both should be hydrated;
        // if it ignores the flag (the original bug), only the
        // first batch's email is hydrated.
        struct ChangesSequence {
            counter: Arc<AtomicU32>,
        }
        impl Respond for ChangesSequence {
            fn respond(&self, _req: &wiremock::Request) -> ResponseTemplate {
                let n = self.counter.fetch_add(1, Ordering::SeqCst);
                let body = if n == 0 {
                    serde_json::json!({
                        "sessionState": "s-2",
                        "methodResponses": [
                            ["Email/changes", {
                                "accountId": "acct-1",
                                "oldState": "e-state-1",
                                "newState": "e-state-1b",
                                "hasMoreChanges": true,
                                "created": ["e-batch1"],
                                "updated": [],
                                "destroyed": []
                            }, "c0"]
                        ]
                    })
                } else {
                    serde_json::json!({
                        "sessionState": "s-2",
                        "methodResponses": [
                            ["Email/changes", {
                                "accountId": "acct-1",
                                "oldState": "e-state-1b",
                                "newState": "e-state-2",
                                "hasMoreChanges": false,
                                "created": ["e-batch2"],
                                "updated": [],
                                "destroyed": []
                            }, "c0"]
                        ]
                    })
                };
                ResponseTemplate::new(200).set_body_json(body)
            }
        }
        let counter = Arc::new(AtomicU32::new(0));
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/changes\""))
            .respond_with(ChangesSequence {
                counter: Arc::clone(&counter),
            })
            .expect(2)
            .mount(&server)
            .await;

        // One hydration stub per batch — match on the unique ID.
        let hydration_body = |id: &str| -> serde_json::Value {
            serde_json::json!({
                "sessionState": "s-2",
                "methodResponses": [
                    ["Email/get", {
                        "accountId": "acct-1",
                        "state": "e-state-2",
                        "list": [{
                            "id": id,
                            "threadId": format!("t-{id}"),
                            "blobId": format!("blob-{id}"),
                            "mailboxIds": {"mbx-inbox": true},
                            "keywords": {},
                            "size": 64,
                            "receivedAt": "2026-05-24T10:00:00Z",
                            "from": [{"name": "X", "email": "x@example.com"}],
                            "to": [{"name": "", "email": "bob@example.com"}],
                            "subject": format!("subj {id}"),
                            "preview": ""
                        }],
                        "notFound": []
                    }, "c0"]
                ]
            })
        };
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .and(body_string_contains("\"e-batch1\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(hydration_body("e-batch1")))
            .expect(1)
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .and(body_string_contains("\"e-batch2\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(hydration_body("e-batch2")))
            .expect(1)
            .mount(&server)
            .await;

        // Pre-seed a saved state token so the SDK takes the
        // Email/changes path immediately, skipping bootstrap.
        let cfg = ClientConfig::new(server.uri(), "test-token", db);
        let client = KMailClient::open(cfg).unwrap();
        client
            .state_repo
            .put(SyncTypeName::Email, "e-state-1")
            .unwrap();

        let summary = client.sync().await.unwrap();
        // Both batches must be ingested.
        assert_eq!(
            summary.emails_created, 2,
            "both Email/changes batches must hydrate"
        );
        assert_eq!(counter.load(Ordering::SeqCst), 2);

        // Saved state token must advance to the FINAL batch's
        // newState — not the intermediate "e-state-1b".
        assert_eq!(
            client
                .state_repo
                .get(SyncTypeName::Email)
                .unwrap()
                .as_deref(),
            Some("e-state-2")
        );

        // Both hydrated emails are in cache.
        let in_inbox = client.cached_emails_in_mailbox("mbx-inbox", 50).unwrap();
        assert!(in_inbox.iter().any(|e| e.id == "e-batch1"));
        assert!(in_inbox.iter().any(|e| e.id == "e-batch2"));
    }

    /// Regression: `ClientConfig::Debug` MUST NOT render the bearer
    /// token. A future `tracing::debug!(?config, ...)` should print
    /// `<redacted>` instead of leaking the OIDC token into structured
    /// logs.
    #[test]
    fn client_config_debug_redacts_bearer_token() {
        let cfg = ClientConfig::new(
            "https://kmail.example.com",
            "eyJhbGciOiJSUzI1NiJ9.this-is-a-real-looking-jwt",
            PathBuf::from("/tmp/kmail-redact.db"),
        );
        let dbg = format!("{cfg:?}");
        assert!(!dbg.contains("eyJhbGciOiJSUzI1NiJ9"), "token leaked: {dbg}");
        assert!(!dbg.contains("real-looking-jwt"), "token leaked: {dbg}");
        assert!(
            dbg.contains("<redacted>"),
            "expected redaction marker, got: {dbg}"
        );
        // Non-secret fields should still be visible for debuggability.
        assert!(dbg.contains("kmail.example.com"));
    }

    /// Regression: `KMailClient::set_bearer_token` must be observable
    /// by sibling clones of the same client, since the FFI / napi
    /// layers freely clone the inner `KMailClient`. The transport
    /// holds the token behind `Arc<RwLock<String>>`; this test pins
    /// that contract.
    #[tokio::test]
    async fn set_bearer_token_is_visible_to_clones() {
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-token.db");
        let mut cfg = ClientConfig::new("https://kmail.example.com", "v0-token", db);
        cfg.account_id = Some("acct-fixed".into());
        let client = KMailClient::open(cfg).unwrap();
        let cloned = client.clone();

        client.set_bearer_token("v1-token").unwrap();

        // Any path that reads the live token must see the new value.
        // The transport is the source of truth; both clones share it.
        let live_via_origin = client.jmap.current_bearer_token_for_test().unwrap();
        let live_via_clone = cloned.jmap.current_bearer_token_for_test().unwrap();
        assert_eq!(live_via_origin, "v1-token");
        assert_eq!(live_via_clone, "v1-token");
    }

    /// Regression: `invalidate_session()` must drop the cached
    /// session so the next `discover_session()` re-fetches from
    /// `/jmap/session`. Use case: tenant resharding / plan upgrade
    /// where the server's session document has changed but the SDK
    /// has cached the stale copy.
    #[tokio::test]
    async fn invalidate_session_forces_refetch() {
        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-invalidate.db");
        // The mock expects exactly 2 GETs to /jmap/session — one
        // before invalidation, one after. If invalidation didn't
        // drop the cache, the second discover_session() would be
        // served from memory and the mock would observe only 1
        // request, failing the .expect(2) assertion.
        Mock::given(method("GET"))
            .and(path("/jmap/session"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "apiUrl": format!("{}/jmap/api", server.uri()),
                "capabilities": {CAP_CORE: {}, CAP_MAIL: {}},
                "username": "alice@example.com",
                "accounts": {
                    "acct-1": {
                        "name": "Alice",
                        "isPersonal": true,
                        "isReadOnly": false,
                        "accountCapabilities": {}
                    }
                },
                "primaryAccounts": {CAP_MAIL: "acct-1"}
            })))
            .expect(2)
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "test-token", db);
        let client = KMailClient::open(cfg).unwrap();

        // First call: cold fetch.
        let s1 = client.discover_session().await.unwrap();
        // Second call without invalidation: served from cache, no
        // new HTTP request.
        let s2 = client.discover_session().await.unwrap();
        assert_eq!(s1.username, s2.username);

        // Now invalidate and re-discover: a second HTTP request
        // must hit the mock for `.expect(2)` to be satisfied on
        // drop.
        client.invalidate_session().await;
        let s3 = client.discover_session().await.unwrap();
        assert_eq!(s3.username, "alice@example.com");
    }

    /// Regression for the cross-shell push-token bug: building a
    /// fresh `JmapTransport` from `ClientConfig::bearer_token` in
    /// `register_push_token` would ignore any subsequent
    /// `set_bearer_token` refresh and 401 every push registration
    /// after the first OIDC refresh. The fix routes push through
    /// the live `JmapClient::post_json`. Verify the BFF call is
    /// authenticated with the *current* live token.
    #[tokio::test]
    async fn register_push_token_uses_live_bearer_token() {
        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-push.db");

        // Accept the push subscribe POST only when it carries the
        // refreshed token. If `register_push_token` used the stale
        // ClientConfig snapshot, this stub would never match and
        // the call would 404 (wiremock default for unmatched).
        Mock::given(method("POST"))
            .and(path("/api/v1/push/subscribe"))
            .and(header("authorization", "Bearer refreshed-token"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "id": "sub-1",
                "tenant_id": "t-1",
                "user_id": "u-1",
                "device_type": "ios",
                "push_endpoint": "apns-device-token",
                "created_at": "2024-01-01T00:00:00Z"
            })))
            .expect(1)
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "original-token", db);
        let client = KMailClient::open(cfg).unwrap();
        client.set_bearer_token("refreshed-token").unwrap();

        client
            .register_push_token(PushTransport::Apns, "apns-device-token", None)
            .await
            .expect("push registration should succeed with the live token");
    }

    /// `bootstrap_sync` hits the BFF's
    /// `POST /api/v1/sync/bootstrap` and persists the response in
    /// one transaction per type so the local snapshot matches a
    /// full first-launch `sync()` minus two extra round-trips.
    /// The mock encodes the contract:
    ///
    ///   * Request body shape matches
    ///     `internal/sync/sync.go::BootstrapRequest`
    ///     (`mailbox_role`, `limit`).
    ///   * Response shape matches `BootstrapResponse`
    ///     (`account_id`, `mailboxes`, `mailbox_state`,
    ///     `emails`, `email_state`).
    ///   * After the call: mailboxes + emails are in the local
    ///     store, both state tokens are persisted as JMAP
    ///     cursors, and the cached `account_id` is set so the
    ///     next operation doesn't have to discover the session.
    #[tokio::test]
    async fn bootstrap_sync_persists_full_snapshot() {
        use wiremock::matchers::{body_string_contains, header, method, path};
        use wiremock::{Mock, MockServer, ResponseTemplate};

        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-bootstrap.db");

        let response = serde_json::json!({
            "account_id": "acc-1",
            "mailboxes": [
                {"id": "mbx-inbox", "name": "Inbox", "role": "inbox"},
                {"id": "mbx-sent", "name": "Sent", "role": "sent"}
            ],
            "mailbox_state": "ms-100",
            "emails": [
                {
                    "id": "e-1",
                    "blobId": "b-1",
                    "threadId": "t-1",
                    "mailboxIds": {"mbx-inbox": true},
                    "keywords": {"$seen": true},
                    "subject": "Hello",
                    "preview": "world",
                    "from": [{"name": "Alice", "email": "alice@x.test"}],
                    "to": [{"name": "Bob", "email": "bob@x.test"}],
                    "receivedAt": "2025-01-02T03:04:05Z"
                }
            ],
            "email_state": "es-100",
            "bootstrapped_at": "2025-01-02T03:04:05Z"
        });

        Mock::given(method("POST"))
            .and(path("/api/v1/sync/bootstrap"))
            .and(header("authorization", "Bearer tok"))
            .and(body_string_contains("\"mailbox_role\":\"inbox\""))
            .and(body_string_contains("\"limit\":50"))
            .respond_with(ResponseTemplate::new(200).set_body_json(&response))
            .expect(1)
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "tok", db);
        let client = KMailClient::open(cfg).unwrap();

        let summary = client
            .bootstrap_sync(Some("inbox"), Some(50))
            .await
            .expect("bootstrap_sync should succeed");

        assert_eq!(summary.mailboxes_upserted, 2);
        assert_eq!(summary.emails_created, 1);

        // Mailboxes are persisted.
        let mbs = client.cached_mailboxes().unwrap();
        assert_eq!(mbs.len(), 2);
        assert!(mbs.iter().any(|m| m.id == "mbx-inbox"));

        // Emails are persisted in the inbox.
        let inbox = client.cached_emails_in_mailbox("mbx-inbox", 10).unwrap();
        assert_eq!(inbox.len(), 1);
        assert_eq!(inbox[0].id, "e-1");

        // State tokens are persisted on the canonical cursor keys.
        let email_state = client
            .state_repo
            .get(SyncTypeName::Email)
            .unwrap()
            .expect("email state should be persisted");
        assert_eq!(email_state, "es-100");
        let mailbox_state = client
            .state_repo
            .get(SyncTypeName::Mailbox)
            .unwrap()
            .expect("mailbox state should be persisted");
        assert_eq!(mailbox_state, "ms-100");

        // Account ID is eagerly cached.
        let cached_acc = client.account_id.lock().unwrap().clone();
        assert_eq!(cached_acc.as_deref(), Some("acc-1"));
    }

    /// Without `email_id`, an `EmailDeliveryHint` is meaningless
    /// — the SDK falls back to a full `sync()` rather than insert
    /// a row keyed by an empty string. Pin the gate: any
    /// `from_data` call without `email_id` MUST return `None`.
    #[test]
    fn email_delivery_hint_requires_email_id() {
        use crate::push::EmailDeliveryHint;
        let mut data = std::collections::BTreeMap::new();
        data.insert("account_id".into(), "acc-1".into());
        assert!(EmailDeliveryHint::from_data(&data).is_none());
        data.insert("email_id".into(), "e-1".into());
        let hint = EmailDeliveryHint::from_data(&data).expect("present");
        assert_eq!(hint.email_id.as_deref(), Some("e-1"));
        assert_eq!(hint.account_id.as_deref(), Some("acc-1"));
    }

    /// `EmailDeliveryHint::from_data` parses every BFF-emitted
    /// field into its typed accessor. Wire-format keys are pinned
    /// in `internal/push/email_delivery.go::EmailDeliveryKey*` —
    /// rename one without updating both sides and this test fires.
    #[test]
    fn email_delivery_hint_parses_full_payload() {
        use crate::push::EmailDeliveryHint;
        let mut data = std::collections::BTreeMap::new();
        data.insert("email_id".into(), "e-42".into());
        data.insert("account_id".into(), "acc-7".into());
        data.insert("mailbox_id".into(), "mbx-inbox".into());
        data.insert("thread_id".into(), "t-9".into());
        data.insert("subject".into(), "Hi".into());
        data.insert("snippet".into(), "world".into());
        data.insert("from".into(), "Alice <a@x.test>".into());
        data.insert("received_at_unix".into(), "1735787045".into());
        data.insert("has_attachment".into(), "true".into());
        data.insert("email_state".into(), "es-1".into());
        data.insert("mailbox_state".into(), "ms-1".into());
        data.insert("keywords".into(), "$seen,$important".into());

        let h = EmailDeliveryHint::from_data(&data).expect("present");
        assert_eq!(h.email_id.as_deref(), Some("e-42"));
        assert_eq!(h.account_id.as_deref(), Some("acc-7"));
        assert_eq!(h.mailbox_id.as_deref(), Some("mbx-inbox"));
        assert_eq!(h.thread_id.as_deref(), Some("t-9"));
        assert_eq!(h.subject.as_deref(), Some("Hi"));
        assert_eq!(h.snippet.as_deref(), Some("world"));
        assert_eq!(h.from.as_deref(), Some("Alice <a@x.test>"));
        assert_eq!(h.received_at_unix, Some(1_735_787_045));
        assert_eq!(h.has_attachment, Some(true));
        assert_eq!(h.email_state.as_deref(), Some("es-1"));
        assert_eq!(h.mailbox_state.as_deref(), Some("ms-1"));
        assert_eq!(
            h.keywords,
            vec!["$seen".to_string(), "$important".to_string()]
        );
    }

    /// Malformed numeric / boolean fields are dropped silently —
    /// the SDK degrades gracefully rather than poisoning the
    /// whole hint. `email_id` still has to be present, so callers
    /// still get useful metadata back from the surviving fields.
    #[test]
    fn email_delivery_hint_degrades_on_malformed_fields() {
        use crate::push::EmailDeliveryHint;
        let mut data = std::collections::BTreeMap::new();
        data.insert("email_id".into(), "e-1".into());
        data.insert("received_at_unix".into(), "not-an-integer".into());
        data.insert("has_attachment".into(), "maybe".into());
        let h = EmailDeliveryHint::from_data(&data).expect("present");
        assert_eq!(h.received_at_unix, None);
        assert_eq!(h.has_attachment, None);
    }

    /// `enqueue_set_keywords` MUST shape its payload as a JMAP
    /// path-style PatchObject so the server merges the update
    /// instead of replacing the whole `keywords` property. The
    /// stored payload is what `apply_pending_action` later embeds
    /// verbatim under `update[<emailId>]` in `Email/set`, so the
    /// shape of the row in the queue is exactly the shape of the
    /// wire patch. Pin both the path format and the value
    /// validation.
    #[test]
    fn enqueue_set_keywords_emits_path_style_patch() {
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-kw.db");
        let cfg = ClientConfig::new("https://bff.test", "tok", db);
        let client = KMailClient::open(cfg).unwrap();

        client
            .enqueue_set_keywords(
                "e-1",
                &serde_json::json!({"$seen": true, "$flagged": serde_json::Value::Null}),
            )
            .unwrap();

        let batch = client.actions_repo.next_batch(10).unwrap();
        assert_eq!(batch.len(), 1);
        let stored = &batch[0].payload;
        let obj = stored
            .as_object()
            .expect("stored payload must be an object");
        // Path-style: every key MUST be prefixed with `keywords/`,
        // there is NO bare `keywords` entry (which would whole-
        // property-replace and silently drop other keywords).
        assert!(
            obj.contains_key("keywords/$seen"),
            "missing path-style key: {obj:?}"
        );
        assert!(
            obj.contains_key("keywords/$flagged"),
            "missing path-style key: {obj:?}"
        );
        assert!(
            !obj.contains_key("keywords"),
            "bare `keywords` key would whole-property-replace: {obj:?}"
        );
        assert_eq!(obj.get("keywords/$seen").unwrap(), &serde_json::json!(true));
        assert!(obj.get("keywords/$flagged").unwrap().is_null());

        // Invalid shapes are rejected up front, not at apply time.
        let bad = client.enqueue_set_keywords("e-1", &serde_json::json!("not-an-object"));
        assert!(matches!(bad, Err(Error::InvalidArgument(_))));

        let bad = client.enqueue_set_keywords("e-1", &serde_json::json!({}));
        assert!(matches!(bad, Err(Error::InvalidArgument(_))));

        let bad = client.enqueue_set_keywords("e-1", &serde_json::json!({"$seen": "yes-please"}));
        assert!(matches!(bad, Err(Error::InvalidArgument(_))));

        let bad = client.enqueue_set_keywords("e-1", &serde_json::json!({"keywords/$seen": true}));
        assert!(
            matches!(bad, Err(Error::InvalidArgument(_))),
            "keyword names containing `/` MUST be rejected so callers can't smuggle paths"
        );
    }

    /// `apply_pending_action(DeleteEmail)` MUST treat a `notDestroyed`
    /// entry in the `Email/set` response as a terminal `JmapMethod`
    /// error. Before the fix the response was parsed into a
    /// `serde_json::Value` and immediately discarded (`let _r = ...`),
    /// so server-side refusals (read-only mailbox, permissions,
    /// concurrent conflict, …) were silently treated as success.
    /// The queued action was then `complete()`d, the local row was
    /// already gone, and the next `Email/changes` re-introduced the
    /// email because it still existed on the server — a user-visible
    /// "I deleted it and it came back" bug.
    #[tokio::test]
    async fn delete_email_surfaces_not_destroyed_as_terminal_jmap_error() {
        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-del.db");

        mount_session(&server).await;

        // BFF responds with notDestroyed for our target ID.
        use wiremock::matchers::body_string_contains;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"destroy\""))
            .and(body_string_contains("\"e-target\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["Email/set", {
                        "accountId": "acct-1",
                        "destroyed": [],
                        "notDestroyed": {
                            "e-target": {
                                "type": "forbidden",
                                "description": "mailbox is read-only"
                            }
                        }
                    }, "c0"]
                ]
            })))
            .expect(1)
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "test-token", db);
        let client = KMailClient::open(cfg).unwrap();

        // Enqueue the action manually rather than via a public API
        // (there is no `enqueue_delete_email` today — the BFF triggers
        // deletes through other paths). Drive the dispatch with the
        // private `apply_pending_action` directly so this test
        // exercises exactly the parse path under review.
        let id = client
            .actions_repo
            .enqueue(
                PendingActionKind::DeleteEmail,
                "e-target",
                &serde_json::json!({}),
            )
            .unwrap();
        let session = client.discover_session().await.unwrap();
        let batch = client.actions_repo.next_batch(1).unwrap();
        assert_eq!(batch.len(), 1);
        assert_eq!(batch[0].id, id);

        let result = client
            .apply_pending_action(&session, "acct-1", &batch[0])
            .await;

        match result {
            Err(Error::JmapMethod { code, description }) => {
                assert_eq!(code, "forbidden");
                assert_eq!(description, "mailbox is read-only");
            }
            other => panic!("expected JmapMethod error, got {other:?}"),
        }
        // And that error is NOT retryable — the queue drainer
        // would terminally fail the action, not silently complete it.
        assert!(!Error::JmapMethod {
            code: "forbidden".into(),
            description: String::new(),
        }
        .is_retryable());
    }

    /// Regression for the SyncStateDiverged ghost-email bug.
    ///
    /// Setup: the local cache holds two emails (`e-1`, `e-2`) plus
    /// an `Email` state token. The server then loses our cursor —
    /// next `Email/changes` returns `cannotCalculateChanges` — and
    /// the bootstrap window now only contains `e-1`. The expected
    /// behaviour is that `e-2` is purged from the local cache: it
    /// was deleted on the server during the gap that triggered the
    /// divergence, and the next `Email/changes` (starting from the
    /// fresh state) will never report it as `destroyed`. Without
    /// the purge it would linger as a ghost forever.
    #[tokio::test]
    async fn rebootstrap_on_diverged_state_purges_stale_local_emails() {
        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");
        mount_session(&server).await;

        use wiremock::matchers::body_string_contains;

        // Mailbox/get for the initial population.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/get\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(mailbox_get_body()))
            .mount(&server)
            .await;

        // Email/changes returns cannotCalculateChanges — server has
        // moved past our cursor.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/changes\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-2",
                "methodResponses": [
                    ["error", {
                        "type": "urn:ietf:params:jmap:error:cannotCalculateChanges",
                        "description": "state too old"
                    }, "c0"]
                ]
            })))
            .expect(1)
            .mount(&server)
            .await;

        // Re-bootstrap returns ONLY `e-1` (so `e-2` is the ghost
        // we expect to be purged).
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/query\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-2",
                "methodResponses": [
                    ["Email/query", {
                        "accountId": "acct-1",
                        "queryState": "q-2",
                        "canCalculateChanges": true,
                        "position": 0,
                        "total": 1,
                        "ids": ["e-1"]
                    }, "c0"],
                    ["Email/get", {
                        "accountId": "acct-1",
                        "state": "e-state-fresh",
                        "list": [],
                        "notFound": []
                    }, "c1"]
                ]
            })))
            .expect(1)
            .mount(&server)
            .await;

        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .and(body_string_contains("\"ids\":[\"e-1\"]"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-2",
                "methodResponses": [
                    ["Email/get", {
                        "accountId": "acct-1",
                        "state": "e-state-fresh",
                        "list": [{
                            "id": "e-1",
                            "threadId": "t-1",
                            "blobId": "blob-1",
                            "mailboxIds": {"mbx-inbox": true},
                            "keywords": {"$seen": true},
                            "size": 1024,
                            "receivedAt": "2026-05-24T10:00:00Z",
                            "from": [{"name": "Alice", "email": "alice@example.com"}],
                            "to": [{"name": "", "email": "bob@example.com"}],
                            "subject": "Hello",
                            "preview": "Hi there"
                        }],
                        "notFound": []
                    }, "c0"]
                ]
            })))
            .expect(1)
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "test-token", db);
        let client = KMailClient::open(cfg).unwrap();

        // Pre-seed the local cache with both emails and a stale
        // state token, mimicking what a prior successful sync
        // would have left.
        client
            .mailbox_repo
            .upsert(&Mailbox {
                id: "mbx-inbox".into(),
                name: "Inbox".into(),
                role: Some(MailboxRole::Inbox),
                parent_id: None,
                sort_order: 0,
                total_emails: 0,
                unread_emails: 0,
                total_threads: 0,
                unread_threads: 0,
                is_vault: false,
                my_rights: None,
            })
            .unwrap();
        for id in ["e-1", "e-2"] {
            let summary = EmailSummary {
                id: id.into(),
                thread_id: format!("t-{id}"),
                blob_id: format!("blob-{id}"),
                mailbox_ids: std::iter::once(("mbx-inbox".to_string(), true)).collect(),
                keywords: std::collections::BTreeMap::new(),
                size: 100,
                received_at: chrono::Utc::now(),
                sent_at: None,
                from: vec![],
                to: vec![],
                cc: vec![],
                bcc: vec![],
                reply_to: vec![],
                subject: "old".into(),
                preview: String::new(),
                has_attachment: false,
            };
            client.email_repo.upsert(&summary).unwrap();
        }
        client
            .state_repo
            .put(SyncTypeName::Email, "stale-state")
            .unwrap();

        // Sanity: both are in the cache.
        assert_eq!(client.email_repo.count().unwrap(), 2);
        assert!(client.email_repo.get("e-2").unwrap().is_some());

        let summary = client.sync().await.unwrap();

        // The ghost — `e-2` — must be gone.
        assert!(
            client.email_repo.get("e-2").unwrap().is_none(),
            "e-2 was deleted server-side during the divergence gap; \
             local cache must purge it on re-bootstrap, not leave it as a ghost"
        );
        // Only the fresh bootstrap window remains.
        assert_eq!(client.email_repo.count().unwrap(), 1);
        assert!(client.email_repo.get("e-1").unwrap().is_some());
        // Summary surfaces the purged rows so callers can show a
        // "cache rebuilt" notice if they want.
        assert!(
            summary.emails_destroyed >= 2,
            "expected destroyed counter to include purged ghosts, got {}",
            summary.emails_destroyed
        );
        // Fresh state token persisted.
        assert_eq!(
            client
                .state_repo
                .get(SyncTypeName::Email)
                .unwrap()
                .as_deref(),
            Some("e-state-fresh")
        );
    }

    /// Regression: the mailbox state token persisted by a prior
    /// sync must drive the next sync through `Mailbox/changes`, not
    /// just be inert metadata.
    #[tokio::test]
    async fn second_sync_consumes_persisted_mailbox_state_via_mailbox_changes() {
        use std::sync::atomic::{AtomicU32, Ordering};
        use std::sync::Arc;
        use wiremock::matchers::body_string_contains;

        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");
        mount_session(&server).await;

        let mailbox_get_calls = Arc::new(AtomicU32::new(0));
        let mailbox_changes_calls = Arc::new(AtomicU32::new(0));

        // Counter-driven Mailbox/get responder: returns the full
        // set the first time, an empty set on any subsequent call
        // (which would fail the test's assertion).
        struct CountingResponder {
            counter: Arc<AtomicU32>,
            body: serde_json::Value,
        }
        impl wiremock::Respond for CountingResponder {
            fn respond(&self, _req: &wiremock::Request) -> ResponseTemplate {
                self.counter.fetch_add(1, Ordering::SeqCst);
                ResponseTemplate::new(200).set_body_json(self.body.clone())
            }
        }

        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/get\""))
            .respond_with(CountingResponder {
                counter: Arc::clone(&mailbox_get_calls),
                body: mailbox_get_body(),
            })
            .mount(&server)
            .await;

        // Mailbox/changes responder: empty diff that advances the
        // state from "mbx-1" -> "mbx-2".
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/changes\""))
            .respond_with(CountingResponder {
                counter: Arc::clone(&mailbox_changes_calls),
                body: serde_json::json!({
                    "sessionState": "s-2",
                    "methodResponses": [
                        ["Mailbox/changes", {
                            "accountId": "acct-1",
                            "oldState": "mbx-1",
                            "newState": "mbx-2",
                            "hasMoreChanges": false,
                            "created": [],
                            "updated": [],
                            "destroyed": []
                        }, "c0"]
                    ]
                }),
            })
            .mount(&server)
            .await;

        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/query\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(bootstrap_email_window_body()))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(email_get_body()))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/changes\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-2",
                "methodResponses": [
                    ["Email/changes", {
                        "accountId": "acct-1",
                        "oldState": "e-state-1",
                        "newState": "e-state-1",
                        "hasMoreChanges": false,
                        "created": [],
                        "updated": [],
                        "destroyed": []
                    }, "c0"]
                ]
            })))
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "test-token", db);
        let client = KMailClient::open(cfg).unwrap();

        let _first = client.sync().await.unwrap();
        // First sync: Mailbox/get (no state token yet),
        // Mailbox/changes NOT called.
        assert_eq!(mailbox_get_calls.load(Ordering::SeqCst), 1);
        assert_eq!(mailbox_changes_calls.load(Ordering::SeqCst), 0);
        assert_eq!(
            client
                .state_repo
                .get(SyncTypeName::Mailbox)
                .unwrap()
                .as_deref(),
            Some("mbx-1")
        );

        let _second = client.sync().await.unwrap();
        // Second sync: state token was persisted, so the SDK takes
        // the Mailbox/changes path. Mailbox/get must NOT be called
        // a second time — that would re-fetch the entire mailbox
        // set unnecessarily and ignore the state cursor.
        assert_eq!(
            mailbox_get_calls.load(Ordering::SeqCst),
            1,
            "second sync should NOT re-fetch the full mailbox set; \
             the persisted state token must drive Mailbox/changes instead"
        );
        assert_eq!(mailbox_changes_calls.load(Ordering::SeqCst), 1);

        // The new state was adopted.
        assert_eq!(
            client
                .state_repo
                .get(SyncTypeName::Mailbox)
                .unwrap()
                .as_deref(),
            Some("mbx-2"),
            "Mailbox/changes newState must replace the prior cursor"
        );
    }

    /// Regression: `flush_pending_actions` counts must distinguish
    /// terminally-failed actions from successfully-applied ones.
    /// Previous implementation conflated both into "flushed", so a
    /// queue of nothing-but-bad-payloads silently drained sync
    /// after sync with no observability signal.
    #[tokio::test]
    async fn flush_pending_actions_separates_applied_failed_and_deferred() {
        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");
        mount_session(&server).await;

        use wiremock::matchers::body_string_contains;

        // Two actions in the queue:
        //   - "ok-1": Email/set returns successfully → applied
        //   - "bad-1": Email/set returns notUpdated with a 4xx-equivalent
        //     terminal error → failed (NOT retried)
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"ok-1\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["Email/set", {
                        "accountId": "acct-1",
                        "newState": "e-state-1",
                        "updated": {"ok-1": null}
                    }, "c0"]
                ]
            })))
            .mount(&server)
            .await;

        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"bad-1\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["Email/set", {
                        "accountId": "acct-1",
                        "newState": "e-state-1",
                        "notUpdated": {
                            "bad-1": {
                                "type": "invalidProperties",
                                "description": "keyword too long"
                            }
                        }
                    }, "c0"]
                ]
            })))
            .mount(&server)
            .await;

        let mut cfg = ClientConfig::new(server.uri(), "test-token", db);
        cfg.account_id = Some("acct-1".into());
        let client = KMailClient::open(cfg).unwrap();

        client
            .actions_repo
            .enqueue(
                PendingActionKind::SetKeywords,
                "ok-1",
                &serde_json::json!({"keywords/$seen": true}),
            )
            .unwrap();
        client
            .actions_repo
            .enqueue(
                PendingActionKind::SetKeywords,
                "bad-1",
                &serde_json::json!({"keywords/$seen": true}),
            )
            .unwrap();

        let session = client.discover_session().await.unwrap();
        let outcome = client
            .flush_pending_actions(&session, "acct-1", 10)
            .await
            .unwrap();

        assert_eq!(outcome.applied, 1, "ok-1 should be applied");
        assert_eq!(outcome.failed, 1, "bad-1 should be terminally failed");
        assert_eq!(outcome.deferred, 0, "no network blip → nothing deferred");
    }

    /// Regression: `MailboxRepo::upsert_many_with_state` must
    /// either commit both the rows and the state token, or commit
    /// neither — never one without the other. Without this
    /// atomicity, a crash between the two writes would leave the
    /// cache convinced it had observed a JMAP cursor it never
    /// actually consumed, silently dropping any subsequent
    /// Mailbox/changes against that state.
    #[test]
    fn upsert_many_with_state_is_atomic() {
        let store = Store::open_in_memory().unwrap();
        let repo = MailboxRepo::new(store.clone());
        let state = StateRepo::new(store);

        let mbx = Mailbox {
            id: "m-1".into(),
            name: "Inbox".into(),
            role: Some(MailboxRole::Inbox),
            parent_id: None,
            sort_order: 0,
            total_emails: 0,
            unread_emails: 0,
            total_threads: 0,
            unread_threads: 0,
            is_vault: false,
            my_rights: None,
        };
        repo.upsert_many_with_state(std::slice::from_ref(&mbx), &[], "mbx-state-1")
            .unwrap();

        // Both writes landed.
        assert_eq!(repo.count().unwrap(), 1);
        assert_eq!(
            state.get(SyncTypeName::Mailbox).unwrap().as_deref(),
            Some("mbx-state-1")
        );

        // A subsequent call with `destroyed` removes the row and
        // advances the cursor — also in one transaction.
        repo.upsert_many_with_state(&[], &["m-1".to_string()], "mbx-state-2")
            .unwrap();
        assert_eq!(repo.count().unwrap(), 0);
        assert_eq!(
            state.get(SyncTypeName::Mailbox).unwrap().as_deref(),
            Some("mbx-state-2")
        );
    }

    /// Regression: a `cannotCalculateChanges` raised by the
    /// *continuation* call to `Mailbox/changes` (i.e. on the
    /// second batch when `hasMoreChanges: true` on the first)
    /// must fall through to a full pull, exactly like the same
    /// error on the first batch already does. Previous shape of
    /// `sync_mailboxes` only handled divergence on the initial
    /// call and propagated it from continuations — which left the
    /// local cache wedged against a stale cursor whenever the
    /// server's change-log eviction landed mid-pagination.
    #[tokio::test]
    async fn mailbox_changes_continuation_divergence_falls_back_to_full_pull() {
        use std::sync::atomic::{AtomicU32, Ordering};
        use std::sync::Arc;
        use wiremock::matchers::body_string_contains;

        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");
        mount_session(&server).await;

        let mailbox_get_calls = Arc::new(AtomicU32::new(0));
        let mailbox_changes_calls = Arc::new(AtomicU32::new(0));

        struct GetResponder {
            counter: Arc<AtomicU32>,
        }
        impl wiremock::Respond for GetResponder {
            fn respond(&self, _req: &wiremock::Request) -> ResponseTemplate {
                let n = self.counter.fetch_add(1, Ordering::SeqCst) + 1;
                // First call: state = "mbx-1" so the next sync
                // takes the changes path. Second call (the
                // full-pull recovery): state advances to
                // "mbx-recovered" so we can assert it landed.
                let state = if n == 1 { "mbx-1" } else { "mbx-recovered" };
                ResponseTemplate::new(200).set_body_json(serde_json::json!({
                    "sessionState": "s-1",
                    "methodResponses": [
                        ["Mailbox/get", {
                            "accountId": "acct-1",
                            "state": state,
                            "list": [
                                {"id": "mbx-inbox", "name": "Inbox", "role": "inbox"},
                                {"id": "mbx-arch", "name": "Archive", "role": "archive"}
                            ],
                            "notFound": []
                        }, "c0"]
                    ]
                }))
            }
        }

        // Mailbox/changes responder:
        //   1st call (since=mbx-1):     hasMoreChanges=true,
        //                               new_state=mbx-2.
        //   2nd call (since=mbx-2):     methodErrors with
        //                               cannotCalculateChanges →
        //                               must NOT propagate;
        //                               must trigger full pull.
        struct ChangesResponder {
            counter: Arc<AtomicU32>,
        }
        impl wiremock::Respond for ChangesResponder {
            fn respond(&self, _req: &wiremock::Request) -> ResponseTemplate {
                let n = self.counter.fetch_add(1, Ordering::SeqCst) + 1;
                if n == 1 {
                    ResponseTemplate::new(200).set_body_json(serde_json::json!({
                        "sessionState": "s-1",
                        "methodResponses": [
                            ["Mailbox/changes", {
                                "accountId": "acct-1",
                                "oldState": "mbx-1",
                                "newState": "mbx-2",
                                "hasMoreChanges": true,
                                "created": [],
                                "updated": [],
                                "destroyed": []
                            }, "c0"]
                        ]
                    }))
                } else {
                    ResponseTemplate::new(200).set_body_json(serde_json::json!({
                        "sessionState": "s-1",
                        "methodResponses": [
                            ["error", {
                                "type": "cannotCalculateChanges",
                                "description": "state too old"
                            }, "c0"]
                        ]
                    }))
                }
            }
        }

        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/get\""))
            .respond_with(GetResponder {
                counter: Arc::clone(&mailbox_get_calls),
            })
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/changes\""))
            .respond_with(ChangesResponder {
                counter: Arc::clone(&mailbox_changes_calls),
            })
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/query\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(bootstrap_email_window_body()))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(email_get_body()))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/changes\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["Email/changes", {
                        "accountId": "acct-1",
                        "oldState": "e-state-1",
                        "newState": "e-state-1",
                        "hasMoreChanges": false,
                        "created": [],
                        "updated": [],
                        "destroyed": []
                    }, "c0"]
                ]
            })))
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "test-token", db);
        let client = KMailClient::open(cfg).unwrap();

        // First sync: full pull. State token persisted as "mbx-1".
        let _first = client.sync().await.unwrap();
        assert_eq!(mailbox_get_calls.load(Ordering::SeqCst), 1);
        assert_eq!(mailbox_changes_calls.load(Ordering::SeqCst), 0);

        // Second sync:
        //   1) Mailbox/changes(mbx-1) -> ok, hasMore=true, newState=mbx-2
        //   2) Mailbox/changes(mbx-2) -> cannotCalculateChanges
        //   3) Fallback to full Mailbox/get → state=mbx-recovered.
        let _second = client.sync().await.unwrap();
        assert_eq!(
            mailbox_changes_calls.load(Ordering::SeqCst),
            2,
            "both pagination batches must be issued before falling back"
        );
        assert_eq!(
            mailbox_get_calls.load(Ordering::SeqCst),
            2,
            "the continuation divergence must trigger a recovery full pull"
        );
        assert_eq!(
            client
                .state_repo
                .get(SyncTypeName::Mailbox)
                .unwrap()
                .as_deref(),
            Some("mbx-recovered"),
            "the recovery full-pull state token must replace the stale cursor"
        );
    }

    /// Regression: `sync_mailboxes` must drain ALL pages when
    /// the server reports `hasMoreChanges: true` across multiple
    /// continuation batches. Previously the loop issued a single
    /// follow-up call and dropped any additional pages on the
    /// floor, forcing convergence over multiple user-visible
    /// `sync()` invocations.
    #[tokio::test]
    async fn sync_mailboxes_drains_changes_until_has_more_is_false() {
        use std::sync::atomic::{AtomicU32, Ordering};
        use std::sync::Arc;
        use wiremock::matchers::body_string_contains;

        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");
        mount_session(&server).await;

        // Mailbox/get returns state="mbx-1" once on first sync.
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/get\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(mailbox_get_body()))
            .mount(&server)
            .await;

        // 3-step Mailbox/changes pagination:
        //   step 1 (since=mbx-1)  -> hasMore=true, new_state=mbx-2
        //   step 2 (since=mbx-2)  -> hasMore=true, new_state=mbx-3
        //   step 3 (since=mbx-3)  -> hasMore=false, new_state=mbx-final
        let changes_calls = Arc::new(AtomicU32::new(0));
        struct PaginatingResponder {
            counter: Arc<AtomicU32>,
        }
        impl wiremock::Respond for PaginatingResponder {
            fn respond(&self, _req: &wiremock::Request) -> ResponseTemplate {
                let n = self.counter.fetch_add(1, Ordering::SeqCst) + 1;
                let (new_state, more) = match n {
                    1 => ("mbx-2", true),
                    2 => ("mbx-3", true),
                    _ => ("mbx-final", false),
                };
                ResponseTemplate::new(200).set_body_json(serde_json::json!({
                    "sessionState": "s-1",
                    "methodResponses": [
                        ["Mailbox/changes", {
                            "accountId": "acct-1",
                            "oldState": format!("mbx-{n}"),
                            "newState": new_state,
                            "hasMoreChanges": more,
                            "created": [],
                            "updated": [],
                            "destroyed": []
                        }, "c0"]
                    ]
                }))
            }
        }
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Mailbox/changes\""))
            .respond_with(PaginatingResponder {
                counter: Arc::clone(&changes_calls),
            })
            .mount(&server)
            .await;

        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/query\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(bootstrap_email_window_body()))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/get\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(email_get_body()))
            .mount(&server)
            .await;
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/changes\""))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["Email/changes", {
                        "accountId": "acct-1",
                        "oldState": "e-state-1",
                        "newState": "e-state-1",
                        "hasMoreChanges": false,
                        "created": [],
                        "updated": [],
                        "destroyed": []
                    }, "c0"]
                ]
            })))
            .mount(&server)
            .await;

        let cfg = ClientConfig::new(server.uri(), "test-token", db);
        let client = KMailClient::open(cfg).unwrap();

        let _first = client.sync().await.unwrap();
        // Second sync drains all three Mailbox/changes pages in
        // ONE invocation. The previous single-follow-up shape
        // would have called `mailbox_changes` only twice and
        // stalled at state=mbx-3 with one unconsumed page.
        let _second = client.sync().await.unwrap();
        assert_eq!(
            changes_calls.load(Ordering::SeqCst),
            3,
            "every paginated batch must be drained in one sync"
        );
        assert_eq!(
            client
                .state_repo
                .get(SyncTypeName::Mailbox)
                .unwrap()
                .as_deref(),
            Some("mbx-final"),
            "the final page's state token must be the persisted cursor"
        );
    }

    /// Regression: an action that has already exceeded the
    /// retry-attempt ceiling must be drained from the queue
    /// (counted as `failed`) WITHOUT another network call —
    /// previous behaviour would keep retrying forever and let
    /// the queue accumulate.
    #[tokio::test]
    async fn flush_pending_actions_drops_actions_past_retry_ceiling() {
        use std::sync::atomic::{AtomicU32, Ordering};
        use std::sync::Arc;
        use wiremock::matchers::body_string_contains;

        let server = MockServer::start().await;
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");
        mount_session(&server).await;

        // Asserts NO Email/set request is issued for the stuck
        // action — if the circuit breaker were missing, the
        // flush loop would call this responder.
        let email_set_calls = Arc::new(AtomicU32::new(0));
        struct AssertNoCall {
            counter: Arc<AtomicU32>,
        }
        impl wiremock::Respond for AssertNoCall {
            fn respond(&self, _req: &wiremock::Request) -> ResponseTemplate {
                self.counter.fetch_add(1, Ordering::SeqCst);
                ResponseTemplate::new(500)
            }
        }
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(body_string_contains("\"Email/set\""))
            .respond_with(AssertNoCall {
                counter: Arc::clone(&email_set_calls),
            })
            .mount(&server)
            .await;

        let mut cfg = ClientConfig::new(server.uri(), "test-token", db);
        cfg.account_id = Some("acct-1".into());
        let client = KMailClient::open(cfg).unwrap();

        // Enqueue an action and manually bump its `attempts` to
        // the ceiling so the circuit breaker fires on the next
        // flush.
        let id = client
            .actions_repo
            .enqueue(
                PendingActionKind::SetKeywords,
                "stuck-1",
                &serde_json::json!({"keywords/$seen": true}),
            )
            .unwrap();
        for _ in 0..MAX_PENDING_ACTION_ATTEMPTS {
            client
                .actions_repo
                .record_failure(id, "503 from BFF")
                .unwrap();
        }

        let session = client.discover_session().await.unwrap();
        let outcome = client
            .flush_pending_actions(&session, "acct-1", 10)
            .await
            .unwrap();

        assert_eq!(
            email_set_calls.load(Ordering::SeqCst),
            0,
            "the circuit breaker must drop the action without another network call"
        );
        assert_eq!(outcome.applied, 0);
        assert_eq!(
            outcome.failed, 1,
            "an exhausted-retry action must be counted under `failed`"
        );
        assert_eq!(outcome.deferred, 0);
        assert_eq!(
            client.actions_repo.count().unwrap(),
            0,
            "the stuck action must be removed from the queue"
        );
    }

    // ---------------------------------------------------------
    // Crypto round-trip tests against the public `KMailClient`
    // surface. These intentionally do NOT mount a wiremock — the
    // SDK's seal / open methods are pure functions of their
    // arguments and the plugged `MlsKeyProvider`, so there is no
    // network to mock.
    //
    // The point of routing through `KMailClient::open` (with a
    // throwaway SQLite path) rather than calling
    // `crypto::vault::seal` directly is to pin the FFI / napi
    // surface — these are exactly the entry points the platform
    // shells call.
    // ---------------------------------------------------------

    /// `KMailClient::open` requires non-empty bff_url + token
    /// even when the only path we exercise is the crypto API.
    /// This helper builds a minimal client for the crypto tests.
    fn open_crypto_only_client() -> (KMailClient, tempfile::TempDir) {
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail.db");
        // The bff_url must be syntactically present; no network
        // calls happen in these tests so the value doesn't
        // matter.
        let cfg = ClientConfig::new("http://localhost", "test-token", db);
        let client = KMailClient::open(cfg).unwrap();
        (client, dir)
    }

    #[test]
    fn vault_envelope_seal_decrypt_roundtrip_via_client_surface() {
        let (client, _dir) = open_crypto_only_client();
        let key = [0x42u8; 32];
        let env = client
            .seal_vault_envelope(&key, b"vault payload", b"email:1 mbx:vault")
            .unwrap();
        let pt = client.decrypt_vault_envelope(&key, &env).unwrap();
        assert_eq!(pt, b"vault payload");
    }

    #[test]
    fn vault_envelope_wrong_key_fails_via_client_surface() {
        let (client, _dir) = open_crypto_only_client();
        let key = [0x42u8; 32];
        let env = client.seal_vault_envelope(&key, b"secret", b"aad").unwrap();
        let mut wrong = key;
        wrong[0] ^= 0x01;
        let err = client.decrypt_vault_envelope(&wrong, &env).unwrap_err();
        assert!(matches!(err, Error::Decryption(_)));
    }

    #[test]
    fn confidential_envelope_seal_open_roundtrip_via_client_surface() {
        let (client, _dir) = open_crypto_only_client();
        let secret = [0x77u8; 32];
        let env = client
            .seal_confidential_envelope(
                &secret,
                b"confidential payload",
                b"msg-1 to alice",
                b"alice-wrap",
            )
            .unwrap();
        let pt = client.open_confidential_envelope(&secret, &env).unwrap();
        assert_eq!(pt, b"confidential payload");
    }

    #[test]
    fn confidential_envelope_wrong_secret_fails_via_client_surface() {
        let (client, _dir) = open_crypto_only_client();
        let secret = [0x77u8; 32];
        let env = client
            .seal_confidential_envelope(&secret, b"secret", b"aad", b"wrap-aad")
            .unwrap();
        let mut wrong = secret;
        wrong[0] ^= 0x01;
        let err = client.open_confidential_envelope(&wrong, &env).unwrap_err();
        assert!(matches!(err, Error::Decryption(_)));
    }

    #[tokio::test]
    async fn mls_provider_vault_write_open_roundtrip() {
        use crate::crypto::StaticMlsKeyProvider;
        use std::sync::Arc;

        let (client, _dir) = open_crypto_only_client();
        let folder_secret = [0xA1u8; 32];
        let provider = Arc::new(
            StaticMlsKeyProvider::new().with_vault_secret("folder-vault-1", &folder_secret),
        );
        client.set_mls_provider(provider).await;

        let env = client
            .write_vault_message("folder-vault-1", b"vault body", b"aad")
            .await
            .unwrap();
        let pt = client
            .open_vault_message("folder-vault-1", &env)
            .await
            .unwrap();
        assert_eq!(pt, b"vault body");
    }

    #[tokio::test]
    async fn mls_provider_confidential_encrypt_decrypt_roundtrip() {
        use crate::crypto::StaticMlsKeyProvider;
        use std::sync::Arc;

        let (client, _dir) = open_crypto_only_client();
        let alice_secret = [0xB2u8; 32];
        let provider = Arc::new(
            StaticMlsKeyProvider::new().with_confidential_secret("alice@kmail.test", &alice_secret),
        );
        client.set_mls_provider(provider).await;

        let env = client
            .encrypt_confidential_message(
                "alice@kmail.test",
                b"confidential body",
                b"msg-1",
                b"alice-wrap",
            )
            .await
            .unwrap();
        let pt = client
            .decrypt_confidential_message("alice@kmail.test", &env)
            .await
            .unwrap();
        assert_eq!(pt, b"confidential body");
    }

    /// Cross-recipient isolation: a Confidential Send envelope
    /// sealed for alice MUST NOT be openable under bob's leaf
    /// secret. This is the load-bearing property that lets the
    /// SDK ship `encrypt_confidential_message` without per-call
    /// recipient verification — recipient identity is bound
    /// cryptographically, not by trust in the BFF's routing.
    #[tokio::test]
    async fn mls_provider_confidential_isolates_recipients() {
        use crate::crypto::StaticMlsKeyProvider;
        use std::sync::Arc;

        let (client, _dir) = open_crypto_only_client();
        let alice_secret = [0xB2u8; 32];
        let bob_secret = [0xC3u8; 32];
        let provider = Arc::new(
            StaticMlsKeyProvider::new()
                .with_confidential_secret("alice@kmail.test", &alice_secret)
                .with_confidential_secret("bob@kmail.test", &bob_secret),
        );
        client.set_mls_provider(provider).await;

        // Seal for alice.
        let env = client
            .encrypt_confidential_message(
                "alice@kmail.test",
                b"for alice's eyes only",
                b"msg-1",
                b"wrap",
            )
            .await
            .unwrap();

        // Bob tries to open. The KEK derives from his leaf
        // secret instead of alice's, so the wrap won't
        // authenticate.
        let err = client
            .decrypt_confidential_message("bob@kmail.test", &env)
            .await
            .unwrap_err();
        assert!(
            matches!(err, Error::Decryption(_)),
            "bob must not be able to open alice's envelope"
        );

        // Alice can still open her own envelope.
        let pt = client
            .decrypt_confidential_message("alice@kmail.test", &env)
            .await
            .unwrap();
        assert_eq!(pt, b"for alice's eyes only");
    }

    #[tokio::test]
    async fn write_vault_message_without_provider_returns_keystore_error() {
        let (client, _dir) = open_crypto_only_client();
        let err = client
            .write_vault_message("folder-1", b"pt", b"aad")
            .await
            .unwrap_err();
        assert!(matches!(err, Error::KeyStore(_)));
    }

    #[tokio::test]
    async fn encrypt_confidential_without_provider_returns_keystore_error() {
        let (client, _dir) = open_crypto_only_client();
        let err = client
            .encrypt_confidential_message("alice@kmail.test", b"pt", b"aad", b"wrap")
            .await
            .unwrap_err();
        assert!(matches!(err, Error::KeyStore(_)));
    }

    #[tokio::test]
    async fn clear_mls_provider_undoes_set() {
        use crate::crypto::StaticMlsKeyProvider;
        use std::sync::Arc;

        let (client, _dir) = open_crypto_only_client();
        let folder_secret = [0xA1u8; 32];
        let provider = Arc::new(
            StaticMlsKeyProvider::new().with_vault_secret("folder-vault-1", &folder_secret),
        );
        client.set_mls_provider(provider).await;
        assert!(client
            .write_vault_message("folder-vault-1", b"pt", b"aad")
            .await
            .is_ok());

        client.clear_mls_provider().await;
        let err = client
            .write_vault_message("folder-vault-1", b"pt", b"aad")
            .await
            .unwrap_err();
        assert!(
            matches!(err, Error::KeyStore(_)),
            "clear_mls_provider must revert to KeyStore error"
        );
    }

    /// End-to-end vault round-trip with an externally-derived
    /// folder master key, exercising the full path that a real
    /// caller would take: HKDF-derive the folder master from a
    /// pretend "MLS credential exporter output" via the SDK's own
    /// kdf module, plug it into a StaticMlsKeyProvider as if it
    /// came from the KChat MLS SDK, seal a payload through the
    /// client surface, ship the envelope's bytes through a JSON
    /// round-trip (simulating BFF persistence + transit), and
    /// open via the same client.
    ///
    /// This is the load-bearing integration test that pins the
    /// public surface contract end-to-end. If any of the steps —
    /// HKDF, KdfLabel binding, AeadEnvelope wire format, provider
    /// lookup, vault seal/open — regresses, this test breaks.
    #[tokio::test]
    async fn end_to_end_vault_with_kdf_derived_folder_master() {
        use crate::crypto::{hkdf_derive, KdfLabel, StaticMlsKeyProvider};
        use std::sync::Arc;

        let (client, _dir) = open_crypto_only_client();

        // Simulate the MLS credential exporter output by HKDF-
        // deriving a 32-byte folder master from an arbitrary
        // 32-byte "MLS credential secret" + a folder-specific
        // salt. A real KChat MLS SDK would do this via its own
        // exporter; here we mimic the math directly.
        let credential_secret = [0x5Au8; 32];
        let folder_salt = b"folder-vault-42";
        let folder_master = hkdf_derive(
            folder_salt,
            &credential_secret,
            KdfLabel::VaultFolderMaster,
            32,
        )
        .unwrap();
        assert_eq!(folder_master.len(), 32);

        let provider = Arc::new(
            StaticMlsKeyProvider::new().with_vault_secret("folder-vault-42", &folder_master),
        );
        client.set_mls_provider(provider).await;

        // Seal a payload of representative size for an email
        // body. 8 KiB is the median Stalwart body size in
        // production.
        let body = vec![0x37u8; 8 * 1024];
        let aad = b"email:e-42 mbx:vault-42 epoch:1";

        let env = client
            .write_vault_message("folder-vault-42", &body, aad)
            .await
            .unwrap();

        // Serialise the envelope as if shipped through the BFF
        // (base64 of nonce + ciphertext; AAD recovered from
        // mailbox metadata). This catches any wire-format drift
        // in the AeadEnvelope shape.
        use base64::Engine as _;
        let b64 = base64::engine::general_purpose::STANDARD;
        let nonce_b64 = b64.encode(env.nonce);
        let ct_b64 = b64.encode(&env.ciphertext);
        // Reconstruct the envelope from its serialised form.
        let nonce_bytes = b64.decode(nonce_b64).unwrap();
        let mut nonce_arr = [0u8; crate::crypto::NONCE_LEN];
        nonce_arr.copy_from_slice(&nonce_bytes);
        let rebuilt = AeadEnvelope {
            nonce: nonce_arr,
            ciphertext: b64.decode(ct_b64).unwrap(),
            aad: aad.to_vec(),
        };
        assert_eq!(rebuilt, env, "JSON round-trip must preserve envelope");

        let pt = client
            .open_vault_message("folder-vault-42", &rebuilt)
            .await
            .unwrap();
        assert_eq!(pt, body, "round-tripped envelope must yield original body");
    }

    // -----------------------------------------------------------
    // Push ingestion → local notification + preview-row caching.
    // -----------------------------------------------------------

    fn delivery_data(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
            .collect()
    }

    /// A `new_email` push must (a) cache a preview row so the inbox
    /// updates instantly, (b) hand back a renderable notification,
    /// and (c) leave the `Email` sync cursor untouched — the push
    /// state token is a snapshot id, not a safe delta cursor.
    #[test]
    fn ingest_push_delivery_caches_preview_and_builds_notification() {
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-push.db");
        let mut cfg = ClientConfig::new("https://kmail.example.com", "tok", db);
        cfg.account_id = Some("acct-1".into());
        let client = KMailClient::open(cfg).unwrap();

        // A push normally arrives after the device has logged in and
        // pulled its mailboxes (push registration happens post-sync),
        // so seed the inbox the hint references.
        client
            .mailbox_repo
            .upsert(&Mailbox {
                id: "mb-inbox".into(),
                name: "Inbox".into(),
                role: Some(MailboxRole::Inbox),
                parent_id: None,
                sort_order: 0,
                total_emails: 0,
                unread_emails: 0,
                total_threads: 0,
                unread_threads: 0,
                is_vault: false,
                my_rights: None,
            })
            .unwrap();

        // Precondition: cold email cache, no Email state cursor.
        assert!(client.state_repo.get(SyncTypeName::Email).unwrap().is_none());

        let data = delivery_data(&[
            ("account_id", "acct-1"),
            ("email_id", "e-100"),
            ("mailbox_id", "mb-inbox"),
            ("thread_id", "t-1"),
            ("from", "Bob <bob@example.com>"),
            ("subject", "Hi"),
            ("snippet", "hello there"),
            ("email_state", "srv-state-xyz"),
            ("has_attachment", "true"),
            ("received_at_unix", "1700000000"),
        ]);
        let outcome = client.ingest_push_delivery(&data).unwrap();
        assert!(outcome.email_cached);
        assert!(outcome.needs_delta_sync);
        let n = outcome.notification.expect("notification built from hint");
        assert_eq!(n.title, "Bob <bob@example.com>");
        assert_eq!(n.body, "Hi");
        assert_eq!(n.email_id.as_deref(), Some("e-100"));
        assert_eq!(n.tag, "e-100");

        // The preview row landed in the inbox mailbox with a
        // best-effort parsed sender.
        let rows = client.cached_emails_in_mailbox("mb-inbox", 10).unwrap();
        assert_eq!(rows.len(), 1, "preview row should be cached in the inbox");
        assert_eq!(rows[0].id, "e-100");
        assert_eq!(rows[0].subject, "Hi");
        assert!(rows[0].has_attachment);
        assert_eq!(rows[0].from.len(), 1);
        assert_eq!(rows[0].from[0].name, "Bob");
        assert_eq!(rows[0].from[0].email, "bob@example.com");

        // Cursor invariant: a push must not adopt the server state
        // token as the `Email/changes` cursor.
        assert!(
            client.state_repo.get(SyncTypeName::Email).unwrap().is_none(),
            "push delivery must not advance the Email sync cursor"
        );
    }

    /// A metadata-only / malformed payload (no `email_id`) can't be
    /// short-circuited: no notification, nothing cached, but the
    /// shell is still told to run a delta sync.
    #[test]
    fn ingest_push_delivery_malformed_falls_back_to_full_sync() {
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-push-bad.db");
        let client =
            KMailClient::open(ClientConfig::new("https://kmail.example.com", "tok", db)).unwrap();

        let data = delivery_data(&[("@type", "StateChange"), ("account_id", "acct-1")]);
        let outcome = client.ingest_push_delivery(&data).unwrap();
        assert!(outcome.notification.is_none());
        assert!(!outcome.email_cached);
        assert!(outcome.needs_delta_sync);
    }

    /// A push naming a mailbox the device hasn't synced yet must not
    /// fail (the `email_mailboxes` FK would reject the membership):
    /// we cache the email headers without membership and still build
    /// the notification. The next sync links it into the mailbox.
    #[test]
    fn ingest_push_delivery_without_cached_mailbox_caches_headers_only() {
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-push-nomb.db");
        let mut cfg = ClientConfig::new("https://kmail.example.com", "tok", db);
        cfg.account_id = Some("acct-1".into());
        let client = KMailClient::open(cfg).unwrap();

        let data = delivery_data(&[
            ("email_id", "e-200"),
            ("mailbox_id", "mb-brand-new-label"),
            ("subject", "Welcome"),
        ]);
        let outcome = client.ingest_push_delivery(&data).unwrap();
        assert!(outcome.email_cached, "headers should still cache");
        assert!(outcome.notification.is_some());

        // The row exists in the email cache...
        let row = client.email_repo.get("e-200").unwrap().expect("email row");
        assert_eq!(row.subject, "Welcome");
        assert!(
            row.mailbox_ids.is_empty(),
            "membership must be skipped for an unsynced mailbox"
        );
        // ...but is not (yet) linked into the unsynced mailbox.
        assert!(client
            .cached_emails_in_mailbox("mb-brand-new-label", 10)
            .unwrap()
            .is_empty());
    }

    // -----------------------------------------------------------
    // Background sync worker — loop + cancellation semantics.
    // -----------------------------------------------------------

    /// The periodic worker fires its tick repeatedly and stops
    /// cleanly: no further ticks run after `stop_and_join`.
    #[tokio::test]
    async fn background_worker_ticks_then_stops() {
        use std::sync::atomic::{AtomicU64, Ordering};
        let counter = Arc::new(AtomicU64::new(0));
        let c = counter.clone();
        let handle = spawn_periodic(Duration::from_millis(20), move || {
            let c = c.clone();
            async move {
                c.fetch_add(1, Ordering::SeqCst);
            }
        });

        tokio::time::sleep(Duration::from_millis(120)).await;
        let ticks = counter.load(Ordering::SeqCst);
        assert!(ticks >= 2, "worker should have ticked at least twice, got {ticks}");

        handle.stop_and_join().await;
        let after_stop = counter.load(Ordering::SeqCst);
        tokio::time::sleep(Duration::from_millis(80)).await;
        assert_eq!(
            counter.load(Ordering::SeqCst),
            after_stop,
            "no ticks may run after stop_and_join"
        );
    }

    /// Cancelling before the first interval elapses must run zero
    /// ticks (the first sync fires after `interval`, not at t=0).
    #[tokio::test]
    async fn background_worker_cancel_before_first_tick_runs_nothing() {
        use std::sync::atomic::{AtomicU64, Ordering};
        let counter = Arc::new(AtomicU64::new(0));
        let c = counter.clone();
        let handle = spawn_periodic(Duration::from_secs(3600), move || {
            let c = c.clone();
            async move {
                c.fetch_add(1, Ordering::SeqCst);
            }
        });
        handle.stop_and_join().await;
        assert_eq!(counter.load(Ordering::SeqCst), 0);
    }

    /// A failing `sync()` (here: BFF returns 404 for the session
    /// document) must not kill the worker — it logs and retries on
    /// the next tick, and the handle still shuts down cleanly.
    #[tokio::test]
    async fn background_sync_swallows_errors_and_can_be_stopped() {
        let server = MockServer::start().await; // no routes mounted → 404
        let dir = tempfile::tempdir().unwrap();
        let db = dir.path().join("kmail-bgsync.db");
        let client = KMailClient::open(ClientConfig::new(server.uri(), "tok", db)).unwrap();

        let handle = client.spawn_background_sync(Duration::from_millis(15));
        tokio::time::sleep(Duration::from_millis(70)).await;
        // Reaching here (rather than the test task aborting) is the
        // assertion: the failing syncs were swallowed and the loop
        // kept running. Shutdown must also complete promptly.
        handle.stop_and_join().await;
    }
}
