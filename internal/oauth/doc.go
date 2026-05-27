// Package oauth implements the KMail OAuth2 authorization server
// used by third-party integrations (Zapier, Slack apps, custom
// webhooks) to access tenant data on behalf of an authenticated
// KMail user.
//
// # Scope
//
// This package owns:
//
//   - The OAuth2 authorization code grant with PKCE (RFC 7636).
//     Public clients (SPAs / mobile apps) MUST use PKCE;
//     confidential clients MAY use PKCE in addition to a
//     client_secret. Plain `client_credentials` is intentionally
//     NOT supported — every access token must be tied to a user
//     so the row-level-security policies have a tenant context.
//
//   - The refresh-token grant with token rotation (RFC 6819).
//     Each successful refresh issues a new refresh token and
//     marks the old one revoked. A subsequent /oauth/token call
//     that presents an already-revoked refresh token with a
//     non-NULL successor is treated as a replay attack and
//     revokes the entire successor chain.
//
//   - The /oauth/revoke endpoint (RFC 7009). Revoking a refresh
//     token cascades to every access token derived from it.
//
//   - A bearer-token middleware (`AuthMiddleware`) that maps a
//     valid access token to a tenant_id / user_id / scopes set
//     in the request context, mirroring the shape of the OIDC
//     middleware so downstream handlers can treat both auth
//     populations uniformly.
//
//   - Admin handlers for registering and rotating OAuth clients
//     within a tenant.
//
// # Out of scope
//
// The package does NOT host the consent UI; the
// /oauth/authorize endpoint expects the request to ride a
// valid OIDC user session and renders a minimal consent page
// from a Go template. A richer consent screen lives in the web
// client and POSTs back to /oauth/authorize/approve. The actual
// "third-party callable" API surface (JMAP, calendar, etc.) is
// authenticated independently — those handlers chain
// `oauth.AuthMiddleware` BEFORE the OIDC middleware so OAuth
// tokens are validated first and fall through to OIDC only if
// the request carries no bearer token.
//
// # Storage
//
// Persistent state lives in the four oauth_* tables created by
// `migrations/001_baseline.sql`:
//
//   - oauth_clients: registered applications.
//   - oauth_authorization_codes: short-lived (60s) codes from
//     /authorize, exchanged for tokens at /token.
//   - oauth_access_tokens: bearer tokens hash-stored so a DB
//     compromise does not yield valid tokens.
//   - oauth_refresh_tokens: long-lived tokens with rotation.
//
// All token plaintexts are SHA-256 hashed before storage; the
// plaintext exists only in the response body and in the
// third-party app's memory.
package oauth
