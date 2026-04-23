// Package bridge hosts the Chat Bridge and Calendar Bridge
// business logic. Together they implement the "KMail lives inside
// KChat" integration surface.
//
// See docs/ARCHITECTURE.md §7 and docs/JMAP-CONTRACT.md §5.3 for
// push fan-out into KChat notifications.
package bridge

// ChatService is the placeholder root type for the Chat Bridge.
//
// It will hold dependencies (KChat API client, Stalwart JMAP
// client, Go BFF client) and expose email ↔ channel operations
// in Phase 2: share-to-channel, alert routing, task extraction,
// and JMAP-push-to-KChat-notification fan-out.
type ChatService struct{}
