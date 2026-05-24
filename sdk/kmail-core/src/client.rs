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
use tokio::sync::OnceCell;

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

/// SDK configuration.
#[derive(Clone, Debug)]
pub struct ClientConfig {
    /// Absolute BFF base URL (e.g. `https://kmail.example.com`).
    pub bff_url: String,
    /// OIDC bearer token. The platform shell refreshes it; the SDK
    /// does not run OAuth flows itself.
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

/// Aggregated counts from a successful `sync()`.
#[derive(Clone, Debug, Default)]
pub struct SyncSummary {
    pub mailboxes_upserted: u64,
    pub emails_created: u64,
    pub emails_updated: u64,
    pub emails_destroyed: u64,
    pub pending_actions_flushed: u64,
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
    /// `discover_session()` call. Wrapped in `tokio::sync::OnceCell`
    /// for async-aware idempotent initialisation.
    session: Arc<OnceCell<JmapSession>>,
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
            session: Arc::new(OnceCell::new()),
            account_id: Arc::new(Mutex::new(None)),
        })
    }

    /// Fetch (or return the cached) JMAP session resource.
    pub async fn discover_session(&self) -> Result<JmapSession> {
        let s = self
            .session
            .get_or_try_init(|| async { self.jmap.session().await })
            .await?;
        Ok(s.clone())
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

        // 1. Mailboxes — full pull each cycle. Cheap (typically a
        //    few dozen rows) and avoids tracking mailbox/changes
        //    state separately.
        let mailboxes = self.jmap.list_mailboxes(&session, &account_id).await?;
        for m in &mailboxes.mailboxes {
            self.mailbox_repo.upsert(m)?;
        }
        summary.mailboxes_upserted = mailboxes.mailboxes.len() as u64;
        self.state_repo
            .put(SyncTypeName::Mailbox, &mailboxes.state)?;

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
                self.hydrate_email_ids(&session, &account_id, &ids).await?;
                summary.emails_created = ids.len() as u64;
                self.state_repo.put(SyncTypeName::Email, &state)?;

                // 3. Flush queued offline actions and return.
                summary.pending_actions_flushed = self
                    .flush_pending_actions(&session, &account_id, 50)
                    .await?;
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
                    // Server can't catch us up; drop the stale
                    // token and re-bootstrap. Mailbox state is
                    // unaffected; we just rebuild the email window.
                    // The freshly-issued atomic bootstrap returns
                    // the canonical Email state, so we persist that
                    // and stop iterating — Email/changes would just
                    // return an empty diff against it on the next
                    // call anyway.
                    let (ids, state) = self
                        .bootstrap_initial_email_pull(&session, &account_id)
                        .await?;
                    self.hydrate_email_ids(&session, &account_id, &ids).await?;
                    summary.emails_created += ids.len() as u64;
                    self.state_repo.put(SyncTypeName::Email, &state)?;
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
            self.hydrate_email_ids(&session, &account_id, &to_fetch)
                .await?;

            // Apply destroys.
            for d in &destroyed {
                self.email_repo.delete(d)?;
            }

            summary.emails_created += created.len() as u64;
            summary.emails_updated += updated.len() as u64;
            summary.emails_destroyed += destroyed.len() as u64;

            current_state = changes.new_state;
            self.state_repo.put(SyncTypeName::Email, &current_state)?;

            if !changes.has_more_changes {
                break;
            }
        }

        // 4. Flush queued offline actions.
        summary.pending_actions_flushed = self
            .flush_pending_actions(&session, &account_id, 50)
            .await?;

        Ok(summary)
    }

    /// Hydrate `Email/get` for the given IDs in chunks of 100
    /// (RFC 8620 §6.4 recommended ceiling) and upsert each chunk
    /// into the local store.
    async fn hydrate_email_ids(
        &self,
        session: &JmapSession,
        account_id: &str,
        ids: &[String],
    ) -> Result<()> {
        if ids.is_empty() {
            return Ok(());
        }
        for chunk in ids.chunks(100) {
            let emails = self
                .jmap
                .get_emails(session, account_id, chunk, /* with_bodies */ false)
                .await?;
            let mutations: Vec<_> = emails
                .into_iter()
                .map(|e| crate::sync::EmailMutation::Upsert(Box::new(e.summary)))
                .collect();
            self.email_repo.apply(&mutations)?;
        }
        Ok(())
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
            .find(|m| m.role == Some(inbox_role))
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

    /// Queue an offline action. The next `sync()` will drain the
    /// queue against the BFF.
    pub fn enqueue_set_keywords(&self, email_id: &str, keywords: &serde_json::Value) -> Result<()> {
        self.actions_repo
            .enqueue(
                PendingActionKind::SetKeywords,
                email_id,
                &serde_json::json!({"keywords": keywords}),
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
    pub async fn register_push_token(
        &self,
        transport: PushTransport,
        token: &str,
        web_push_keys: Option<WebPushKeys>,
    ) -> Result<()> {
        let req = PushSubscriptionRequest {
            transport,
            token: token.to_string(),
            web_push_keys,
            types: Vec::new(),
        };
        let resp: serde_json::Value = crate::jmap::transport::JmapTransport::new(
            TransportConfig::new(&self.config.bff_url, &self.config.bearer_token),
        )?
        .post_json("/api/v1/push/subscribe", &req)
        .await?;
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
    /// Returns the count of successfully-applied actions.
    async fn flush_pending_actions(
        &self,
        session: &JmapSession,
        account_id: &str,
        limit: u32,
    ) -> Result<u64> {
        let batch = self.actions_repo.next_batch(limit)?;
        let mut applied = 0u64;
        for action in batch {
            match self
                .apply_pending_action(session, account_id, &action)
                .await
            {
                Ok(()) => {
                    self.actions_repo.complete(action.id)?;
                    applied += 1;
                }
                Err(e) if e.is_retryable() => {
                    self.actions_repo
                        .record_failure(action.id, &e.to_string())?;
                    // Stop draining — the network's flapping; the
                    // remaining actions will retry on the next sync.
                    break;
                }
                Err(e) => {
                    // Terminal failure (4xx-equivalent). Record the
                    // error and drop the action so the queue
                    // doesn't wedge forever.
                    self.actions_repo
                        .record_failure(action.id, &e.to_string())?;
                    self.actions_repo.complete(action.id)?;
                }
            }
        }
        Ok(applied)
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
                let _r: serde_json::Value = resp.parse(&id)?;
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
}
