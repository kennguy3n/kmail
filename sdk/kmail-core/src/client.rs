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
use crate::crypto::{aes_gcm_decrypt, hkdf_derive, AeadEnvelope, KdfLabel};
use crate::error::{Error, Result};
use crate::jmap::transport::TransportConfig;
use crate::jmap::JmapClient;
use crate::models::{Email, EmailDraft, EmailSummary, JmapSession, Mailbox};
use crate::push::{PushSubscriptionRequest, PushTransport, WebPushKeys};
use crate::sync::{
    ActionsRepo, EmailRepo, MailboxRepo, PendingAction, PendingActionKind, StateRepo, Store,
    SyncTypeName,
};
use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tokio::sync::RwLock;

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
            session: Arc::new(RwLock::new(None)),
            account_id: Arc::new(Mutex::new(None)),
        })
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
    pub fn decrypt_vault_envelope(
        &self,
        folder_master_key: &[u8],
        envelope: &AeadEnvelope,
    ) -> Result<Vec<u8>> {
        if folder_master_key.len() != 32 {
            return Err(Error::InvalidArgument(
                "folder master key must be 32 bytes".into(),
            ));
        }
        let dek = hkdf_derive(
            &envelope.nonce,
            folder_master_key,
            KdfLabel::VaultFolderMaster,
            32,
        )?;
        aes_gcm_decrypt(&dek, envelope)
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
        let mut processed = 0u64;
        for action in batch {
            processed += 1;

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
                    // `processed - 1` is the count of items the
                    // loop already settled (applied OR failed
                    // terminally) BEFORE this action — those are
                    // off the queue and must not be counted as
                    // deferred. Everything else, including the
                    // current action that just bailed, stays
                    // queued for retry; that's exactly
                    // `total - (processed - 1)`. The
                    // `saturating_sub` is defensive only; the
                    // arithmetic is guaranteed non-negative
                    // because `processed >= 1` here (we bumped
                    // it at the top of the loop iteration) and
                    // `processed <= total` (we only iterate the
                    // batch once).
                    outcome.deferred = total.saturating_sub(processed - 1);
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
}
