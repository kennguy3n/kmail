# 0005 — BFF→Stalwart mutual TLS via cert-manager

- **Status**: Accepted
- **Related**: [`../../deploy/helm/kmail/values.yaml`](../../deploy/helm/kmail/values.yaml) (`mtls`), `deploy/helm/kmail/templates/stalwart-mtls.yaml`, `internal/jmap/proxy.go`, [`../DEVELOPMENT.md`](../DEVELOPMENT.md)

## Context

The BFF proxies JMAP to Stalwart. Historically it relied on a
trusted-network posture (plain HTTP plus a trusted header) — fine on a
flat, private network, but weak in a real cluster where lateral
movement or a compromised neighbour could reach Stalwart's port and
impersonate the BFF or read mail traffic.

## Decision

Support **mutual TLS** between the BFF and Stalwart, wired by
cert-manager. When `mtls.enabled=true`, the chart renders two
`Certificate` resources from a shared Issuer — a client cert mounted on
`kmail-api` and a server cert mounted on the Stalwart StatefulSet — and
overrides `KMAIL_STALWART_URL` to the HTTPS listener (port 8443) so SNI
matches a SAN. Short cert lifetimes (default 24h, 8h renewal) are paired
with Reloader to restart pods on rotation. Local dev keeps plain HTTP to
`http://localhost:8080`.

## Consequences

- Both ends authenticate cryptographically; the BFF is the only client
  Stalwart accepts. The proxy logs a startup WARN on an mTLS + bare
  `.svc` hostname mismatch so misconfiguration surfaces immediately.
- The chosen Issuer **must** emit `ca.crt`; if it doesn't, the BFF fails
  fast (no safe default trust anchor). Resolutions are documented in
  DEVELOPMENT.md "cert-manager Issuer must emit `ca.crt`".
- The server cert SAN list is generated from `stalwart.replicaCount` at
  render time, so Stalwart MUST be scaled via `helm upgrade`, never raw
  `kubectl scale` (see
  [upgrade](../operator/upgrade.md#scaling-stalwart-with-mtls)).
- Default is `mtls.enabled=false` for backwards-compatibility with
  existing dev/staging clusters; production is expected to enable it.
