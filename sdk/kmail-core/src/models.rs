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
    ///
    /// On the wire JMAP encodes capabilities as a `{urn: {...}}`
    /// object (RFC 8620 §2). We deserialise both that shape *and*
    /// a `[urn, urn]` array for legacy fixtures, and serialise back
    /// out to the canonical `{urn: {}}` map so a round-trip
    /// (e.g. the CLI's `run_session` pretty-printer) matches the
    /// raw BFF response byte-for-byte modulo whitespace.
    #[serde(
        default,
        deserialize_with = "deserialize_capabilities",
        serialize_with = "serialize_capabilities"
    )]
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

impl MailboxRole {
    /// Canonical lowercase identifier for this role, matching the
    /// JMAP wire spelling exactly.
    ///
    /// This is the *only* sanctioned way for SDK / shell code to
    /// compare a role against a string — the previous habit of
    /// using `format!("{:?}", role).to_lowercase()` produced a
    /// fragile coupling to the `Debug` derive's output and made
    /// substring matches (e.g. `"a".contains(inbox_role)`) silently
    /// match the wrong variant (`archive`, `all`, `flagged`, ...).
    pub const fn canonical_name(self) -> &'static str {
        match self {
            MailboxRole::Inbox => "inbox",
            MailboxRole::Archive => "archive",
            MailboxRole::Drafts => "drafts",
            MailboxRole::Sent => "sent",
            MailboxRole::Trash => "trash",
            MailboxRole::Junk => "junk",
            MailboxRole::Important => "important",
            MailboxRole::All => "all",
            MailboxRole::Flagged => "flagged",
            MailboxRole::Vault => "vault",
            MailboxRole::Unknown => "unknown",
        }
    }

    /// Inverse of `canonical_name`. Returns `None` for inputs that
    /// don't match a known role label — callers should treat that
    /// as a configuration error rather than silently defaulting to
    /// `Unknown` so a typo in `ClientConfig::bootstrap_mailbox_role`
    /// surfaces explicitly.
    pub fn from_canonical_name(s: &str) -> Option<Self> {
        match s {
            "inbox" => Some(MailboxRole::Inbox),
            "archive" => Some(MailboxRole::Archive),
            "drafts" => Some(MailboxRole::Drafts),
            "sent" => Some(MailboxRole::Sent),
            "trash" => Some(MailboxRole::Trash),
            "junk" => Some(MailboxRole::Junk),
            "important" => Some(MailboxRole::Important),
            "all" => Some(MailboxRole::All),
            "flagged" => Some(MailboxRole::Flagged),
            "vault" => Some(MailboxRole::Vault),
            _ => None,
        }
    }
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

/// Serialise the SDK's internal `Vec<String>` capability list back
/// out as the canonical JMAP `{urn: {}}` map shape. Each URN maps to
/// an empty object — the per-capability arguments object — because
/// the SDK doesn't model capability args today; downstream consumers
/// that care about args parse the raw response themselves.
fn serialize_capabilities<S>(caps: &[String], ser: S) -> Result<S::Ok, S::Error>
where
    S: serde::Serializer,
{
    use serde::ser::SerializeMap;
    let mut m = ser.serialize_map(Some(caps.len()))?;
    for cap in caps {
        m.serialize_entry(cap, &serde_json::Map::<String, serde_json::Value>::new())?;
    }
    m.end()
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

    /// `JmapSession` MUST re-serialise `capabilities` as the
    /// canonical RFC 8620 `{urn: {}}` map shape rather than the
    /// lossy `[urn]` array shape that comes from the underlying
    /// `Vec<String>`. This pins the round-trip property that the
    /// CLI's `run_session` pretty-printer relies on for engineers
    /// comparing SDK output against raw BFF responses.
    #[test]
    fn capabilities_serializes_as_rfc8620_map() {
        let s: JmapSession = serde_json::from_value(serde_json::json!({
            "apiUrl": "/jmap/api",
            "capabilities": {
                "urn:ietf:params:jmap:core": {},
                "urn:ietf:params:jmap:mail": {}
            }
        }))
        .unwrap();
        let back = serde_json::to_value(&s).unwrap();
        let caps = back
            .get("capabilities")
            .expect("capabilities field present");
        assert!(
            caps.is_object(),
            "capabilities must serialise as a map, got: {caps}"
        );
        let map = caps.as_object().unwrap();
        assert_eq!(map.len(), 2);
        for urn in ["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"] {
            assert!(map.contains_key(urn), "missing urn {urn}");
            assert!(
                map.get(urn).unwrap().is_object(),
                "urn {urn} must map to an object, not {:?}",
                map.get(urn)
            );
        }
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

    /// `canonical_name` must match the JMAP wire spelling exactly
    /// (lowercase, no underscores) and round-trip through
    /// `from_canonical_name`. This is the contract that the
    /// bootstrap-mailbox lookup in `KMailClient::initial_email_pull`
    /// depends on; if it ever silently drifts (e.g. because a new
    /// variant is added and forgotten here), the inbox lookup will
    /// fail at runtime — but the test will catch it at build time.
    #[test]
    fn mailbox_role_canonical_name_matches_wire() {
        let all = [
            MailboxRole::Inbox,
            MailboxRole::Archive,
            MailboxRole::Drafts,
            MailboxRole::Sent,
            MailboxRole::Trash,
            MailboxRole::Junk,
            MailboxRole::Important,
            MailboxRole::All,
            MailboxRole::Flagged,
            MailboxRole::Vault,
        ];
        for r in all {
            let name = r.canonical_name();
            assert_eq!(
                MailboxRole::from_canonical_name(name),
                Some(r),
                "canonical_name round-trip failed for {r:?}"
            );

            // The canonical name must also match the serde-encoded
            // form (i.e. the on-the-wire JMAP spelling). That's the
            // whole point of using this method instead of the
            // Debug-derived string.
            let wire = serde_json::to_string(&r).unwrap();
            // serde_json wraps strings in quotes.
            assert_eq!(wire, format!("\"{}\"", name));
        }

        // Unknown is the catch-all and has no wire spelling of its
        // own, but `canonical_name` still has to return *something*
        // distinct so it can't collide with a real role.
        assert_eq!(MailboxRole::Unknown.canonical_name(), "unknown");
        assert!(MailboxRole::from_canonical_name("unknown").is_none());

        // Misspellings must NOT match — the substring-collision
        // regression that motivated this method (e.g. `"a"` matching
        // `archive`) must stay fixed.
        assert!(MailboxRole::from_canonical_name("a").is_none());
        assert!(MailboxRole::from_canonical_name("INBOX").is_none());
        assert!(MailboxRole::from_canonical_name("").is_none());
    }
}
