package monitoring

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// MultiRegionAggregator aggregates SLO data across BFF instances
// in different regions. Each region's SLOTracker writes its
// samples to keys prefixed with `slo:{region}:...` (in addition
// to the existing single-region keys); the aggregator scans
// those prefixes and folds the per-region totals into a single
// platform-wide view for the 99.95% Phase 5 target.
type MultiRegionAggregator struct {
	valkey    *redis.Client
	regions   []string
	target    float64
	maxWindow time.Duration
	now       func() time.Time
}

// NewMultiRegionAggregator returns an aggregator. `regions` is the
// allow-list of region tokens this BFF should fan out to (e.g.
// `["us-east-1","eu-west-1","ap-south-1"]`). Empty list falls
// back to the platform-wide key (single-region deployment).
func NewMultiRegionAggregator(valkey *redis.Client, regions []string) *MultiRegionAggregator {
	clean := make([]string, 0, len(regions))
	for _, r := range regions {
		r = strings.TrimSpace(r)
		if r != "" {
			clean = append(clean, r)
		}
	}
	return &MultiRegionAggregator{
		valkey:    valkey,
		regions:   clean,
		target:    DefaultTarget,
		maxWindow: 7 * 24 * time.Hour,
		now:       time.Now,
	}
}

// WithTarget overrides the aggregator's SLO target.
func (a *MultiRegionAggregator) WithTarget(t float64) *MultiRegionAggregator {
	a.target = t
	return a
}

// RegionAvailability is one region's roll-up.
type RegionAvailability struct {
	Region       string  `json:"region"`
	Total        int     `json:"total"`
	Successes    int     `json:"successes"`
	Failures     int     `json:"failures"`
	Availability float64 `json:"availability"`
	Target       float64 `json:"target"`
}

// MultiRegionResult is the aggregator's output.
type MultiRegionResult struct {
	WindowSec      int                  `json:"window_seconds"`
	Target         float64              `json:"target"`
	Regions        []RegionAvailability `json:"regions"`
	GlobalTotal    int                  `json:"global_total"`
	GlobalSuccess  int                  `json:"global_success"`
	GlobalFailures int                  `json:"global_failures"`
	GlobalAvail    float64              `json:"global_availability"`
}

// Aggregate reads per-region request streams and folds them into
// a global rollup. Returns an empty result (no error) when no
// regions are configured and Valkey is unavailable.
func (a *MultiRegionAggregator) Aggregate(ctx context.Context, window time.Duration) (*MultiRegionResult, error) {
	if a == nil || a.valkey == nil {
		return &MultiRegionResult{Target: DefaultTarget, WindowSec: int(window.Seconds())}, nil
	}
	now := a.now().UTC()
	low := strconv.FormatInt(now.Add(-window).UnixNano(), 10)
	high := strconv.FormatInt(now.UnixNano(), 10)

	res := &MultiRegionResult{
		WindowSec: int(window.Seconds()),
		Target:    a.target,
	}
	regions := a.regions
	if len(regions) == 0 {
		regions = []string{platformKeyToken}
	}
	sort.Strings(regions)
	for _, region := range regions {
		key := "slo:region:" + region + ":requests"
		members, err := a.valkey.ZRangeByScore(ctx, key, &redis.ZRangeBy{Min: low, Max: high}).Result()
		if err != nil {
			return nil, err
		}
		row := RegionAvailability{Region: region, Target: a.target}
		for _, m := range members {
			_, status, ok := splitMember(m)
			if !ok {
				continue
			}
			row.Total++
			if status == 0 {
				row.Successes++
			} else {
				row.Failures++
			}
		}
		if row.Total > 0 {
			row.Availability = float64(row.Successes) / float64(row.Total)
		}
		res.Regions = append(res.Regions, row)
		res.GlobalTotal += row.Total
		res.GlobalSuccess += row.Successes
		res.GlobalFailures += row.Failures
	}
	if res.GlobalTotal > 0 {
		res.GlobalAvail = float64(res.GlobalSuccess) / float64(res.GlobalTotal)
	}
	return res, nil
}

// ErrNoRegions is returned when an aggregator with no regions is
// asked for a per-region view that requires explicit fan-out.
var ErrNoRegions = errors.New("multiregion: no regions configured")
