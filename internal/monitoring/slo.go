// Package monitoring — availability SLO tracking.
//
// Phase 4 closes the "Availability target: 99.9%" checklist item.
// The SLOTracker accepts per-request success/failure samples from
// the metrics middleware, persists them in a Valkey-backed sliding
// window, and exposes percentile / breach-history queries through
// the admin handlers.
//
// Storage layout (one logical key per tenant):
//   slo:{tenantID}:requests          ZSET  score=ts_ns  member=ts_ns:status_code
//   slo:{tenantID}:latency           ZSET  score=ts_ns  member=ts_ns:latency_ms
//
// We use sorted sets rather than HyperLogLog because we need
// percentile queries on latency and full success/fail counts on
// the request stream — HLL only supports cardinality. Keys are
// trimmed to the longest configured window (`MaxWindow`, default
// 7d) at every record, so memory stays bounded.
package monitoring

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultTarget is the platform availability SLO (99.9%).
const DefaultTarget = 0.999

// SLOTracker is the in-process recorder + Valkey reader.
type SLOTracker struct {
	valkey    *redis.Client
	target    float64
	maxWindow time.Duration
	now       func() time.Time
}

// NewSLOTracker returns a tracker. When `valkey` is nil every
// recording is a no-op and reads return zero values — matches the
// dev-without-Valkey posture other services use.
func NewSLOTracker(valkey *redis.Client) *SLOTracker {
	return &SLOTracker{
		valkey:    valkey,
		target:    DefaultTarget,
		maxWindow: 7 * 24 * time.Hour,
		now:       time.Now,
	}
}

// WithTarget overrides the SLO target (default 99.9%).
func (s *SLOTracker) WithTarget(t float64) *SLOTracker {
	s.target = t
	return s
}

// RecordRequest is called from the metrics middleware on every
// completed request. `success` is true iff the response was a
// non-5xx, non-transport-error response.
func (s *SLOTracker) RecordRequest(ctx context.Context, tenantID string, success bool, latencyMs int64) {
	if s == nil || s.valkey == nil {
		return
	}
	now := s.now().UTC()
	score := float64(now.UnixNano())
	status := 0
	if !success {
		status = 1
	}
	cutoff := float64(now.Add(-s.maxWindow).UnixNano())
	cutoffStr := strconv.FormatFloat(cutoff, 'f', 0, 64)
	pipe := s.valkey.Pipeline()
	// Always mirror every sample to the platform-wide sentinel key
	// so `GET /api/v1/admin/slo` (which calls GetAvailability with
	// tenantID == "") aggregates across all tenants. When tenantID
	// is empty (e.g. an unauthenticated request) we still record
	// to the platform key so platform availability stays correct.
	pipe.ZAdd(ctx, requestsKey(""), redis.Z{Score: score, Member: fmt.Sprintf("%d:%d", now.UnixNano(), status)})
	pipe.ZAdd(ctx, latencyKey(""), redis.Z{Score: score, Member: fmt.Sprintf("%d:%d", now.UnixNano(), latencyMs)})
	pipe.ZRemRangeByScore(ctx, requestsKey(""), "0", cutoffStr)
	pipe.ZRemRangeByScore(ctx, latencyKey(""), "0", cutoffStr)
	if tenantID != "" {
		pipe.ZAdd(ctx, requestsKey(tenantID), redis.Z{Score: score, Member: fmt.Sprintf("%d:%d", now.UnixNano(), status)})
		pipe.ZAdd(ctx, latencyKey(tenantID), redis.Z{Score: score, Member: fmt.Sprintf("%d:%d", now.UnixNano(), latencyMs)})
		pipe.ZRemRangeByScore(ctx, requestsKey(tenantID), "0", cutoffStr)
		pipe.ZRemRangeByScore(ctx, latencyKey(tenantID), "0", cutoffStr)
	}
	_, _ = pipe.Exec(ctx)
}

// AvailabilityResult is the response shape for GetAvailability.
type AvailabilityResult struct {
	TenantID    string  `json:"tenant_id"`
	WindowSec   int     `json:"window_seconds"`
	Total       int     `json:"total"`
	Successes   int     `json:"successes"`
	Failures    int     `json:"failures"`
	Availability float64 `json:"availability"`
	Target      float64 `json:"target"`
}

// GetAvailability returns the success ratio over the trailing
// `window`. `tenantID` may be empty to read the platform-wide key
// (caller responsibility to record into the same key).
func (s *SLOTracker) GetAvailability(ctx context.Context, tenantID string, window time.Duration) (*AvailabilityResult, error) {
	if s == nil || s.valkey == nil {
		t := DefaultTarget
		if s != nil {
			t = s.target
		}
		return &AvailabilityResult{TenantID: tenantID, Target: t}, nil
	}
	low, high := s.windowBounds(window)
	members, err := s.valkey.ZRangeByScore(ctx, requestsKey(tenantID), &redis.ZRangeBy{Min: low, Max: high}).Result()
	if err != nil {
		return nil, err
	}
	res := &AvailabilityResult{
		TenantID:  tenantID,
		WindowSec: int(window.Seconds()),
		Target:    s.target,
	}
	for _, m := range members {
		_, status, ok := splitMember(m)
		if !ok {
			continue
		}
		res.Total++
		if status == 0 {
			res.Successes++
		} else {
			res.Failures++
		}
	}
	if res.Total > 0 {
		res.Availability = float64(res.Successes) / float64(res.Total)
	}
	return res, nil
}

// LatencyPercentiles is the response shape for GetLatencyPercentiles.
type LatencyPercentiles struct {
	TenantID  string `json:"tenant_id"`
	WindowSec int    `json:"window_seconds"`
	Count     int    `json:"count"`
	P50Ms     int64  `json:"p50_ms"`
	P95Ms     int64  `json:"p95_ms"`
	P99Ms     int64  `json:"p99_ms"`
}

// GetLatencyPercentiles returns the P50/P95/P99 latencies over the
// trailing `window`.
func (s *SLOTracker) GetLatencyPercentiles(ctx context.Context, tenantID string, window time.Duration) (*LatencyPercentiles, error) {
	if s == nil || s.valkey == nil {
		return &LatencyPercentiles{TenantID: tenantID}, nil
	}
	low, high := s.windowBounds(window)
	members, err := s.valkey.ZRangeByScore(ctx, latencyKey(tenantID), &redis.ZRangeBy{Min: low, Max: high}).Result()
	if err != nil {
		return nil, err
	}
	values := make([]int64, 0, len(members))
	for _, m := range members {
		_, ms, ok := splitMember(m)
		if !ok {
			continue
		}
		values = append(values, ms)
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	res := &LatencyPercentiles{
		TenantID:  tenantID,
		WindowSec: int(window.Seconds()),
		Count:     len(values),
	}
	res.P50Ms = percentile(values, 50)
	res.P95Ms = percentile(values, 95)
	res.P99Ms = percentile(values, 99)
	return res, nil
}

// SLOBreach is one period where availability dropped below the
// target. Currently emitted as a one-shot bucket per evaluation
// window for the BFF history endpoint.
type SLOBreach struct {
	TenantID     string    `json:"tenant_id"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`
	Availability float64   `json:"availability"`
	Target       float64   `json:"target"`
}

// ListSLOBreaches returns synthetic breach buckets generated from
// the persisted request stream by walking the trailing 24h in
// 1-hour slices. We compute breaches on read rather than persist
// them so a target change re-classifies historical data correctly.
func (s *SLOTracker) ListSLOBreaches(ctx context.Context, tenantID string) ([]SLOBreach, error) {
	if s == nil || s.valkey == nil {
		return nil, nil
	}
	now := s.now().UTC()
	var out []SLOBreach
	for i := 0; i < 24; i++ {
		end := now.Add(-time.Duration(i) * time.Hour)
		start := end.Add(-time.Hour)
		low := strconv.FormatInt(start.UnixNano(), 10)
		high := strconv.FormatInt(end.UnixNano(), 10)
		members, err := s.valkey.ZRangeByScore(ctx, requestsKey(tenantID), &redis.ZRangeBy{Min: low, Max: high}).Result()
		if err != nil {
			return nil, err
		}
		var ok, total int
		for _, m := range members {
			_, status, parsed := splitMember(m)
			if !parsed {
				continue
			}
			total++
			if status == 0 {
				ok++
			}
		}
		if total == 0 {
			continue
		}
		ratio := float64(ok) / float64(total)
		if ratio < s.target {
			out = append(out, SLOBreach{
				TenantID:     tenantID,
				StartedAt:    start,
				EndedAt:      end,
				Availability: ratio,
				Target:       s.target,
			})
		}
	}
	return out, nil
}

func (s *SLOTracker) windowBounds(window time.Duration) (low, high string) {
	now := s.now().UTC()
	from := now.Add(-window)
	return strconv.FormatInt(from.UnixNano(), 10), strconv.FormatInt(now.UnixNano(), 10)
}

// platformKeyToken is the sentinel used in the Valkey key when no
// tenant scope applies (the platform-wide rollup). We prefer an
// explicit token over an empty segment so the key shape is
// unambiguous and an accidental empty-tenantID write doesn't
// silently land in another tenant's namespace.
const platformKeyToken = "_platform"

func keyTenant(tenantID string) string {
	if tenantID == "" {
		return platformKeyToken
	}
	return tenantID
}

func requestsKey(tenantID string) string { return "slo:" + keyTenant(tenantID) + ":requests" }
func latencyKey(tenantID string) string  { return "slo:" + keyTenant(tenantID) + ":latency" }

func splitMember(m string) (ts int64, value int64, ok bool) {
	idx := strings.LastIndex(m, ":")
	if idx <= 0 || idx == len(m)-1 {
		return 0, 0, false
	}
	t, err := strconv.ParseInt(m[:idx], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	v, err := strconv.ParseInt(m[idx+1:], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return t, v, true
}

func percentile(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	idx := (p * (len(sorted) - 1)) / 100
	return sorted[idx]
}

// ErrNotFound is exported so handlers can map it onto a 404. Kept
// in the SLO package even though it is unused today — Phase 5 will
// add per-tenant SLO config that may 404 when missing.
var ErrNotFound = errors.New("slo: not found")
