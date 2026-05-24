// JMAP request shapes.
//
// RFC 8620 §3 defines the request envelope as:
//
//   { "using": ["urn..."],
//     "methodCalls": [ [methodName, args, callId], ... ],
//     "createdIds": { ... }   (optional) }
//
// We expose an ergonomic builder (`JmapRequest::call`) while a
// hand-rolled `Serialize` keeps the wire format exact — each
// invocation is serialised as a 3-element heterogeneous array,
// not as a JSON object.

use serde::ser::{SerializeSeq, SerializeStruct};
use serde::{Deserialize, Serialize, Serializer};
use std::collections::BTreeMap;

/// Capability URNs the SDK needs at minimum.
pub const CAP_CORE: &str = "urn:ietf:params:jmap:core";
pub const CAP_MAIL: &str = "urn:ietf:params:jmap:mail";
pub const CAP_SUBMISSION: &str = "urn:ietf:params:jmap:submission";

/// Top-level JMAP request envelope.
#[derive(Clone, Debug)]
pub struct JmapRequest {
    pub using: Vec<String>,
    pub method_calls: Vec<JmapInvocation>,
    pub created_ids: BTreeMap<String, String>,
}

impl JmapRequest {
    pub fn new(using: Vec<String>) -> Self {
        Self {
            using,
            method_calls: Vec::new(),
            created_ids: BTreeMap::new(),
        }
    }

    /// Append a method call. Returns the assigned call ID so the
    /// caller can correlate the response invocation.
    pub fn call(&mut self, method: impl Into<String>, args: serde_json::Value) -> String {
        let call_id = format!("c{}", self.method_calls.len());
        self.method_calls.push(JmapInvocation {
            method: method.into(),
            args,
            call_id: call_id.clone(),
        });
        call_id
    }

    /// Append a method call with an explicit call ID. Used when
    /// later invocations reference an earlier one via JMAP back-
    /// references (`#callId`).
    pub fn call_with_id(
        &mut self,
        method: impl Into<String>,
        args: serde_json::Value,
        call_id: impl Into<String>,
    ) {
        self.method_calls.push(JmapInvocation {
            method: method.into(),
            args,
            call_id: call_id.into(),
        });
    }
}

impl Serialize for JmapRequest {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        // The declared field count must match the number of fields
        // we will actually emit. `serde_json` treats this argument
        // as a size hint, but binary formats (bincode, MessagePack,
        // CBOR with definite-length encoding) decode using the
        // declared length verbatim, so a 3/2 mismatch would corrupt
        // the wire output in those formats. JMAP itself is always
        // JSON, but we still want `JmapRequest` to obey the generic
        // `Serialize` contract in case anyone ever logs / persists
        // a request via a non-JSON serializer.
        let field_count = if self.created_ids.is_empty() { 2 } else { 3 };
        let mut s = serializer.serialize_struct("JmapRequest", field_count)?;
        s.serialize_field("using", &self.using)?;
        s.serialize_field("methodCalls", &MethodCalls(&self.method_calls))?;
        if !self.created_ids.is_empty() {
            s.serialize_field("createdIds", &self.created_ids)?;
        }
        s.end()
    }
}

/// Single invocation in the `methodCalls` array.
#[derive(Clone, Debug, Deserialize)]
pub struct JmapInvocation {
    pub method: String,
    pub args: serde_json::Value,
    pub call_id: String,
}

impl Serialize for JmapInvocation {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        // RFC 8620 §3.2: `[methodName, args, callId]`.
        let mut seq = serializer.serialize_seq(Some(3))?;
        seq.serialize_element(&self.method)?;
        seq.serialize_element(&self.args)?;
        seq.serialize_element(&self.call_id)?;
        seq.end()
    }
}

struct MethodCalls<'a>(&'a [JmapInvocation]);

impl Serialize for MethodCalls<'_> {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        let mut seq = serializer.serialize_seq(Some(self.0.len()))?;
        for inv in self.0 {
            seq.serialize_element(inv)?;
        }
        seq.end()
    }
}

// === Typed method arguments =====================================

/// `Mailbox/get` arguments (RFC 8621 §2).
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct MailboxGetArgs {
    pub account_id: String,
    /// `null` selects every mailbox.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ids: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub properties: Option<Vec<String>>,
}

/// `Email/get` arguments (RFC 8621 §4.2).
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct EmailGetArgs {
    pub account_id: String,
    pub ids: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub properties: Option<Vec<String>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fetch_text_body_values: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fetch_html_body_values: Option<bool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub fetch_all_body_values: Option<bool>,
}

/// `Email/query` arguments (RFC 8621 §4.4). Trimmed to the fields
/// the SDK uses today; add as needed.
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct EmailQueryArgs {
    pub account_id: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub filter: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub sort: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub position: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub limit: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub collapse_threads: Option<bool>,
}

/// `Email/changes` arguments (RFC 8620 §5.2).
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct EmailChangesArgs {
    pub account_id: String,
    pub since_state: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_changes: Option<u32>,
}

/// `Mailbox/changes` arguments (RFC 8620 §5.2 + RFC 8621 §2.4).
///
/// Same wire shape as `Email/changes`; we keep the type distinct
/// rather than aliasing because the response shape diverges
/// (`Mailbox/changes` returns the `updatedProperties` hint that
/// `Email/changes` does not) and conflating them would tempt
/// future calls to take the wrong type.
#[derive(Clone, Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct MailboxChangesArgs {
    pub account_id: String,
    pub since_state: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_changes: Option<u32>,
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The on-the-wire format for `methodCalls` is an array of
    /// `[method, args, callId]` triplets per RFC 8620 §3.2. A
    /// regression here breaks every JMAP request.
    #[test]
    fn request_serializes_method_calls_as_triplets() {
        let mut req = JmapRequest::new(vec![CAP_CORE.into(), CAP_MAIL.into()]);
        req.call(
            "Mailbox/get",
            serde_json::to_value(MailboxGetArgs {
                account_id: "acct-1".into(),
                ids: None,
                properties: None,
            })
            .unwrap(),
        );

        let s = serde_json::to_string(&req).unwrap();
        let v: serde_json::Value = serde_json::from_str(&s).unwrap();
        assert!(v["using"].is_array());
        let calls = v["methodCalls"].as_array().unwrap();
        assert_eq!(calls.len(), 1);
        let call = calls[0].as_array().unwrap();
        assert_eq!(call.len(), 3);
        assert_eq!(call[0], "Mailbox/get");
        assert_eq!(call[1]["accountId"], "acct-1");
        assert_eq!(call[2], "c0");
    }

    /// `createdIds` should only appear when populated. The BFF
    /// rejects empty objects on some endpoints; better to omit.
    #[test]
    fn empty_created_ids_is_omitted() {
        let req = JmapRequest::new(vec![CAP_CORE.into()]);
        let v: serde_json::Value = serde_json::to_value(&req).unwrap();
        assert!(v.get("createdIds").is_none());
    }

    #[test]
    fn populated_created_ids_is_serialised() {
        let mut req = JmapRequest::new(vec![CAP_CORE.into()]);
        req.created_ids.insert("draft1".into(), "real-id-1".into());
        let v: serde_json::Value = serde_json::to_value(&req).unwrap();
        assert_eq!(v["createdIds"]["draft1"], "real-id-1");
    }

    #[test]
    fn call_ids_increment_and_explicit_ids_pass_through() {
        let mut req = JmapRequest::new(vec![CAP_CORE.into()]);
        let id1 = req.call("Mailbox/get", serde_json::json!({}));
        let id2 = req.call("Email/changes", serde_json::json!({}));
        req.call_with_id("Email/get", serde_json::json!({}), "explicit");
        assert_eq!(id1, "c0");
        assert_eq!(id2, "c1");
        let v: serde_json::Value = serde_json::to_value(&req).unwrap();
        assert_eq!(v["methodCalls"][2][2], "explicit");
    }
}
