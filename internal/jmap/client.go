// Package jmap hosts the Go BFF's JMAP client logic: speak JMAP
// to Stalwart on behalf of the React client, translate KChat OIDC
// auth into Stalwart auth, enforce tenant policy, and manage
// capability negotiation.
//
// See docs/JMAP-CONTRACT.md for the contract this package
// implements against, and docs/ARCHITECTURE.md §7 for the Go
// service topology.
package jmap

// Client is the placeholder JMAP client used by the BFF.
//
// It will hold the upstream Stalwart JMAP endpoint, token minter,
// capability allowlist, rate limiter, and observability hooks in
// Phase 2.
type Client struct{}
