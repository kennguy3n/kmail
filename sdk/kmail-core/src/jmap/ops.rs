// `JmapClient` — typed wrappers over the JMAP HTTP API.
//
// Responsibilities:
//   * Discover the session (`GET /jmap/session`).
//   * Batch one or more typed method calls into a single
//     POST against the session's `apiUrl`.
//   * Parse the batched response and surface JMAP-method errors as
//     `Error::JmapMethod`.
//
// What this layer does NOT do:
//   * Persistence — that's `sync::*Repo`.
//   * Higher-level sync orchestration — that's `KMailClient::sync()`.
//
// JMAP back-references are supported (`#callId` resolution): each
// `call_*` helper assigns a stable call ID so the caller can chain
// `Email/query` → `Email/get` in a single round-trip.

use crate::error::{Error, Result};
use crate::jmap::request::{
    EmailChangesArgs, EmailGetArgs, EmailQueryArgs, JmapRequest, MailboxGetArgs, CAP_CORE,
    CAP_MAIL, CAP_SUBMISSION,
};
use crate::jmap::response::{
    EmailChangesResponse, EmailGetResponse, EmailQueryResponse, EmailSubmissionSetResponse,
    JmapResponse, MailboxGetResponse,
};
use crate::jmap::transport::{JmapTransport, TransportConfig};
use crate::models::{Email, EmailDraft, JmapSession, Mailbox};
use std::collections::BTreeMap;

#[derive(Clone)]
pub struct JmapClient {
    transport: JmapTransport,
    /// Path or absolute URL where session discovery happens.
    /// Defaults to `/jmap/session` against the BFF — the BFF
    /// surfaces the session resource at that path per the React
    /// client (`web/src/api/jmap.ts`). The RFC 8620 well-known
    /// path is `/.well-known/jmap`, which 301s to the same place.
    session_path: String,
}

impl JmapClient {
    pub fn new(transport: TransportConfig) -> Result<Self> {
        Ok(Self {
            transport: JmapTransport::new(transport)?,
            session_path: "/jmap/session".to_string(),
        })
    }

    /// Use a non-default session path. Useful when the BFF is
    /// proxied behind a path prefix (`/integrations/kmail/...`).
    pub fn with_session_path(mut self, path: impl Into<String>) -> Self {
        self.session_path = path.into();
        self
    }

    /// `GET /jmap/session` → `JmapSession`.
    pub async fn session(&self) -> Result<JmapSession> {
        self.transport.get_json(&self.session_path).await
    }

    /// Dispatch a pre-built batch request against the session's
    /// `apiUrl`. The session is fetched if not supplied — pass
    /// one in when chaining several batches to avoid the extra
    /// GET each time.
    pub async fn dispatch(
        &self,
        session: &JmapSession,
        request: &JmapRequest,
    ) -> Result<JmapResponse> {
        let api_url = if session.api_url.is_empty() {
            "/jmap/api".to_string()
        } else {
            session.api_url.clone()
        };
        self.transport.post_json(&api_url, request).await
    }

    /// Fetch every mailbox the account can see in a single round-trip.
    pub async fn list_mailboxes(
        &self,
        session: &JmapSession,
        account_id: &str,
    ) -> Result<MailboxesResult> {
        let mut req = JmapRequest::new(vec![CAP_CORE.into(), CAP_MAIL.into()]);
        let id = req.call(
            "Mailbox/get",
            serde_json::to_value(MailboxGetArgs {
                account_id: account_id.to_string(),
                ids: None,
                properties: None,
            })?,
        );
        let resp = self.dispatch(session, &req).await?;
        let r: MailboxGetResponse = resp.parse(&id)?;
        Ok(MailboxesResult {
            state: r.state,
            mailboxes: r.list,
        })
    }

    /// Fetch a specific mailbox by ID.
    pub async fn get_mailbox(
        &self,
        session: &JmapSession,
        account_id: &str,
        mailbox_id: &str,
    ) -> Result<Option<Mailbox>> {
        let mut req = JmapRequest::new(vec![CAP_CORE.into(), CAP_MAIL.into()]);
        let id = req.call(
            "Mailbox/get",
            serde_json::to_value(MailboxGetArgs {
                account_id: account_id.to_string(),
                ids: Some(vec![mailbox_id.to_string()]),
                properties: None,
            })?,
        );
        let resp = self.dispatch(session, &req).await?;
        let r: MailboxGetResponse = resp.parse(&id)?;
        Ok(r.list.into_iter().next())
    }

    /// Query email IDs in a mailbox, newest first.
    pub async fn query_emails_in_mailbox(
        &self,
        session: &JmapSession,
        account_id: &str,
        mailbox_id: &str,
        limit: u32,
    ) -> Result<EmailQueryResponse> {
        let mut req = JmapRequest::new(vec![CAP_CORE.into(), CAP_MAIL.into()]);
        let id = req.call(
            "Email/query",
            serde_json::to_value(EmailQueryArgs {
                account_id: account_id.to_string(),
                filter: Some(serde_json::json!({"inMailbox": mailbox_id})),
                sort: Some(serde_json::json!([
                    {"property": "receivedAt", "isAscending": false}
                ])),
                position: Some(0),
                limit: Some(limit),
                collapse_threads: Some(false),
            })?,
        );
        let resp = self.dispatch(session, &req).await?;
        resp.parse(&id)
    }

    /// Atomic bootstrap: combine `Email/query` (newest-N IDs) with
    /// an `Email/get ids: []` state probe into a single batched
    /// JMAP request. RFC 8620 §3.4 guarantees that all method
    /// calls within one request are processed against the same
    /// server state snapshot, which closes the race window where a
    /// new email arriving between two HTTP round-trips would be
    /// permanently missed from the local cache (the state token
    /// from a later `Email/get` would already cover it, so the
    /// next `Email/changes` would not return it either).
    ///
    /// Returns `(ids_newest_first, canonical_email_state)`.
    pub async fn bootstrap_email_window(
        &self,
        session: &JmapSession,
        account_id: &str,
        mailbox_id: &str,
        limit: u32,
    ) -> Result<(Vec<String>, String)> {
        let mut req = JmapRequest::new(vec![CAP_CORE.into(), CAP_MAIL.into()]);

        let query_id = req.call(
            "Email/query",
            serde_json::to_value(EmailQueryArgs {
                account_id: account_id.to_string(),
                filter: Some(serde_json::json!({"inMailbox": mailbox_id})),
                sort: Some(serde_json::json!([
                    {"property": "receivedAt", "isAscending": false}
                ])),
                position: Some(0),
                limit: Some(limit),
                collapse_threads: Some(false),
            })?,
        );

        // `Email/get` with an empty `ids` array is the canonical
        // JMAP way to ask "what is the current state token for
        // this type?" — RFC 8620 §5.1 explicitly requires the
        // server to return `state` regardless of how many objects
        // matched. Co-locating it in the same request as the query
        // is what gives us the atomicity guarantee.
        let state_id = req.call(
            "Email/get",
            serde_json::to_value(EmailGetArgs {
                account_id: account_id.to_string(),
                ids: Vec::new(),
                properties: None,
                fetch_text_body_values: None,
                fetch_html_body_values: None,
                fetch_all_body_values: None,
            })?,
        );

        let resp = self.dispatch(session, &req).await?;
        let q: EmailQueryResponse = resp.parse(&query_id)?;
        let g: EmailGetResponse = resp.parse(&state_id)?;
        Ok((q.ids, g.state))
    }

    /// Fetch full `Email` payloads by ID.
    pub async fn get_emails(
        &self,
        session: &JmapSession,
        account_id: &str,
        ids: &[String],
        with_bodies: bool,
    ) -> Result<Vec<Email>> {
        if ids.is_empty() {
            return Ok(Vec::new());
        }
        let mut req = JmapRequest::new(vec![CAP_CORE.into(), CAP_MAIL.into()]);
        let id = req.call(
            "Email/get",
            serde_json::to_value(EmailGetArgs {
                account_id: account_id.to_string(),
                ids: ids.to_vec(),
                properties: None,
                fetch_text_body_values: Some(with_bodies),
                fetch_html_body_values: Some(with_bodies),
                fetch_all_body_values: None,
            })?,
        );
        let resp = self.dispatch(session, &req).await?;
        let r: EmailGetResponse = resp.parse(&id)?;
        Ok(r.list)
    }

    /// Incremental sync — fetch the change set since `since_state`.
    pub async fn email_changes(
        &self,
        session: &JmapSession,
        account_id: &str,
        since_state: &str,
    ) -> Result<EmailChangesResponse> {
        let mut req = JmapRequest::new(vec![CAP_CORE.into(), CAP_MAIL.into()]);
        let id = req.call(
            "Email/changes",
            serde_json::to_value(EmailChangesArgs {
                account_id: account_id.to_string(),
                since_state: since_state.to_string(),
                max_changes: Some(500),
            })?,
        );
        let resp = self.dispatch(session, &req).await?;
        let parsed: Result<EmailChangesResponse> = resp.parse(&id);
        match parsed {
            Ok(v) => Ok(v),
            Err(Error::JmapMethod {
                code,
                description: _,
            }) if code.ends_with("cannotCalculateChanges") => Err(Error::SyncStateDiverged),
            Err(other) => Err(other),
        }
    }

    /// Persist a draft + submit it via `EmailSubmission/set`.
    /// Returns the server-assigned Email ID.
    ///
    /// The `Email/set` response is parsed first and any
    /// `notCreated` entry is surfaced as the underlying JMAP
    /// method error (`overQuota`, `tooManyKeywords`, ...) BEFORE
    /// looking at `EmailSubmission/set`. If we skipped that and
    /// went straight to the submission response, the user would
    /// see `invalidResultReference` (because the `#draft1`
    /// back-reference failed to resolve) instead of the actual
    /// failure reason — bad UX and bad telemetry.
    pub async fn send_email(
        &self,
        session: &JmapSession,
        account_id: &str,
        draft: &EmailDraft,
    ) -> Result<String> {
        if draft.to.is_empty() && draft.cc.is_empty() && draft.bcc.is_empty() {
            return Err(Error::InvalidArgument(
                "draft must have at least one recipient".into(),
            ));
        }
        let mut req = JmapRequest::new(vec![
            CAP_CORE.into(),
            CAP_MAIL.into(),
            CAP_SUBMISSION.into(),
        ]);

        let draft_value = serde_json::to_value(draft)?;
        let mut create_map = serde_json::Map::new();
        create_map.insert("draft1".to_string(), draft_value);

        let create_id = req.call(
            "Email/set",
            serde_json::json!({
                "accountId": account_id,
                "create": serde_json::Value::Object(create_map),
            }),
        );

        let submit_id = req.call(
            "EmailSubmission/set",
            serde_json::json!({
                "accountId": account_id,
                "create": {
                    "submit1": {
                        "emailId": format!("#{}", "draft1"),
                        "identityId": null,
                        "envelope": null,
                    }
                },
                "onSuccessUpdateEmail": {
                    "#submit1": {
                        "keywords/$draft": null
                    }
                }
            }),
        );

        let resp = self.dispatch(session, &req).await?;

        // 1. Surface `Email/set` creation errors first. If draft
        //    creation failed, the EmailSubmission/set back-reference
        //    would have produced a confusing `invalidResultReference`
        //    response — return the real reason instead.
        let create_resp: crate::jmap::response::EmailSetResponse = resp.parse(&create_id)?;
        if let Some((_, err)) = create_resp.not_created.into_iter().next() {
            return Err(Error::JmapMethod {
                code: err.r#type,
                description: err.description.unwrap_or_default(),
            });
        }

        // 2. Now parse the submission response normally.
        let sub: EmailSubmissionSetResponse = resp.parse(&submit_id)?;
        if let Some((_, err)) = sub.not_created.into_iter().next() {
            return Err(Error::JmapMethod {
                code: err.r#type,
                description: err.description.unwrap_or_default(),
            });
        }

        // 3. Resolve `#draft1` against the response's `createdIds`
        //    (server-assigned ID for the creation tag) or the
        //    submission's echo, whichever is populated.
        let created_id = resp
            .created_ids
            .get("draft1")
            .cloned()
            .or_else(|| {
                create_resp
                    .created
                    .get("draft1")
                    .and_then(|v| v.get("id"))
                    .and_then(|v| v.as_str())
                    .map(String::from)
            })
            .or_else(|| {
                sub.created
                    .get("submit1")
                    .and_then(|v| v.get("emailId"))
                    .and_then(|v| v.as_str())
                    .map(String::from)
            })
            .ok_or_else(|| Error::Protocol("EmailSubmission/set returned no email ID".into()))?;
        Ok(created_id)
    }
}

/// Result of `list_mailboxes` — the mailbox set plus the state
/// token the caller should persist for incremental sync.
#[derive(Clone, Debug)]
pub struct MailboxesResult {
    pub state: String,
    pub mailboxes: Vec<Mailbox>,
}

/// Convenience map type used by the FFI / napi wrappers.
pub type Headers = BTreeMap<String, String>;

#[cfg(test)]
mod tests {
    use super::*;
    use crate::models::{JmapAccount, JmapSession};
    use wiremock::matchers::{header, method, path};
    use wiremock::{Mock, MockServer, ResponseTemplate};

    fn fake_session(api: &str) -> JmapSession {
        let mut accounts = BTreeMap::new();
        accounts.insert(
            "acct-1".into(),
            JmapAccount {
                name: "Test".into(),
                is_personal: true,
                is_read_only: false,
                account_capabilities: BTreeMap::new(),
            },
        );
        JmapSession {
            api_url: api.into(),
            event_source_url: String::new(),
            upload_url: String::new(),
            download_url: String::new(),
            accounts,
            primary_accounts: BTreeMap::new(),
            capabilities: vec![CAP_CORE.into(), CAP_MAIL.into()],
            username: "test@example.com".into(),
            state: String::new(),
        }
    }

    async fn fresh_client(server: &MockServer) -> JmapClient {
        JmapClient::new(TransportConfig::new(server.uri(), "test-token")).unwrap()
    }

    #[tokio::test]
    async fn session_round_trip() {
        let server = MockServer::start().await;
        Mock::given(method("GET"))
            .and(path("/jmap/session"))
            .and(header("authorization", "Bearer test-token"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "apiUrl": "/jmap/api",
                "capabilities": {"urn:ietf:params:jmap:core": {}, "urn:ietf:params:jmap:mail": {}},
                "username": "alice@example.com",
                "accounts": {
                    "acct-1": {
                        "name": "Alice",
                        "isPersonal": true,
                        "isReadOnly": false,
                        "accountCapabilities": {}
                    }
                },
                "primaryAccounts": {"urn:ietf:params:jmap:mail": "acct-1"}
            })))
            .expect(1)
            .mount(&server)
            .await;

        let client = fresh_client(&server).await;
        let sess = client.session().await.unwrap();
        assert_eq!(sess.api_url, "/jmap/api");
        assert_eq!(sess.username, "alice@example.com");
        assert!(sess.capabilities.iter().any(|c| c == CAP_MAIL));
        assert_eq!(sess.accounts.get("acct-1").unwrap().name, "Alice");
    }

    #[tokio::test]
    async fn list_mailboxes_dispatches_typed_batch() {
        let server = MockServer::start().await;
        let server_uri = server.uri();
        let api_url = format!("{server_uri}/jmap/api");
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .and(header("authorization", "Bearer test-token"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["Mailbox/get", {
                        "accountId": "acct-1",
                        "state": "mbx-state-1",
                        "list": [
                            {"id": "mbx-inbox", "name": "Inbox", "role": "inbox", "totalEmails": 4, "unreadEmails": 2},
                            {"id": "mbx-vault", "name": "Confidential", "role": "vault", "isVault": true}
                        ],
                        "notFound": []
                    }, "c0"]
                ]
            })))
            .expect(1)
            .mount(&server)
            .await;

        let client = fresh_client(&server).await;
        let sess = fake_session(&api_url);
        let r = client.list_mailboxes(&sess, "acct-1").await.unwrap();
        assert_eq!(r.state, "mbx-state-1");
        assert_eq!(r.mailboxes.len(), 2);
        assert!(r
            .mailboxes
            .iter()
            .any(|m| m.id == "mbx-vault" && m.is_vault));
    }

    #[tokio::test]
    async fn jmap_method_error_propagates() {
        let server = MockServer::start().await;
        let api_url = format!("{}/jmap/api", server.uri());
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["error", {
                        "type": "urn:ietf:params:jmap:error:accountNotFound",
                        "description": "no such account"
                    }, "c0"]
                ]
            })))
            .mount(&server)
            .await;

        let client = fresh_client(&server).await;
        let sess = fake_session(&api_url);
        let err = client.list_mailboxes(&sess, "ghost").await.unwrap_err();
        match err {
            Error::JmapMethod { code, description } => {
                assert!(code.ends_with("accountNotFound"));
                assert_eq!(description, "no such account");
            }
            other => panic!("expected JmapMethod, got {other:?}"),
        }
    }

    /// `cannotCalculateChanges` is the JMAP signal for "state token
    /// too stale; re-bootstrap". The SDK must surface it as
    /// `Error::SyncStateDiverged` so the upper layer can drop the
    /// cached state token and re-pull from scratch.
    #[tokio::test]
    async fn cannot_calculate_changes_maps_to_sync_state_diverged() {
        let server = MockServer::start().await;
        let api_url = format!("{}/jmap/api", server.uri());
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["error", {
                        "type": "urn:ietf:params:jmap:error:cannotCalculateChanges",
                        "description": "state too old"
                    }, "c0"]
                ]
            })))
            .mount(&server)
            .await;

        let client = fresh_client(&server).await;
        let sess = fake_session(&api_url);
        let err = client
            .email_changes(&sess, "acct-1", "s-very-old")
            .await
            .unwrap_err();
        assert!(matches!(err, Error::SyncStateDiverged));
    }

    /// 429 from the BFF must surface the `Retry-After` value in
    /// `Error::RateLimit`. The retry budget is exhausted in this
    /// test (max_attempts = 1) so we observe the typed error.
    #[tokio::test]
    async fn rate_limit_carries_retry_after() {
        let server = MockServer::start().await;
        let api_url = format!("{}/jmap/api", server.uri());
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .respond_with(
                ResponseTemplate::new(429)
                    .insert_header("retry-after", "7")
                    .set_body_string("slow down"),
            )
            .mount(&server)
            .await;

        let mut cfg = TransportConfig::new(server.uri(), "test-token");
        cfg.max_attempts = 1;
        cfg.retry_budget = std::time::Duration::from_millis(1);
        let client = JmapClient::new(cfg).unwrap();
        let sess = fake_session(&api_url);
        let err = client.list_mailboxes(&sess, "acct-1").await.unwrap_err();
        assert!(matches!(
            err,
            Error::RateLimit {
                retry_after_seconds: 7
            }
        ));
    }

    /// When `Email/set` rejects the draft (e.g. `overQuota`,
    /// `invalidProperties`), the chained `EmailSubmission/set`
    /// fails with the opaque `invalidResultReference` because the
    /// `#draft1` back-reference cannot resolve. `send_email` MUST
    /// look at the `Email/set` response first and surface the real
    /// reason rather than the misleading reference error.
    #[tokio::test]
    async fn send_email_surfaces_email_set_failure_before_submission() {
        let server = MockServer::start().await;
        let api_url = format!("{}/jmap/api", server.uri());
        Mock::given(method("POST"))
            .and(path("/jmap/api"))
            .respond_with(ResponseTemplate::new(200).set_body_json(serde_json::json!({
                "sessionState": "s-1",
                "methodResponses": [
                    ["Email/set", {
                        "accountId": "acct-1",
                        "newState": "e-state-2",
                        "created": {},
                        "notCreated": {
                            "draft1": {
                                "type": "urn:ietf:params:jmap:error:overQuota",
                                "description": "mailbox quota exceeded"
                            }
                        }
                    }, "c0"],
                    ["error", {
                        "type": "urn:ietf:params:jmap:error:invalidResultReference",
                        "description": "could not resolve #draft1"
                    }, "c1"]
                ]
            })))
            .mount(&server)
            .await;

        let client = fresh_client(&server).await;
        let sess = fake_session(&api_url);
        let draft = EmailDraft {
            subject: "test".into(),
            to: vec![crate::models::EmailAddress {
                name: "Recipient".into(),
                email: "rcpt@example.com".into(),
            }],
            ..EmailDraft::default()
        };
        let err = client
            .send_email(&sess, "acct-1", &draft)
            .await
            .unwrap_err();
        match err {
            Error::JmapMethod { code, description } => {
                // The real failure reason (overQuota) must surface,
                // NOT the downstream `invalidResultReference` from
                // the broken `#draft1` back-reference.
                assert!(
                    code.ends_with("overQuota"),
                    "expected overQuota, got {code}"
                );
                assert!(description.contains("quota"));
            }
            other => panic!("expected JmapMethod overQuota, got {other:?}"),
        }
    }

    /// `Email/changes` may signal `hasMoreChanges: true` when the
    /// returned change set is truncated. Verifies the response type
    /// parses the flag correctly (the loop wiring lives in
    /// `client::tests::*`).
    #[tokio::test]
    async fn email_changes_has_more_flag_parses() {
        let raw = serde_json::json!({
            "accountId": "acct-1",
            "oldState": "s-old",
            "newState": "s-new",
            "hasMoreChanges": true,
            "created": [],
            "updated": ["e-1", "e-2"],
            "destroyed": []
        });
        let r: EmailChangesResponse = serde_json::from_value(raw).unwrap();
        assert!(r.has_more_changes);
        assert_eq!(r.updated, vec!["e-1", "e-2"]);
    }
}
