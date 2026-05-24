// Package valkeyurl normalises the `KMAIL_VALKEY_URL` /
// `VALKEY_URL` environment variable into a `*redis.Options` ready
// for `redis.NewClient`. Two wire formats are accepted, which
// matches the dual convention used across the codebase:
//
//   - Full DSN: `redis://host:port[/db][?param=value]` (or
//     `rediss://…` for TLS). This is the form the Helm chart
//     emits into the Kubernetes Secret because it carries
//     parameters cleanly across Helm rendering and supports
//     `rediss://` for managed Valkey clusters.
//
//   - Bare authority: `host:port` (e.g. `valkey:6379`). This is
//     the form `docker-compose.yml` uses and the form
//     `redis.NewClient` accepts in its `Options.Addr` field.
//
// Without this normalisation, code that passes the raw string to
// `redis.Options{Addr: ...}` (e.g. `cmd/kmail-api/main.go`) will
// try to resolve `redis://valkey` as a hostname and fail with a
// DNS error in any Helm deployment that ships the chart's default
// DSN-formatted Secret value.
package valkeyurl

import (
	"errors"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Parse normalises `raw` into `*redis.Options`. An empty input
// returns an error so callers can decide whether to fall back to
// an in-process counter (dev) or refuse to boot (production).
//
// Scheme detection is case-insensitive: `redis.ParseURL` itself
// accepts mixed-case schemes per RFC 3986 §3.1, so refusing
// `Redis://...` here would create a needless gap between the two
// code paths. Operator-pasted overrides (env files, shell exports)
// are the most likely producers of mixed case; routing them
// through `redis.ParseURL` keeps query-string parameters working.
func Parse(raw string) (*redis.Options, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("valkeyurl: empty url")
	}
	// Match the scheme prefix on the lowercased form. Only the
	// scheme prefix is lower-cased for the comparison; the
	// original string is passed to `redis.ParseURL` so any
	// case-sensitive parts (passwords, paths, query values)
	// survive unchanged.
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "redis://") || strings.HasPrefix(lower, "rediss://") {
		return redis.ParseURL(trimmed)
	}
	return &redis.Options{Addr: trimmed}, nil
}
