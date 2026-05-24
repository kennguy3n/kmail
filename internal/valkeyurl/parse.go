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
func Parse(raw string) (*redis.Options, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("valkeyurl: empty url")
	}
	if strings.HasPrefix(raw, "redis://") || strings.HasPrefix(raw, "rediss://") {
		return redis.ParseURL(raw)
	}
	return &redis.Options{Addr: raw}, nil
}
