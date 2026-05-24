// Device push token registration + JMAP push payload parsing.
//
// The BFF accepts `POST /api/v1/push/subscribe` with a uniform
// payload regardless of transport (APNs / FCM / Web Push). The
// SDK normalises platform-specific token shapes into that wire
// format so callers only have to forward whatever their platform
// gave them. See `internal/push/push.go` (lines 47-56 for the
// canonical `Subscription` struct, lines 115-118 for the
// accepted `device_type` values) and `internal/push/handlers.go`
// (line 51, which directly `json.Unmarshal`s the request body
// into `Subscription`). Because `json.Unmarshal` silently
// discards unknown fields, any deviation from the BFF's JSON
// shape — including snake_case vs camelCase, nested vs flat
// keys, or alternative enum spellings — would silently strip the
// payload to zero values and the BFF would create an empty
// subscription with no push endpoint, no device type, and no
// keys. The wire types in this module mirror that contract
// exactly so SDK clients cannot accidentally produce a no-op
// subscription.

use serde::{Deserialize, Serialize};

/// Push transport on the SDK's public API. The variants are named
/// after the underlying transport protocol (APNs / FCM / Web
/// Push) because that's how mobile platforms describe themselves;
/// they serialise as the **device-type strings** the BFF accepts
/// (`"ios"`, `"android"`, `"web"` per
/// `internal/push/push.go:115-118`). The 1:1 mapping is:
///
/// | Rust variant         | Wire (`device_type`) |
/// | -------------------- | -------------------- |
/// | `PushTransport::Apns`    | `"ios"`              |
/// | `PushTransport::Fcm`     | `"android"`          |
/// | `PushTransport::WebPush` | `"web"`              |
///
/// Any other spelling would be rejected by
/// `internal/push/push.go:117-118` with `ErrInvalidInput`.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub enum PushTransport {
    #[serde(rename = "ios")]
    Apns,
    #[serde(rename = "android")]
    Fcm,
    #[serde(rename = "web")]
    WebPush,
}

impl PushTransport {
    /// Canonical BFF-facing string (matches the JSON serialisation).
    pub fn as_str(self) -> &'static str {
        match self {
            PushTransport::Apns => "ios",
            PushTransport::Fcm => "android",
            PushTransport::WebPush => "web",
        }
    }
}

/// `POST /api/v1/push/subscribe` request body.
///
/// **Wire format.** This struct serialises with the **exact**
/// JSON field names the Go BFF's `Subscription` struct expects
/// (`internal/push/push.go:47-56`):
///
/// ```json
/// {
///   "device_type":   "ios" | "android" | "web",
///   "push_endpoint": "<opaque token / Web Push endpoint URL>",
///   "p256dh_key":    "<base64url, Web Push only>",
///   "auth_key":      "<base64url, Web Push only>"
/// }
/// ```
///
/// The Rust field names follow Rust naming conventions and are
/// remapped via `#[serde(rename = "...")]`. Web Push keys are
/// **flat** top-level fields — the BFF does NOT accept a nested
/// `web_push_keys` object, and Go's `json.Unmarshal` silently
/// discards unknown keys (so a nested shape would no-op the
/// entire request). The SDK's higher-level
/// `KMailClient::register_push_token` API still takes a
/// `WebPushKeys { p256dh, auth }` for ergonomics and flattens it
/// here just before the HTTP call.
///
/// **No `types` field.** RFC 8620 §7.3 push subscriptions can be
/// filtered to a subset of JMAP type names, but the current BFF
/// `Subscription` struct does not store or forward that filter.
/// Including `types: []` in the wire format would be silently
/// discarded and create the false impression of filtering, so we
/// omit it entirely; if the BFF gains the field later, add it
/// here in the same PR.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct PushSubscriptionRequest {
    #[serde(rename = "device_type")]
    pub transport: PushTransport,
    /// Opaque per-device token. APNs: 64 hex chars. FCM: longer
    /// base64-ish string. Web Push: the `endpoint` URL.
    #[serde(rename = "push_endpoint")]
    pub token: String,
    /// Web Push P-256 ECDH public key (RFC 8291), base64url
    /// without padding. `None` on APNs / FCM.
    #[serde(
        rename = "p256dh_key",
        default,
        skip_serializing_if = "Option::is_none"
    )]
    pub p256dh_key: Option<String>,
    /// Web Push authentication secret (RFC 8291), base64url
    /// without padding. `None` on APNs / FCM.
    #[serde(rename = "auth_key", default, skip_serializing_if = "Option::is_none")]
    pub auth_key: Option<String>,
}

/// Web Push key pair as per RFC 8291. Both fields are base64url
/// without padding. Used as an ergonomic input type for
/// `KMailClient::register_push_token`; on the wire the two
/// fields are flattened into top-level `p256dh_key` /
/// `auth_key` on `PushSubscriptionRequest` to match the BFF
/// `Subscription` struct.
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

    /// Regression for the BFF-contract bug: the SDK previously
    /// serialised `transport: "apns"` / `token: "..."` /
    /// `web_push_keys: { p256dh, auth }` / `types: []`. The Go
    /// BFF's `Subscription` struct expects
    /// `device_type / push_endpoint / p256dh_key / auth_key` —
    /// `json.Unmarshal` silently discards mismatched fields, so
    /// the old SDK shape would have created a subscription with
    /// every field zero-valued (no push for any native client).
    /// This test pins the wire format byte-for-byte against the
    /// BFF struct definition.
    #[test]
    fn subscription_request_matches_bff_subscription_contract() {
        // APNs case: no Web Push keys, single device-type
        // discriminator.
        let req = PushSubscriptionRequest {
            transport: PushTransport::Apns,
            token: "deadbeefcafebabe".into(),
            p256dh_key: None,
            auth_key: None,
        };
        let v = serde_json::to_value(&req).unwrap();
        assert_eq!(
            v["device_type"], "ios",
            "device_type must serialise as 'ios' / 'android' / 'web' (internal/push/push.go:115-118)"
        );
        assert_eq!(v["push_endpoint"], "deadbeefcafebabe");
        // Web Push fields are absent (not null) so the BFF
        // doesn't store empty strings into `auth_key`/`p256dh_key`.
        assert!(
            v.get("p256dh_key").is_none(),
            "p256dh_key must be omitted on APNs (not serialised as null)"
        );
        assert!(
            v.get("auth_key").is_none(),
            "auth_key must be omitted on APNs (not serialised as null)"
        );
        // Old wire-format keys must never appear.
        for stale in [
            "transport",
            "token",
            "web_push_keys",
            "webPushKeys",
            "types",
        ] {
            assert!(
                v.get(stale).is_none(),
                "stale wire-format key {stale:?} must not appear (would silently no-op on the BFF)"
            );
        }
    }

    /// Web Push subscription: both keys present, transport
    /// renders as `"web"`.
    #[test]
    fn subscription_request_webpush_flattens_keys() {
        let req = PushSubscriptionRequest {
            transport: PushTransport::WebPush,
            token: "https://updates.push.services.mozilla.com/sub/abc".into(),
            p256dh_key: Some("BHA-base64url".into()),
            auth_key: Some("auth-base64url".into()),
        };
        let v = serde_json::to_value(&req).unwrap();
        assert_eq!(v["device_type"], "web");
        assert_eq!(
            v["push_endpoint"],
            "https://updates.push.services.mozilla.com/sub/abc"
        );
        // Flat fields per the BFF Subscription struct — NOT a
        // nested web_push_keys object.
        assert_eq!(v["p256dh_key"], "BHA-base64url");
        assert_eq!(v["auth_key"], "auth-base64url");
        assert!(
            v.get("web_push_keys").is_none(),
            "web_push_keys nested object must not appear — BFF expects flat fields"
        );
    }

    /// Android / FCM serialises as `"android"`.
    #[test]
    fn fcm_serializes_as_android() {
        let req = PushSubscriptionRequest {
            transport: PushTransport::Fcm,
            token: "fcm-registration-token".into(),
            p256dh_key: None,
            auth_key: None,
        };
        let v = serde_json::to_value(&req).unwrap();
        assert_eq!(v["device_type"], "android");
    }

    /// Reverse direction: parse a BFF-shaped JSON body back
    /// into the struct. Web Push keys arrive as flat fields.
    #[test]
    fn deserialize_bff_subscription_body() {
        let raw = r#"{
            "device_type": "web",
            "push_endpoint": "https://updates.push.services.mozilla.com/sub/abc",
            "p256dh_key": "BHA...",
            "auth_key": "auth..."
        }"#;
        let parsed: PushSubscriptionRequest = serde_json::from_str(raw).unwrap();
        assert_eq!(parsed.transport, PushTransport::WebPush);
        assert_eq!(parsed.p256dh_key.as_deref(), Some("BHA..."));
        assert_eq!(parsed.auth_key.as_deref(), Some("auth..."));
    }
}
