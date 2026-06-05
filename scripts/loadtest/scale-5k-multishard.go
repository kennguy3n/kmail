// scale-5k-multishard is the multi-shard scale driver for KMail. It
// extends the single-stack scale-5k harness (see
// scripts/loadtest/scale-5k.go) to a sharded fleet and measures the
// three operational numbers Task 7 of the platform-reliability
// workstream calls out:
//
//   - cross-shard routing latency — how much the BFF's tenant→shard
//     resolution adds on a cold (cache-miss) request versus a warm
//     (cache-hit) one, broken down per shard.
//   - shard failover time — how long tenants on a shard are
//     unreachable when that shard is drained and its tenants are
//     rebalanced onto the rest of the fleet.
//   - rebalance duration — wall-clock of a POST .../rebalance call.
//
// The driver routes every request to a specific tenant via the
// dev-bypass routing header (X-KMail-Dev-Tenant-Id), exactly how the
// BFF resolves a shard in load tests, so traffic fans out across
// every shard the target tenants live on.
//
// Tenant/shard inventory comes from one of three sources:
//
//   --discover            pull the live fleet: GET /api/v1/admin/shards
//                         for shards and GET /api/v1/tenants for tenant
//                         ids (what you run against a seeded env).
//   --tenant-ids a,b,c    an explicit tenant id list (skip discovery).
//   neither (dry-run)     synthesize --tenants ids across --shards
//                         shards; only valid with --dry-run since
//                         synthetic ids do not route.
//
// Usage:
//
//	go run ./scripts/loadtest/scale-5k-multishard.go \
//	  --api-url http://localhost:8088 --auth-token kmail-dev \
//	  --discover --tenants 5000 --shards 10 --workers 64 \
//	  --rampup 1m --steady 10m --cooldown 1m \
//	  --failover --rebalance \
//	  --json-out multishard-report.json
//
// --failover and --rebalance mutate fleet state (drain, rebalance)
// and are OFF by default; a plain run only measures routing latency
// read-only. --dry-run validates the plan, writes a zero-stat JSON
// summary so the reporting path is exercised, and exits without
// touching the network — this is what `make scale-test` can self-check.
//
//go:build ignore

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	apiURL      = flag.String("api-url", "http://localhost:8088", "BFF base URL")
	authToken   = flag.String("auth-token", "kmail-dev", "Bearer token (dev bypass by default)")
	numTenants  = flag.Int("tenants", 5000, "Tenant count to spread across shards")
	numShards   = flag.Int("shards", 10, "Shard count (synthetic plan / sanity check)")
	workers     = flag.Int("workers", 64, "Parallel worker count")
	rampUp      = flag.Duration("rampup", 1*time.Minute, "Ramp-up duration")
	steady      = flag.Duration("steady", 10*time.Minute, "Steady-state duration")
	cooldown    = flag.Duration("cooldown", 1*time.Minute, "Cool-down duration")
	iterations  = flag.Int("iterations", 200000, "Total request budget across all workers")
	discover    = flag.Bool("discover", false, "Discover shards+tenants from the live API")
	tenantIDs   = flag.String("tenant-ids", "", "Explicit comma-separated tenant ids (skips discovery)")
	doFailover  = flag.Bool("failover", false, "Run the failover drill (drains a shard; MUTATES fleet)")
	doRebalance = flag.Bool("rebalance", false, "Time a rebalance on the busiest shard (MUTATES fleet)")
	jsonOut     = flag.String("json-out", "", "Optional path to write JSON summary")
	dryRun      = flag.Bool("dry-run", false, "Validate plan + write zero-stat JSON; no network")
)

// shardInfo is the subset of the /api/v1/admin/shards row this driver
// needs to plan and report.
type shardInfo struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Status           string `json:"status"`
	MaxMailboxes     int    `json:"max_mailboxes"`
	CurrentMailboxes int    `json:"current_mailboxes"`
}

// tenant pairs a tenant id with the shard it is planned/observed to
// live on (shard id may be empty when the placement is unknown, e.g.
// discovery returned tenants but no per-tenant assignment surface).
type tenant struct {
	ID      string
	ShardID string
}

// sample is one observed request outcome.
type sample struct {
	ShardID string
	Latency time.Duration
	Cold    bool // first touch of this tenant (routing cache miss)
	Err     string
}

// shardStat is the per-shard rollup emitted in the JSON summary.
type shardStat struct {
	ShardID   string  `json:"shard_id"`
	Name      string  `json:"name,omitempty"`
	N         int     `json:"n"`
	ColdP50ms float64 `json:"cold_p50_ms"`
	ColdP95ms float64 `json:"cold_p95_ms"`
	WarmP50ms float64 `json:"warm_p50_ms"`
	WarmP95ms float64 `json:"warm_p95_ms"`
	RoutingMs float64 `json:"routing_overhead_ms"` // cold_p50 - warm_p50
	ErrPct    float64 `json:"err_pct"`
}

// summary is the machine-readable run report.
type summary struct {
	GeneratedAt    time.Time   `json:"generated_at"`
	APIURL         string      `json:"api_url"`
	Tenants        int         `json:"tenants"`
	Shards         int         `json:"shards"`
	Workers        int         `json:"workers"`
	DurationS      float64     `json:"duration_s"`
	TotalRequests  int         `json:"total_requests"`
	OverallErrPct  float64     `json:"overall_err_pct"`
	PerShard       []shardStat `json:"per_shard"`
	FailoverMs     float64     `json:"failover_ms,omitempty"`
	FailoverShard  string      `json:"failover_shard,omitempty"`
	RebalanceMs    float64     `json:"rebalance_ms,omitempty"`
	RebalanceShard string      `json:"rebalance_shard,omitempty"`
	Note           string      `json:"note,omitempty"`
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "scale-5k-multishard:", err)
		os.Exit(1)
	}
}

func run() error {
	if *numShards <= 0 {
		return errors.New("--shards must be > 0")
	}
	if *numTenants <= 0 {
		return errors.New("--tenants must be > 0")
	}

	client := &http.Client{Timeout: 15 * time.Second}

	// ---- inventory ------------------------------------------------
	var shards []shardInfo
	var tenants []tenant
	switch {
	case *tenantIDs != "":
		for _, id := range splitCSV(*tenantIDs) {
			tenants = append(tenants, tenant{ID: id})
		}
	case *discover && !*dryRun:
		var err error
		if shards, err = fetchShards(client); err != nil {
			return fmt.Errorf("discover shards: %w", err)
		}
		if tenants, err = fetchTenants(client); err != nil {
			return fmt.Errorf("discover tenants: %w", err)
		}
	default:
		// Synthetic plan — only meaningful for --dry-run since these
		// ids will not route against a real BFF.
		if !*dryRun {
			return errors.New("no real inventory: pass --discover or --tenant-ids (synthetic ids only work with --dry-run)")
		}
		shards = synthShards(*numShards)
		tenants = synthTenants(*numTenants, *numShards)
	}
	if len(tenants) == 0 {
		return errors.New("no tenants to drive")
	}

	plan := fmt.Sprintf("plan: %d tenant(s) across %d shard(s), %d workers, phases %s/%s/%s, budget %d req",
		len(tenants), max(len(shards), *numShards), *workers, *rampUp, *steady, *cooldown, *iterations)
	fmt.Println(plan)

	if *dryRun {
		fmt.Println("dry-run: configuration valid; no network calls made")
		s := summary{
			GeneratedAt: time.Now().UTC(),
			APIURL:      *apiURL,
			Tenants:     len(tenants),
			Shards:      max(len(shards), *numShards),
			Workers:     *workers,
			Note:        "dry-run: zero-stat summary (reporting path self-check)",
		}
		return writeJSON(s)
	}

	// ---- steady-state routing-latency load ------------------------
	samples := driveLoad(client, tenants)

	s := summary{
		GeneratedAt:   time.Now().UTC(),
		APIURL:        *apiURL,
		Tenants:       len(tenants),
		Shards:        max(len(shards), *numShards),
		Workers:       *workers,
		DurationS:     (*rampUp + *steady + *cooldown).Seconds(),
		TotalRequests: len(samples),
	}
	s.PerShard, s.OverallErrPct = rollup(samples, shards)

	// ---- failover drill (optional, mutates fleet) -----------------
	if *doFailover {
		ms, shardID, err := failoverDrill(client, shards, tenants)
		if err != nil {
			fmt.Fprintln(os.Stderr, "failover drill:", err)
		} else {
			s.FailoverMs = ms
			s.FailoverShard = shardID
		}
	}

	// ---- rebalance timing (optional, mutates fleet) ---------------
	if *doRebalance {
		ms, shardID, err := timeRebalance(client, shards)
		if err != nil {
			fmt.Fprintln(os.Stderr, "rebalance timing:", err)
		} else {
			s.RebalanceMs = ms
			s.RebalanceShard = shardID
		}
	}

	printReport(s)
	return writeJSON(s)
}

// driveLoad runs the ramp/steady/cooldown phases, routing each request
// to a random tenant and recording per-shard latency. The first time a
// tenant is touched the request is tagged Cold (routing-cache miss).
func driveLoad(client *http.Client, tenants []tenant) []sample {
	totalDur := *rampUp + *steady + *cooldown
	ctx, cancel := context.WithTimeout(context.Background(), totalDur)
	defer cancel()

	out := make(chan sample, 4096)
	var sent int64
	var seen sync.Map // tenantID -> struct{}, marks first touch

	var wg sync.WaitGroup
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))
			for {
				if atomic.AddInt64(&sent, 1) > int64(*iterations) {
					return
				}
				if ctx.Err() != nil {
					return
				}
				t := tenants[rng.Intn(len(tenants))]
				_, cold := seen.LoadOrStore(t.ID, struct{}{})
				cold = !cold // LoadOrStore returns loaded=true when present
				start := time.Now()
				err := mailboxGet(ctx, client, t.ID)
				sm := sample{ShardID: t.ShardID, Latency: time.Since(start), Cold: cold}
				if err != nil {
					sm.Err = err.Error()
				}
				select {
				case out <- sm:
				default:
				}
			}
		}(i)
	}
	go func() { wg.Wait(); close(out) }()

	var samples []sample
	for sm := range out {
		samples = append(samples, sm)
	}
	return samples
}

// rollup computes per-shard cold/warm percentiles and the routing
// overhead (cold_p50 - warm_p50). Samples with an empty shard id are
// bucketed under "(unknown)".
func rollup(samples []sample, shards []shardInfo) ([]shardStat, float64) {
	names := map[string]string{}
	for _, sh := range shards {
		names[sh.ID] = sh.Name
	}
	type bucket struct {
		cold, warm []time.Duration
		errs, n    int
	}
	buckets := map[string]*bucket{}
	totalErr := 0
	for _, s := range samples {
		key := s.ShardID
		if key == "" {
			key = "(unknown)"
		}
		b := buckets[key]
		if b == nil {
			b = &bucket{}
			buckets[key] = b
		}
		b.n++
		if s.Err != "" {
			b.errs++
			totalErr++
			continue
		}
		if s.Cold {
			b.cold = append(b.cold, s.Latency)
		} else {
			b.warm = append(b.warm, s.Latency)
		}
	}
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var stats []shardStat
	for _, k := range keys {
		b := buckets[k]
		coldP50, coldP95 := pctl(b.cold, 50), pctl(b.cold, 95)
		warmP50, warmP95 := pctl(b.warm, 50), pctl(b.warm, 95)
		errPct := 0.0
		if b.n > 0 {
			errPct = 100 * float64(b.errs) / float64(b.n)
		}
		stats = append(stats, shardStat{
			ShardID:   k,
			Name:      names[k],
			N:         b.n,
			ColdP50ms: ms(coldP50), ColdP95ms: ms(coldP95),
			WarmP50ms: ms(warmP50), WarmP95ms: ms(warmP95),
			RoutingMs: ms(coldP50) - ms(warmP50),
			ErrPct:    errPct,
		})
	}
	overallErr := 0.0
	if len(samples) > 0 {
		overallErr = 100 * float64(totalErr) / float64(len(samples))
	}
	return stats, overallErr
}

// failoverDrill drains one shard, rebalances its tenants onto the rest
// of the fleet, and measures the time until a tenant that lived on the
// drained shard answers successfully again. The shard is restored to
// active afterwards. Returns failover ms and the shard id exercised.
func failoverDrill(client *http.Client, shards []shardInfo, tenants []tenant) (float64, string, error) {
	target := pickActiveShard(shards)
	if target.ID == "" {
		return 0, "", errors.New("no active shard to drill")
	}
	victim := firstTenantOnShard(tenants, target.ID)
	if victim == "" {
		return 0, "", fmt.Errorf("no tenant on shard %s to observe", target.ID)
	}

	// Drain the shard, then rebalance its tenants off it.
	if err := setShardStatus(client, target.ID, "draining"); err != nil {
		return 0, "", fmt.Errorf("drain: %w", err)
	}
	defer func() { _ = setShardStatus(client, target.ID, "active") }()

	start := time.Now()
	if _, err := postRebalance(client, target.ID); err != nil {
		return 0, "", fmt.Errorf("rebalance: %w", err)
	}
	// Poll the victim tenant until it answers from its new shard.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		if err := mailboxGet(ctx, client, victim); err == nil {
			return ms(time.Since(start)), target.ID, nil
		}
		if ctx.Err() != nil {
			return ms(time.Since(start)), target.ID, errors.New("victim tenant did not recover within 60s")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// timeRebalance times a rebalance call on the busiest active shard.
func timeRebalance(client *http.Client, shards []shardInfo) (float64, string, error) {
	target := busiestShard(shards)
	if target.ID == "" {
		return 0, "", errors.New("no shard to rebalance")
	}
	start := time.Now()
	if _, err := postRebalance(client, target.ID); err != nil {
		return 0, "", err
	}
	return ms(time.Since(start)), target.ID, nil
}

// ---- HTTP helpers -------------------------------------------------

func mailboxGet(ctx context.Context, c *http.Client, tenantID string) error {
	body := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": []any{
			[]any{"Mailbox/get", map[string]any{"accountId": "kmail-dev"}, "c0"},
		},
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *apiURL+"/jmap", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*authToken)
	req.Header.Set("Content-Type", "application/json")
	if tenantID != "" {
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("jmap %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func fetchShards(c *http.Client) ([]shardInfo, error) {
	var shards []shardInfo
	if err := getJSON(c, "/api/v1/admin/shards", &shards); err != nil {
		// Some deployments wrap the list: {"shards":[...]}.
		var wrap struct {
			Shards []shardInfo `json:"shards"`
		}
		if err2 := getJSON(c, "/api/v1/admin/shards", &wrap); err2 != nil {
			return nil, err
		}
		shards = wrap.Shards
	}
	return shards, nil
}

func fetchTenants(c *http.Client) ([]tenant, error) {
	var raw []struct {
		ID      string `json:"id"`
		ShardID string `json:"shard_id"`
	}
	if err := getJSON(c, "/api/v1/tenants", &raw); err != nil {
		var wrap struct {
			Tenants []struct {
				ID      string `json:"id"`
				ShardID string `json:"shard_id"`
			} `json:"tenants"`
		}
		if err2 := getJSON(c, "/api/v1/tenants", &wrap); err2 != nil {
			return nil, err
		}
		raw = wrap.Tenants
	}
	var out []tenant
	for _, r := range raw {
		out = append(out, tenant{ID: r.ID, ShardID: r.ShardID})
	}
	return out, nil
}

func getJSON(c *http.Client, path string, v any) error {
	req, err := http.NewRequest(http.MethodGet, *apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*authToken)
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s -> %d: %s", path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return json.Unmarshal(b, v)
}

func setShardStatus(c *http.Client, id, status string) error {
	body, _ := json.Marshal(map[string]any{"status": status})
	req, err := http.NewRequest(http.MethodPut, *apiURL+"/api/v1/admin/shards/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+*authToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("PUT shard %s -> %d: %s", id, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func postRebalance(c *http.Client, id string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, *apiURL+"/api/v1/admin/shards/"+id+"/rebalance", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+*authToken)
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("POST rebalance %s -> %d: %s", id, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

// ---- plan/selection helpers --------------------------------------

func synthShards(n int) []shardInfo {
	out := make([]shardInfo, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, shardInfo{
			ID:           fmt.Sprintf("shard-%02d", i),
			Name:         fmt.Sprintf("synthetic-shard-%02d", i),
			Status:       "active",
			MaxMailboxes: 100000,
		})
	}
	return out
}

func synthTenants(n, shards int) []tenant {
	out := make([]tenant, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, tenant{
			ID:      fmt.Sprintf("tenant-%05d", i),
			ShardID: fmt.Sprintf("shard-%02d", i%shards),
		})
	}
	return out
}

func pickActiveShard(shards []shardInfo) shardInfo {
	for _, s := range shards {
		if s.Status == "active" {
			return s
		}
	}
	return shardInfo{}
}

func busiestShard(shards []shardInfo) shardInfo {
	best := shardInfo{}
	bestLoad := -1.0
	for _, s := range shards {
		if s.MaxMailboxes <= 0 {
			continue
		}
		load := float64(s.CurrentMailboxes) / float64(s.MaxMailboxes)
		if load > bestLoad {
			bestLoad = load
			best = s
		}
	}
	if best.ID == "" && len(shards) > 0 {
		return shards[0]
	}
	return best
}

func firstTenantOnShard(tenants []tenant, shardID string) string {
	for _, t := range tenants {
		if t.ShardID == shardID {
			return t.ID
		}
	}
	return ""
}

// ---- output helpers ----------------------------------------------

func printReport(s summary) {
	fmt.Printf("\nmulti-shard scale report — %d requests across %d shard(s), overall err %.2f%%\n",
		s.TotalRequests, len(s.PerShard), s.OverallErrPct)
	fmt.Println("shard            n      cold_p50  cold_p95  warm_p50  warm_p95  route_ovh  err%")
	fmt.Println("--------------------------------------------------------------------------------")
	for _, st := range s.PerShard {
		fmt.Printf("%-15s %6d  %8.1f  %8.1f  %8.1f  %8.1f  %8.1f  %5.2f\n",
			truncate(st.ShardID, 15), st.N, st.ColdP50ms, st.ColdP95ms, st.WarmP50ms, st.WarmP95ms, st.RoutingMs, st.ErrPct)
	}
	if s.FailoverShard != "" {
		fmt.Printf("\nfailover: shard %s drained+rebalanced; tenant recovered in %.0f ms\n", s.FailoverShard, s.FailoverMs)
	}
	if s.RebalanceShard != "" {
		fmt.Printf("rebalance: shard %s rebalanced in %.0f ms\n", s.RebalanceShard, s.RebalanceMs)
	}
}

func writeJSON(s summary) error {
	if *jsonOut == "" {
		return nil
	}
	blob, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(*jsonOut, blob, 0o644)
}

func pctl(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	idx := (len(ds) * p) / 100
	if idx >= len(ds) {
		idx = len(ds) - 1
	}
	return ds[idx]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
