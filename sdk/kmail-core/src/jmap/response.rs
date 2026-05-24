// JMAP response shape.
//
// The wire format mirrors the request envelope inversely: a
// `methodResponses` array of `[methodName, args, callId]` triplets
// plus a top-level `sessionState`. Some invocations return a JMAP
// error in `args` instead of the expected `*/get`-style payload;
// the parser surfaces those as typed `MethodErrorPayload` so the
// caller can `?`-propagate them with `Error::JmapMethod`.

use crate::error::{Error, Result};
use serde::de::{self, Deserializer, SeqAccess, Visitor};
use serde::{Deserialize, Serialize};
use std::fmt;

/// Top-level batch response.
#[derive(Clone, Debug, Deserialize)]
pub struct JmapResponse {
    /// Session state at the time the response was produced.
    #[serde(rename = "sessionState", default)]
    pub session_state: String,
    /// Ordered list of method responses. Each entry is
    /// `[methodName, args, callId]` per RFC 8620 §3.3.
    #[serde(rename = "methodResponses", default)]
    pub method_responses: Vec<JmapInvocationResponse>,
    /// Echo of `createdIds` from the request, plus any new IDs the
    /// server assigned to client-supplied creation tags.
    #[serde(rename = "createdIds", default)]
    pub created_ids: std::collections::BTreeMap<String, String>,
}

/// Single entry in `methodResponses`.
#[derive(Clone, Debug)]
pub struct JmapInvocationResponse {
    pub method: String,
    pub args: serde_json::Value,
    pub call_id: String,
}

/// JMAP method-level error payload (RFC 8620 §3.5.1).
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct MethodErrorPayload {
    /// The JMAP error code, e.g.
    /// `urn:ietf:params:jmap:error:invalidArguments`. Some servers
    /// return the short form (`"invalidArguments"`); both are
    /// accepted here.
    pub r#type: String,
    /// Human-readable description. Optional per spec.
    #[serde(default)]
    pub description: Option<String>,
}

impl JmapResponse {
    /// Locate the response invocation whose `callId` matches `id`.
    /// Returns `None` if no such invocation exists (should not
    /// happen on a well-formed BFF response).
    pub fn find(&self, id: &str) -> Option<&JmapInvocationResponse> {
        self.method_responses.iter().find(|r| r.call_id == id)
    }

    /// Look up `id` and parse its args as `T`. Surfaces
    /// method-level JMAP errors as `Error::JmapMethod`.
    pub fn parse<T: serde::de::DeserializeOwned>(&self, id: &str) -> Result<T> {
        let resp = self
            .find(id)
            .ok_or_else(|| Error::Protocol(format!("no response for callId {id}")))?;
        if resp.method == "error" {
            let err: MethodErrorPayload = serde_json::from_value(resp.args.clone())?;
            return Err(Error::JmapMethod {
                code: err.r#type,
                description: err.description.unwrap_or_default(),
            });
        }
        Ok(serde_json::from_value(resp.args.clone())?)
    }
}

impl<'de> Deserialize<'de> for JmapInvocationResponse {
    fn deserialize<D>(deserializer: D) -> std::result::Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        struct InvocationVisitor;

        impl<'de> Visitor<'de> for InvocationVisitor {
            type Value = JmapInvocationResponse;

            fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
                f.write_str("a JMAP method response triplet [method, args, callId]")
            }

            fn visit_seq<A>(self, mut seq: A) -> std::result::Result<Self::Value, A::Error>
            where
                A: SeqAccess<'de>,
            {
                let method: String = seq
                    .next_element()?
                    .ok_or_else(|| de::Error::invalid_length(0, &"3 elements"))?;
                let args: serde_json::Value = seq
                    .next_element()?
                    .ok_or_else(|| de::Error::invalid_length(1, &"3 elements"))?;
                let call_id: String = seq
                    .next_element()?
                    .ok_or_else(|| de::Error::invalid_length(2, &"3 elements"))?;
                Ok(JmapInvocationResponse {
                    method,
                    args,
                    call_id,
                })
            }
        }

        deserializer.deserialize_seq(InvocationVisitor)
    }
}

// === Typed response payloads ====================================

/// `Mailbox/get` response payload (RFC 8621 §2.5).
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MailboxGetResponse {
    pub account_id: String,
    pub state: String,
    pub list: Vec<crate::models::Mailbox>,
    #[serde(default)]
    pub not_found: Vec<String>,
}

/// `Email/get` response payload.
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EmailGetResponse {
    pub account_id: String,
    pub state: String,
    pub list: Vec<crate::models::Email>,
    #[serde(default)]
    pub not_found: Vec<String>,
}

/// `Email/query` response payload (subset).
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EmailQueryResponse {
    pub account_id: String,
    pub query_state: String,
    pub can_calculate_changes: bool,
    pub position: i64,
    pub total: Option<u64>,
    pub ids: Vec<String>,
}

/// `Email/changes` response payload.
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EmailChangesResponse {
    pub account_id: String,
    pub old_state: String,
    pub new_state: String,
    pub has_more_changes: bool,
    #[serde(default)]
    pub created: Vec<String>,
    #[serde(default)]
    pub updated: Vec<String>,
    #[serde(default)]
    pub destroyed: Vec<String>,
}

/// `Mailbox/changes` response payload (RFC 8621 §2.4).
///
/// `updated_properties`, when present and non-null, is the
/// server's hint that ONLY the listed mailbox properties changed
/// for the IDs in `updated`. The SDK ignores the hint today and
/// always re-fetches the full `Mailbox` object, which keeps the
/// hydration path uniform with `created` and is cheap because
/// `Mailbox/get` returns the entire mailbox set per call (mailbox
/// counts are typically O(dozens)). We parse the field for
/// completeness and so a future optimisation can consume it
/// without a wire-format change.
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct MailboxChangesResponse {
    pub account_id: String,
    pub old_state: String,
    pub new_state: String,
    pub has_more_changes: bool,
    #[serde(default)]
    pub created: Vec<String>,
    #[serde(default)]
    pub updated: Vec<String>,
    #[serde(default)]
    pub destroyed: Vec<String>,
    #[serde(default)]
    pub updated_properties: Option<Vec<String>>,
}

/// `EmailSubmission/set` response payload (subset).
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EmailSubmissionSetResponse {
    pub account_id: String,
    pub new_state: String,
    #[serde(default)]
    pub created: std::collections::BTreeMap<String, serde_json::Value>,
    #[serde(default)]
    pub not_created: std::collections::BTreeMap<String, MethodErrorPayload>,
}

/// `Email/set` response payload (subset).
///
/// We only parse the fields that drive send-flow correctness:
/// `created` (so we can recover the server-assigned draft ID
/// when `createdIds` is absent from the envelope) and
/// `notCreated` (so quota / mime-validation failures surface as
/// the actual `overQuota` / `invalidProperties` error rather
/// than the downstream `invalidResultReference` from the
/// `#draft1` back-reference in `EmailSubmission/set`).
#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct EmailSetResponse {
    pub account_id: String,
    #[serde(default)]
    pub new_state: String,
    #[serde(default)]
    pub created: std::collections::BTreeMap<String, serde_json::Value>,
    #[serde(default)]
    pub not_created: std::collections::BTreeMap<String, MethodErrorPayload>,
    #[serde(default)]
    pub updated: std::collections::BTreeMap<String, serde_json::Value>,
    #[serde(default)]
    pub not_updated: std::collections::BTreeMap<String, MethodErrorPayload>,
    #[serde(default)]
    pub destroyed: Vec<String>,
    #[serde(default)]
    pub not_destroyed: std::collections::BTreeMap<String, MethodErrorPayload>,
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn parses_canonical_response_envelope() {
        let raw = json!({
            "sessionState": "s-1",
            "methodResponses": [
                ["Mailbox/get", {
                    "accountId": "acct-1",
                    "state": "mbx-state-1",
                    "list": [],
                    "notFound": []
                }, "c0"]
            ],
            "createdIds": {"d1": "real-id-1"}
        });

        let resp: JmapResponse = serde_json::from_value(raw).unwrap();
        assert_eq!(resp.session_state, "s-1");
        assert_eq!(resp.method_responses.len(), 1);
        assert_eq!(resp.method_responses[0].method, "Mailbox/get");
        assert_eq!(resp.method_responses[0].call_id, "c0");

        let mbx: MailboxGetResponse = resp.parse("c0").unwrap();
        assert_eq!(mbx.account_id, "acct-1");
        assert_eq!(mbx.state, "mbx-state-1");

        let raw_id = resp.created_ids.get("d1").map(String::as_str);
        assert_eq!(raw_id, Some("real-id-1"));
    }

    /// A method-level error is wire-encoded as
    /// `["error", {"type": "...", "description": "..."}, "c0"]`.
    /// `parse` must surface it as `Error::JmapMethod`.
    #[test]
    fn method_level_error_surfaces_as_jmap_method() {
        let raw = json!({
            "sessionState": "s-1",
            "methodResponses": [
                ["error", {
                    "type": "urn:ietf:params:jmap:error:invalidArguments",
                    "description": "ids must be a non-empty array"
                }, "c0"]
            ]
        });
        let resp: JmapResponse = serde_json::from_value(raw).unwrap();
        let err = resp
            .parse::<MailboxGetResponse>("c0")
            .expect_err("method error should propagate");
        match err {
            Error::JmapMethod { code, description } => {
                assert!(code.ends_with("invalidArguments"));
                assert!(description.contains("non-empty"));
            }
            other => panic!("expected JmapMethod, got {other:?}"),
        }
    }

    #[test]
    fn missing_callid_yields_protocol_error() {
        let raw = json!({"methodResponses": []});
        let resp: JmapResponse = serde_json::from_value(raw).unwrap();
        let err = resp.parse::<MailboxGetResponse>("nope").unwrap_err();
        assert!(matches!(err, Error::Protocol(_)));
    }

    #[test]
    fn email_changes_envelope_parses() {
        let raw = json!({
            "accountId": "acct-1",
            "oldState": "s-old",
            "newState": "s-new",
            "hasMoreChanges": false,
            "created": ["e1", "e2"],
            "updated": [],
            "destroyed": ["eOld"]
        });
        let r: EmailChangesResponse = serde_json::from_value(raw).unwrap();
        assert_eq!(r.new_state, "s-new");
        assert_eq!(r.created, vec!["e1", "e2"]);
        assert_eq!(r.destroyed, vec!["eOld"]);
    }
}
