// Domain models shared by every layer.
//
// Wire-format choices deliberately mirror the BFF's TypeScript
// types in `web/src/types/jmap.ts` so the SDK and the React web
// client see the same JMAP shapes. Where JMAP uses CamelCase
// field names ("blobId", "receivedAt") we rename via serde so the
// idiomatic Rust types still serialise to the canonical JMAP
// wire-format with no manual fixup.

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

/// JMAP session resource returned by `GET /jmap/session`
/// (or via `GET /.well-known/jmap`).
///
/// Captures the subset the SDK needs to dispatch subsequent
/// requests — full primary-account / state shapes are kept
/// open-ended via `extra` so the BFF can extend the response
/// without forcing an SDK version bump.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct JmapSession {
    /// JMAP API endpoint (`/jmap/api` against the BFF, per
    /// docs/JMAP-CONTRACT.md §4.1). Always an absolute path or
    /// URL — relative paths are resolved against the BFF base.
    #[serde(rename = "apiUrl")]
    pub api_url: String,

    /// Server-Sent Events endpoint for push (RFC 8620 §7.2).
    #[serde(rename = "eventSourceUrl", default)]
    pub event_source_url: String,

    /// Upload endpoint for blob ingestion (RFC 8620 §6.1).
    #[serde(rename = "uploadUrl", default)]
    pub upload_url: String,

    /// Download endpoint for blob retrieval (RFC 8620 §6.2).
    #[serde(rename = "downloadUrl", default)]
    pub download_url: String,

    /// Accounts the authenticated principal can act on. The BFF
    /// always returns exactly one account per OIDC subject; we
    /// keep the map shape for forward-compatibility with delegated
    /// access flows in Phase 6+.
    #[serde(default)]
    pub accounts: BTreeMap<String, JmapAccount>,

    /// `primaryAccounts` maps a capability URN to the account ID
    /// that owns it. The BFF mirrors RFC 8620 §2.2 exactly.
    #[serde(rename = "primaryAccounts", default)]
    pub primary_accounts: BTreeMap<String, String>,

    /// Capabilities the BFF advertises (see docs/JMAP-CONTRACT.md
    /// §2). Stored as `Vec<String>` rather than a typed enum so
    /// a new capability lands as data, not as a code change.
    #[serde(default, deserialize_with = "deserialize_capabilities")]
    pub capabilities: Vec<String>,

    /// Username the BFF associates with this session. Surfaced to
    /// the UI banner and used for log correlation.
    #[serde(default)]
    pub username: String,

    /// State string. Opaque to clients; supplied back to JMAP
    /// methods that take `ifInState`.
    #[serde(default)]
    pub state: String,
}

/// JMAP account descriptor.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct JmapAccount {
    pub name: String,
    #[serde(rename = "isPersonal", default)]
    pub is_personal: bool,
    #[serde(rename = "isReadOnly", default)]
    pub is_read_only: bool,
    /// Map of capability URN → capability-specific account info.
    /// Kept opaque (`serde_json::Value`) because each capability
    /// defines its own sub-schema.
    #[serde(rename = "accountCapabilities", default)]
    pub account_capabilities: BTreeMap<String, serde_json::Value>,
}

/// Roles defined by RFC 8621 §2 + KMail extensions.
///
/// The `Vault` variant is KMail-specific and corresponds to
/// mailboxes flagged with the `vault` extension property — these
/// have server-side search disabled (Zero-Access Vault, see
/// docs/JMAP-CONTRACT.md §2.4 and docs/ARCHITECTURE.md §5).
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum MailboxRole {
    Inbox,
    Archive,
    Drafts,
    Sent,
    Trash,
    Junk,
    Important,
    All,
    Flagged,
    Vault,
    #[serde(other)]
    Unknown,
}

/// JMAP `Mailbox` as exposed via the BFF.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Mailbox {
    pub id: String,
    pub name: String,
    #[serde(default)]
    pub role: Option<MailboxRole>,
    #[serde(rename = "parentId", default)]
    pub parent_id: Option<String>,
    #[serde(rename = "sortOrder", default)]
    pub sort_order: u32,
    #[serde(rename = "totalEmails", default)]
    pub total_emails: u64,
    #[serde(rename = "unreadEmails", default)]
    pub unread_emails: u64,
    #[serde(rename = "totalThreads", default)]
    pub total_threads: u64,
    #[serde(rename = "unreadThreads", default)]
    pub unread_threads: u64,
    /// KMail extension. True when this mailbox is a Zero-Access
    /// Vault (server cannot index / search).
    #[serde(rename = "isVault", default)]
    pub is_vault: bool,
    /// `MyRights` substructure per RFC 8621 §2.
    #[serde(rename = "myRights", default)]
    pub my_rights: Option<MailboxRights>,
}

/// `MyRights` sub-object from `Mailbox/get`.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct MailboxRights {
    #[serde(rename = "mayReadItems", default)]
    pub may_read_items: bool,
    #[serde(rename = "mayAddItems", default)]
    pub may_add_items: bool,
    #[serde(rename = "mayRemoveItems", default)]
    pub may_remove_items: bool,
    #[serde(rename = "maySetSeen", default)]
    pub may_set_seen: bool,
    #[serde(rename = "maySetKeywords", default)]
    pub may_set_keywords: bool,
    #[serde(rename = "mayCreateChild", default)]
    pub may_create_child: bool,
    #[serde(rename = "mayRename", default)]
    pub may_rename: bool,
    #[serde(rename = "mayDelete", default)]
    pub may_delete: bool,
    #[serde(rename = "maySubmit", default)]
    pub may_submit: bool,
}

/// RFC 5322 address pair as JMAP encodes it.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct EmailAddress {
    /// Display name, e.g. `"Alice Example"`. Empty if absent.
    #[serde(default)]
    pub name: String,
    pub email: String,
}

/// Lightweight `Email` projection used for list views.
///
/// Carries enough to render an inbox row without fetching the
/// full body. The body parts are intentionally NOT included —
/// the sync engine pulls them on demand (`fetch_email`).
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct EmailSummary {
    pub id: String,
    #[serde(rename = "blobId", default)]
    pub blob_id: String,
    #[serde(rename = "threadId")]
    pub thread_id: String,
    #[serde(rename = "mailboxIds", default)]
    pub mailbox_ids: BTreeMap<String, bool>,
    #[serde(default)]
    pub keywords: BTreeMap<String, bool>,
    #[serde(default)]
    pub size: u64,
    #[serde(rename = "receivedAt", default = "default_epoch")]
    pub received_at: DateTime<Utc>,
    #[serde(rename = "sentAt", default)]
    pub sent_at: Option<DateTime<Utc>>,
    #[serde(default)]
    pub from: Vec<EmailAddress>,
    #[serde(default)]
    pub to: Vec<EmailAddress>,
    #[serde(default)]
    pub cc: Vec<EmailAddress>,
    #[serde(default)]
    pub bcc: Vec<EmailAddress>,
    #[serde(rename = "replyTo", default)]
    pub reply_to: Vec<EmailAddress>,
    #[serde(default)]
    pub subject: String,
    #[serde(default)]
    pub preview: String,
    #[serde(rename = "hasAttachment", default)]
    pub has_attachment: bool,
}

/// Full `Email` projection — same as `EmailSummary` plus body
/// parts and headers.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Email {
    #[serde(flatten)]
    pub summary: EmailSummary,
    #[serde(rename = "bodyValues", default)]
    pub body_values: BTreeMap<String, EmailBodyValue>,
    #[serde(rename = "textBody", default)]
    pub text_body: Vec<EmailBodyPart>,
    #[serde(rename = "htmlBody", default)]
    pub html_body: Vec<EmailBodyPart>,
    #[serde(default)]
    pub attachments: Vec<EmailBodyPart>,
    /// All headers preserved verbatim. Used for the "View original"
    /// debug pane in the desktop client.
    #[serde(default)]
    pub headers: Vec<EmailHeader>,
}

/// Raw header line.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct EmailHeader {
    pub name: String,
    pub value: String,
}

/// Body part metadata. Body bytes live keyed in `bodyValues`.
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct EmailBodyPart {
    #[serde(rename = "partId", default)]
    pub part_id: String,
    #[serde(rename = "blobId", default)]
    pub blob_id: String,
    #[serde(rename = "type", default)]
    pub mime_type: String,
    #[serde(default)]
    pub charset: Option<String>,
    #[serde(default)]
    pub disposition: Option<String>,
    #[serde(default)]
    pub name: Option<String>,
    #[serde(default)]
    pub language: Option<Vec<String>>,
    #[serde(default)]
    pub size: u64,
}

/// Decoded body bytes (UTF-8 already normalised by the BFF).
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct EmailBodyValue {
    pub value: String,
    #[serde(rename = "isEncodingProblem", default)]
    pub is_encoding_problem: bool,
    #[serde(rename = "isTruncated", default)]
    pub is_truncated: bool,
}

/// Outbound draft used by `KMailClient::send_email`.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct EmailDraft {
    /// Source mailbox (e.g. Drafts). Required by JMAP.
    #[serde(rename = "mailboxIds")]
    pub mailbox_ids: BTreeMap<String, bool>,
    #[serde(default)]
    pub from: Vec<EmailAddress>,
    #[serde(default)]
    pub to: Vec<EmailAddress>,
    #[serde(default)]
    pub cc: Vec<EmailAddress>,
    #[serde(default)]
    pub bcc: Vec<EmailAddress>,
    #[serde(rename = "replyTo", default)]
    pub reply_to: Vec<EmailAddress>,
    #[serde(default)]
    pub subject: String,
    #[serde(rename = "bodyText", default)]
    pub body_text: Option<String>,
    #[serde(rename = "bodyHtml", default)]
    pub body_html: Option<String>,
    /// `inReplyTo` header value (Message-ID being replied to).
    #[serde(rename = "inReplyTo", default)]
    pub in_reply_to: Option<String>,
    /// `references` header (parent thread Message-IDs).
    #[serde(default)]
    pub references: Vec<String>,
}

fn default_epoch() -> DateTime<Utc> {
    DateTime::<Utc>::from_timestamp(0, 0).expect("unix epoch is in range")
}

/// JMAP `capabilities` is a `{ urn: {...} }` map per RFC 8620.
/// The web client and the BFF both treat the URN keys as the
/// authoritative list; we deserialise into a Vec to keep ordering
/// predictable for tests.
fn deserialize_capabilities<'de, D>(deserializer: D) -> Result<Vec<String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    use serde::de::{self, MapAccess, Visitor};
    use std::fmt;

    struct CapVisitor;

    impl<'de> Visitor<'de> for CapVisitor {
        type Value = Vec<String>;

        fn expecting(&self, f: &mut fmt::Formatter) -> fmt::Result {
            f.write_str("a JMAP capabilities object or capability URN array")
        }

        fn visit_map<A>(self, mut map: A) -> Result<Vec<String>, A::Error>
        where
            A: MapAccess<'de>,
        {
            let mut out = Vec::new();
            while let Some((key, _value)) = map.next_entry::<String, serde::de::IgnoredAny>()? {
                out.push(key);
            }
            Ok(out)
        }

        fn visit_seq<A>(self, mut seq: A) -> Result<Vec<String>, A::Error>
        where
            A: de::SeqAccess<'de>,
        {
            let mut out = Vec::new();
            while let Some(v) = seq.next_element::<String>()? {
                out.push(v);
            }
            Ok(out)
        }
    }

    deserializer.deserialize_any(CapVisitor)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The capabilities field can arrive as either a `{ urn: {} }`
    /// map (per RFC 8620, which is what Stalwart and the BFF
    /// emit) or as a `[urn, urn]` array (used by some
    /// older test fixtures). Both must round-trip.
    #[test]
    fn capabilities_accepts_map_and_array() {
        let from_map: JmapSession = serde_json::from_value(serde_json::json!({
            "apiUrl": "/jmap/api",
            "capabilities": {
                "urn:ietf:params:jmap:core": {},
                "urn:ietf:params:jmap:mail": {}
            }
        }))
        .unwrap();
        assert_eq!(from_map.capabilities.len(), 2);
        assert!(from_map
            .capabilities
            .iter()
            .any(|c| c == "urn:ietf:params:jmap:core"));

        let from_arr: JmapSession = serde_json::from_value(serde_json::json!({
            "apiUrl": "/jmap/api",
            "capabilities": ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"]
        }))
        .unwrap();
        assert_eq!(from_arr.capabilities.len(), 2);
    }

    /// `MailboxRole::Vault` is a KMail extension. It must round-trip
    /// through serde so the SQLite cache layer can store + restore
    /// it without losing the vault flag.
    #[test]
    fn mailbox_role_vault_roundtrips() {
        let raw = serde_json::json!({
            "id": "mbx-1",
            "name": "Confidential",
            "role": "vault",
            "isVault": true
        });
        let mbx: Mailbox = serde_json::from_value(raw).unwrap();
        assert_eq!(mbx.role, Some(MailboxRole::Vault));
        assert!(mbx.is_vault);

        let back = serde_json::to_value(&mbx).unwrap();
        assert_eq!(back["role"], "vault");
        assert_eq!(back["isVault"], true);
    }

    /// Unknown roles must not fail deserialisation — the BFF can
    /// add new ones (e.g. `scheduled`) without breaking older SDK
    /// builds.
    #[test]
    fn mailbox_role_unknown_is_lenient() {
        let raw = serde_json::json!({
            "id": "mbx-1",
            "name": "Weird",
            "role": "schedulednotyetimplemented"
        });
        let mbx: Mailbox = serde_json::from_value(raw).unwrap();
        assert_eq!(mbx.role, Some(MailboxRole::Unknown));
    }
}
