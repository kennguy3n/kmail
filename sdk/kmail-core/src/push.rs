// Device push token registration + JMAP push payload parsing.
//
// The BFF accepts `POST /api/v1/push/subscribe` with a uniform
// payload regardless of transport (APNs / FCM / Web Push). The
// SDK normalises platform-specific token shapes into that wire
// format so callers only have to forward whatever their platform
// gave them. See `cmd/kmail-api/main.go` (the existing push
// transport router) for the BFF side.

use serde::{Deserialize, Serialize};

/// Push transport. Matches the `transport` discriminator the BFF
/// expects in `POST /api/v1/push/subscribe`.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PushTransport {
    Apns,
    Fcm,
    WebPush,
}

impl PushTransport {
    pub fn as_str(self) -> &'static str {
        match self {
            PushTransport::Apns => "apns",
            PushTransport::Fcm => "fcm",
            PushTransport::WebPush => "webpush",
        }
    }
}

/// Subscription request wire format.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct PushSubscriptionRequest {
    pub transport: PushTransport,
    /// Opaque per-device token. APNs: 64 hex chars. FCM: longer
    /// base64-ish string. Web Push: the `endpoint` URL.
    pub token: String,
    /// Optional Web Push keypair (`p256dh`, `auth`). Ignored on
    /// APNs / FCM.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub web_push_keys: Option<WebPushKeys>,
    /// Optional list of JMAP type names the subscription should
    /// fire for. Defaults to `["Email", "EmailDelivery", "Mailbox",
    /// "EmailSubmission"]` server-side when omitted.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub types: Vec<String>,
}

/// Web Push key pair as per RFC 8291. Both fields are base64url
/// without padding.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct WebPushKeys {
    pub p256dh: String,
    pub auth: String,
}

/// Decoded JMAP `StateChange` push payload. Matches RFC 8620 §7.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
pub struct PushPayload {
    #[serde(rename = "@type", default = "default_state_change_type")]
    pub type_name: String,
    /// Map of account ID → (type name → state token).
    pub changed: std::collections::BTreeMap<String, std::collections::BTreeMap<String, String>>,
}

fn default_state_change_type() -> String {
    "StateChange".to_string()
}

impl PushPayload {
    /// Parse a raw JSON push payload.
    pub fn parse(raw: &str) -> crate::error::Result<Self> {
        Ok(serde_json::from_str(raw)?)
    }

    /// Convenience: returns the new state token the SDK should
    /// pass to `Email/changes` for the given account.
    pub fn email_state(&self, account_id: &str) -> Option<&str> {
        self.changed
            .get(account_id)
            .and_then(|m| m.get("Email"))
            .map(String::as_str)
    }

    /// Convenience: returns the new state token for `Mailbox`
    /// changes for the given account.
    pub fn mailbox_state(&self, account_id: &str) -> Option<&str> {
        self.changed
            .get(account_id)
            .and_then(|m| m.get("Mailbox"))
            .map(String::as_str)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// RFC 8620 §7 example payload — must parse cleanly and the
    /// state-token accessors must surface the right values.
    #[test]
    fn parses_rfc8620_state_change() {
        let raw = r#"{
            "@type": "StateChange",
            "changed": {
                "a-tenant-1": {
                    "Email": "s-101",
                    "Mailbox": "s-202"
                }
            }
        }"#;
        let p = PushPayload::parse(raw).unwrap();
        assert_eq!(p.type_name, "StateChange");
        assert_eq!(p.email_state("a-tenant-1"), Some("s-101"));
        assert_eq!(p.mailbox_state("a-tenant-1"), Some("s-202"));
        assert_eq!(p.email_state("other"), None);
    }

    #[test]
    fn subscription_request_serializes_transport_lowercase() {
        let req = PushSubscriptionRequest {
            transport: PushTransport::Apns,
            token: "deadbeef".into(),
            web_push_keys: None,
            types: vec![],
        };
        let v = serde_json::to_value(&req).unwrap();
        assert_eq!(v["transport"], "apns");
        assert!(v.get("webPushKeys").is_none() && v.get("web_push_keys").is_none());
    }

    #[test]
    fn webpush_keys_optional() {
        let raw = r#"{
            "transport": "webpush",
            "token": "https://updates.push.services.mozilla.com/sub/abc",
            "web_push_keys": {"p256dh": "BHA...", "auth": "auth..."}
        }"#;
        let parsed: PushSubscriptionRequest = serde_json::from_str(raw).unwrap();
        assert_eq!(parsed.transport, PushTransport::WebPush);
        assert!(parsed.web_push_keys.is_some());
    }
}
