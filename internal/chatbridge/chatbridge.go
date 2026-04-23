// Package chatbridge hosts the Email-to-Chat Bridge business
// logic — the "KMail lives inside KChat" integration surface on
// the mail side.
//
// Responsibilities (per docs/PROPOSAL.md §5 and
// docs/ARCHITECTURE.md §7): share-to-channel, alert routing from
// aliases like alerts@ into a channel, task extraction from mail,
// and fan JMAP push events into KChat notifications (see
// docs/JMAP-CONTRACT.md §5.3).
package chatbridge

// Service is the placeholder root type for the Chat Bridge.
//
// It will hold dependencies (KChat API client, Stalwart JMAP
// client, Go BFF client) and expose email ↔ channel operations
// in Phase 2.
type Service struct{}
