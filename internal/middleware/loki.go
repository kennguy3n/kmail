// Package middleware — Loki labels for structured request logs.
//
// Phase 7 ships the Loki + Promtail observability stack alongside
// the existing Prometheus + OpenTelemetry pieces. Promtail
// scrapes the BFF's structured-JSON request log (already emitted
// by `RequestLogger` when `KMAIL_LOG_FORMAT=json`) and ships the
// records to Loki; the Grafana datasource at
// `deploy/grafana/datasources.yml` lets operators query the
// resulting stream alongside Prometheus metrics.
//
// This file owns the small piece of code that pre-computes the
// Loki labels (`tenant_id`, `route`, `status_class`) so Promtail's
// pipeline stages can lift them out of the JSON body. Promtail
// itself runs as a sidecar and ingests files; we keep the
// labelling in code so the JSON line is self-describing.
package middleware

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// LokiLabels controls how RequestLogger derives Loki labels. The
// zero value is fine for most deployments; tests override
// individual hooks.
type LokiLabels struct {
	// Job is the Loki `job` label (e.g. "kmail-api"). Falls back
	// to "kmail-api" when empty.
	Job string
	// Env is the Loki `env` label (e.g. "prod" / "staging").
	// Empty omits the label so a single config can be reused
	// across environments without forcing a tag.
	Env string
}

// LokiLogLine is the per-request shape Promtail's pipeline reads.
// It is a strict superset of the existing JSON request log so the
// Phase 4 logger (`RequestLogger`) keeps working even when Loki
// is not configured.
type LokiLogLine struct {
	TS         string `json:"ts"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	Route      string `json:"route"`
	Status     int    `json:"status"`
	StatusCls  string `json:"status_class"`
	DurationMs int64  `json:"duration_ms"`
	TenantID   string `json:"tenant_id,omitempty"`
	UserID     string `json:"user_id,omitempty"`
	TraceID    string `json:"trace_id,omitempty"`
	Job        string `json:"job"`
	Env        string `json:"env,omitempty"`
}

// AsJSON marshals the line as a single-line JSON value with a
// trailing newline. RequestLogger writes this directly to its
// configured logger so the output works with both stdout-tailing
// Promtail and a journald-aware Loki sidecar.
func (l LokiLogLine) AsJSON() (string, error) {
	b, err := json.Marshal(l)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// BuildLokiLine constructs a LokiLogLine for the given request /
// response. It is intentionally pure so callers can unit-test
// label derivation without spinning up an HTTP handler.
func BuildLokiLine(start time.Time, r *http.Request, status int, dur time.Duration, labels LokiLabels) LokiLogLine {
	job := labels.Job
	if job == "" {
		job = "kmail-api"
	}
	return LokiLogLine{
		TS:         start.UTC().Format(time.RFC3339Nano),
		Method:     r.Method,
		Path:       r.URL.Path,
		Route:      routeFor(r.URL.Path),
		Status:     status,
		StatusCls:  statusClass(status),
		DurationMs: dur.Milliseconds(),
		TenantID:   TenantIDFrom(r.Context()),
		UserID:     KChatUserIDFrom(r.Context()),
		TraceID:    TraceIDFrom(r.Context()),
		Job:        job,
		Env:        labels.Env,
	}
}

// statusClass returns "2xx" / "3xx" / "4xx" / "5xx" / "1xx" for a
// given HTTP status code. Loki dashboards typically slice on the
// class rather than every individual status.
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	case status >= 100:
		return "1xx"
	default:
		return fmt.Sprintf("%d", status)
	}
}

// routeFor collapses high-cardinality URL paths into a low-card
// route label. Specifically, we replace any UUID-shaped path
// segment with `:id` and any numeric segment with `:n`. This
// avoids exploding Loki's stream count under a `route` label.
func routeFor(p string) string {
	if p == "" {
		return "/"
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		switch {
		case isUUID(seg):
			parts[i] = ":id"
		case isAllDigits(seg):
			parts[i] = ":n"
		}
	}
	return strings.Join(parts, "/")
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
