// Push payload → renderable local notification.
//
// The transport-level push (APNs / FCM / Web Push) only carries
// the metadata the BFF chose to embed; the SDK turns that into a
// platform-agnostic [`LocalNotification`] that each app shell maps
// onto its native notification centre (iOS
// `UNUserNotificationCenter`, Android `NotificationManagerCompat`,
// Electron `Notification`). Keeping the title / body composition
// here — rather than in three separate app shells — means the
// "New message from …" wording, the (no subject) fallback, and the
// per-email dedupe `tag` stay identical across every platform and
// are unit-tested in one place.

use crate::push::EmailDeliveryHint;
use serde::{Deserialize, Serialize};

/// A ready-to-render local notification.
///
/// Every field is platform-neutral. The shell maps them onto its
/// native API:
///
/// - `title` → notification title (sender display).
/// - `body` → notification body (subject, then snippet).
/// - `tag` → dedupe / replace key, set to the JMAP email id. A
///   re-delivered push (APNs/FCM are at-least-once) updates the
///   existing banner instead of stacking a duplicate. iOS maps this
///   onto the request identifier, Android the notification tag, Web
///   Push the `tag` option.
/// - the remaining ids let the shell deep-link straight to the
///   message when the user taps the notification.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct LocalNotification {
    pub title: String,
    pub body: String,
    pub tag: String,
    pub account_id: Option<String>,
    pub email_id: Option<String>,
    pub mailbox_id: Option<String>,
    pub thread_id: Option<String>,
    pub received_at_unix: Option<i64>,
    pub has_attachment: bool,
}

/// Title shown when the push payload carried no sender display.
const FALLBACK_TITLE: &str = "New message";
/// Body shown when the push payload carried neither subject nor
/// snippet.
const FALLBACK_BODY: &str = "(no subject)";

impl LocalNotification {
    /// Build a notification from a decoded email-delivery hint.
    ///
    /// Returns `None` when the hint carries no email id. That can't
    /// happen for a hint produced by [`EmailDeliveryHint::from_data`]
    /// (which already requires `email_id`), but a hand-built hint
    /// could omit it — and a notification without a stable `tag`
    /// would be silently coalesced by the OS, so we refuse to build
    /// one rather than emit a tagless banner.
    pub fn from_email_delivery(hint: &EmailDeliveryHint) -> Option<Self> {
        let email_id = hint.email_id.clone().filter(|s| !s.trim().is_empty())?;

        let title = non_empty(hint.from.as_deref()).unwrap_or(FALLBACK_TITLE).to_string();
        let body = non_empty(hint.subject.as_deref())
            .or_else(|| non_empty(hint.snippet.as_deref()))
            .unwrap_or(FALLBACK_BODY)
            .to_string();

        Some(Self {
            title,
            body,
            tag: email_id.clone(),
            account_id: hint.account_id.clone(),
            email_id: Some(email_id),
            mailbox_id: hint.mailbox_id.clone(),
            thread_id: hint.thread_id.clone(),
            received_at_unix: hint.received_at_unix,
            has_attachment: hint.has_attachment.unwrap_or(false),
        })
    }
}

/// Return the trimmed-non-empty string slice, or `None` for a
/// missing / whitespace-only value. Keeps the builder from
/// surfacing a blank title or body when the BFF emitted an empty
/// string rather than omitting the key.
fn non_empty(s: Option<&str>) -> Option<&str> {
    s.map(str::trim).filter(|s| !s.is_empty())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    fn data(pairs: &[(&str, &str)]) -> BTreeMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_string(), (*v).to_string()))
            .collect()
    }

    #[test]
    fn builds_full_notification_from_hint() {
        let hint = EmailDeliveryHint::from_data(&data(&[
            ("account_id", "a-1"),
            ("email_id", "e-42"),
            ("mailbox_id", "mb-inbox"),
            ("thread_id", "t-7"),
            ("from", "Alice Example <alice@example.com>"),
            ("subject", "Lunch?"),
            ("snippet", "Are you free at noon"),
            ("received_at_unix", "1700000000"),
            ("has_attachment", "true"),
        ]))
        .expect("hint");

        let n = LocalNotification::from_email_delivery(&hint).expect("notification");
        assert_eq!(n.title, "Alice Example <alice@example.com>");
        assert_eq!(n.body, "Lunch?");
        assert_eq!(n.tag, "e-42", "tag must be the email id for dedupe");
        assert_eq!(n.email_id.as_deref(), Some("e-42"));
        assert_eq!(n.mailbox_id.as_deref(), Some("mb-inbox"));
        assert_eq!(n.thread_id.as_deref(), Some("t-7"));
        assert_eq!(n.received_at_unix, Some(1_700_000_000));
        assert!(n.has_attachment);
    }

    #[test]
    fn falls_back_to_snippet_when_subject_absent() {
        let hint = EmailDeliveryHint::from_data(&data(&[
            ("email_id", "e-1"),
            ("snippet", "body preview text"),
        ]))
        .expect("hint");
        let n = LocalNotification::from_email_delivery(&hint).expect("notification");
        assert_eq!(n.title, FALLBACK_TITLE);
        assert_eq!(n.body, "body preview text");
        assert!(!n.has_attachment);
    }

    #[test]
    fn falls_back_to_placeholders_when_metadata_blank() {
        // BFF emitted empty strings rather than omitting the keys —
        // the builder must still produce a non-blank title/body.
        let hint = EmailDeliveryHint::from_data(&data(&[
            ("email_id", "e-9"),
            ("from", "   "),
            ("subject", ""),
            ("snippet", "  "),
        ]))
        .expect("hint");
        let n = LocalNotification::from_email_delivery(&hint).expect("notification");
        assert_eq!(n.title, FALLBACK_TITLE);
        assert_eq!(n.body, FALLBACK_BODY);
    }

    #[test]
    fn refuses_to_build_without_email_id() {
        // Hand-built hint with no email id → no stable dedupe tag.
        let hint = EmailDeliveryHint {
            email_id: None,
            subject: Some("orphan".into()),
            ..EmailDeliveryHint::default()
        };
        assert!(LocalNotification::from_email_delivery(&hint).is_none());
    }
}
