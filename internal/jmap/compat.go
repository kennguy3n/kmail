// Package jmap — Stalwart v0.16.0 ↔ v1.0.0 compatibility shim.
//
// The KMail BFF speaks JMAP to a pinned Stalwart version (currently
// v0.16.0; see `docs/STALWART_UPGRADE.md` and the version pin note
// in `README.md`). Stalwart v1.0.0 is expected H1 2026 and the
// upstream changelog has flagged a small number of breaking
// changes:
//
//   - The JMAP session response `urn` capability identifiers
//     drop the `:stalwart-` prefix and align with the IANA
//     registry (`urn:ietf:params:jmap:...`). Older clients still
//     accept both.
//   - The admin registry moves from JMAP `x:Domain/set` and
//     friends to a typed `urn:stalwart:admin:*` namespace; the
//     wire shape is otherwise identical.
//   - The JSON envelope for error responses gains a `code`
//     field; the existing `type` field stays present for at
//     least one major version.
//
// This shim detects the Stalwart version on first request (cached
// per shard) and exposes a `Adapter` that callers consult before
// shaping outbound requests or interpreting responses. The shim is
// deliberately permissive: when the version cannot be resolved we
// fall back to the v0 shape so an unreachable Stalwart never
// breaks the BFF startup path.
//
// The runbook (`docs/STALWART_UPGRADE.md`) documents the staged
// rollout: blue/green pair with v0.16.0 + v1.0.0 nodes, the
// `scripts/test-stalwart-upgrade.sh` harness that runs
// `scripts/test-e2e.sh` against each version sequentially, and
// the rollback procedure (re-pin compose, redeploy, re-run e2e).
package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// StalwartVersion is the parsed semantic version of an upstream
// Stalwart instance. v0.16.0 is the current pin; v1.0.0 is the
// upcoming major release.
type StalwartVersion struct {
	Major int
	Minor int
	Patch int
	Raw   string
}

// IsAtLeast returns true when v ≥ (major, minor).
func (v StalwartVersion) IsAtLeast(major, minor int) bool {
	if v.Major != major {
		return v.Major > major
	}
	return v.Minor >= minor
}

// IsV1 is shorthand for IsAtLeast(1, 0). Callers gate v1-specific
// behaviour on this method so the call site reads as intent
// rather than version arithmetic.
func (v StalwartVersion) IsV1() bool { return v.IsAtLeast(1, 0) }

// String renders the version as "MAJOR.MINOR.PATCH". Used in logs
// and the `/admin/stalwart/version` endpoint.
func (v StalwartVersion) String() string {
	if v.Raw != "" {
		return v.Raw
	}
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// ParseStalwartVersion parses a Stalwart-style version string. It
// accepts a leading `v` and a trailing `-suffix` (e.g.
// `v1.0.0-rc.1`); only the leading numeric components matter.
func ParseStalwartVersion(s string) (StalwartVersion, error) {
	v := StalwartVersion{Raw: s}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return v, fmt.Errorf("stalwart version %q: expected MAJOR.MINOR[.PATCH]", v.Raw)
	}
	get := func(idx int) (int, error) {
		if idx >= len(parts) {
			return 0, nil
		}
		n := 0
		for _, c := range parts[idx] {
			if c < '0' || c > '9' {
				return 0, fmt.Errorf("stalwart version %q: non-numeric component %q", v.Raw, parts[idx])
			}
			n = n*10 + int(c-'0')
		}
		return n, nil
	}
	var err error
	if v.Major, err = get(0); err != nil {
		return v, err
	}
	if v.Minor, err = get(1); err != nil {
		return v, err
	}
	if v.Patch, err = get(2); err != nil {
		return v, err
	}
	return v, nil
}

// Adapter applies version-specific transforms to JMAP requests and
// responses. The zero value is the v0 (legacy) adapter.
type Adapter struct {
	Version StalwartVersion
}

// AdaptCapabilityURN rewrites a capability URN so it matches the
// detected Stalwart's preferred namespace. v1+ accepts both legacy
// `urn:stalwart:*` and IANA `urn:ietf:params:jmap:*` URNs; we
// always emit IANA URNs against v1 so log scrapers see consistent
// values, but echo whatever the caller passed when targetting v0.
func (a Adapter) AdaptCapabilityURN(urn string) string {
	if !a.Version.IsV1() {
		return urn
	}
	if strings.HasPrefix(urn, "urn:stalwart:") {
		return "urn:ietf:params:jmap:" + strings.TrimPrefix(urn, "urn:stalwart:")
	}
	return urn
}

// AdaptAdminMethod maps a v0 admin-registry method name onto its
// v1 equivalent. v0 uses the `x:` namespace (`x:Domain/set`); v1
// uses the typed admin namespace (`urn:stalwart:admin:Domain/set`).
func (a Adapter) AdaptAdminMethod(method string) string {
	if !a.Version.IsV1() {
		return method
	}
	if strings.HasPrefix(method, "x:") {
		return "urn:stalwart:admin:" + strings.TrimPrefix(method, "x:")
	}
	return method
}

// AdaptErrorEnvelope normalises an upstream error envelope so the
// BFF can present a single shape to clients regardless of which
// Stalwart version produced it. v0 emits `{"type": "...",
// "description": "..."}`; v1 adds a `code` integer alongside.
func (a Adapter) AdaptErrorEnvelope(raw map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range raw {
		out[k] = v
	}
	if _, ok := out["type"]; !ok {
		if c, ok := out["code"]; ok {
			out["type"] = fmt.Sprintf("%v", c)
		}
	}
	return out
}

// VersionDetector resolves and caches the Stalwart version for a
// given JMAP session URL. Detection is best-effort: if the
// upstream cannot be reached the cached value is returned and the
// detector retries on the next call.
type VersionDetector struct {
	Client *http.Client
	TTL    time.Duration

	mu    sync.Mutex
	cache map[string]versionCacheEntry
}

type versionCacheEntry struct {
	value     StalwartVersion
	cachedAt  time.Time
	probedErr error
}

// NewVersionDetector returns a detector with a 5-minute cache TTL
// and a 5-second HTTP timeout. Tests can override Client and TTL.
func NewVersionDetector() *VersionDetector {
	return &VersionDetector{
		Client: &http.Client{Timeout: 5 * time.Second},
		TTL:    5 * time.Minute,
		cache:  map[string]versionCacheEntry{},
	}
}

// Detect probes the JMAP session endpoint at sessionURL and
// returns the parsed Stalwart version. The session response shape
// (`{"capabilities": {...}, "primaryAccounts": {...},
// "stalwartVersion": "v1.0.0"}`) is conservative — older
// Stalwart builds omit `stalwartVersion` and we fall back to the
// `Server: Stalwart/v0.16.0` response header.
func (d *VersionDetector) Detect(ctx context.Context, sessionURL string) (StalwartVersion, error) {
	d.mu.Lock()
	if d.cache == nil {
		d.cache = map[string]versionCacheEntry{}
	}
	if entry, ok := d.cache[sessionURL]; ok && time.Since(entry.cachedAt) < d.TTL {
		d.mu.Unlock()
		if entry.probedErr != nil {
			return entry.value, entry.probedErr
		}
		return entry.value, nil
	}
	d.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sessionURL, nil)
	if err != nil {
		return StalwartVersion{}, err
	}
	resp, err := d.Client.Do(req)
	if err != nil {
		return d.cacheError(sessionURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	var session struct {
		StalwartVersion string `json:"stalwartVersion"`
		ServerInfo      struct {
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	_ = json.Unmarshal(body, &session)
	raw := session.StalwartVersion
	if raw == "" {
		raw = session.ServerInfo.Version
	}
	if raw == "" {
		raw = parseServerHeader(resp.Header.Get("Server"))
	}
	if raw == "" {
		return d.cacheError(sessionURL, errors.New("stalwart version: not present in session or Server header"))
	}
	v, err := ParseStalwartVersion(raw)
	if err != nil {
		return d.cacheError(sessionURL, err)
	}
	d.mu.Lock()
	d.cache[sessionURL] = versionCacheEntry{value: v, cachedAt: time.Now()}
	d.mu.Unlock()
	return v, nil
}

func (d *VersionDetector) cacheError(sessionURL string, err error) (StalwartVersion, error) {
	d.mu.Lock()
	d.cache[sessionURL] = versionCacheEntry{cachedAt: time.Now(), probedErr: err}
	d.mu.Unlock()
	return StalwartVersion{}, err
}

// parseServerHeader extracts a version out of strings shaped like
// `Stalwart/v0.16.0` or `Stalwart-Mail/1.0.0`.
func parseServerHeader(server string) string {
	if server == "" {
		return ""
	}
	for _, tok := range strings.Fields(server) {
		if i := strings.Index(tok, "/"); i >= 0 {
			rhs := tok[i+1:]
			if rhs != "" {
				return rhs
			}
		}
	}
	return ""
}
