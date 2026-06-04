// scale-5k is the multi-tenant scale load-test driver for KMail.
// It exercises the BFF / JMAP proxy across a fleet of seeded
// tenants (see scripts/loadtest/seed-tenants.go) with a realistic,
// weighted workload mix and three phases — ramp-up, steady state,
// and cool-down — while collecting per-operation P50/P95/P99
// latency, error rates, and throughput-over-time buckets.
//
// The run emits a machine-readable JSON summary (--json-out) that
// scripts/loadtest/report.go renders into a Markdown report with an
// SLO pass/fail verdict.
//
// Workload mix (must sum to 100):
//
//	inbox_open         40%   JMAP Mailbox/get
//	message_read       20%   JMAP Email/query + Email/get (backref)
//	search             15%   JMAP Email/query (text filter)
//	send               10%   JMAP Email/set (draft create)
//	calendar            5%   GET /api/v1/calendars/{accountId}
//	admin_api           5%   GET /api/v1/tenants/{id}/users
//	attachment_upload   5%   POST /api/v1/attachments/upload (multipart)
//
// Usage:
//
//	go run ./scripts/loadtest/scale-5k.go \
//	  --api-url http://localhost:8088 --auth-token kmail-dev \
//	  --tenants 100 --workers 64 \
//	  --rampup 1m --steady 10m --cooldown 1m \
//	  --json-out scale-report.json
//
// --dry-run validates the configuration (including that the
// workload weights sum to 100), prints the plan, writes a
// zero-stat JSON summary so the reporting path is exercised, and
// exits without touching the network. This is what `make
// scale-test` runs in its self-check.
//
//go:build ignore

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	mrand "math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------

type config struct {
	apiURL         string
	authToken      string
	tenants        int
	workers        int
	rampup         time.Duration
	steady         time.Duration
	cooldown       time.Duration
	bucket         time.Duration
	jsonOut        string
	slugPrefix     string
	attachmentSize int64
	maxSamples     int
	httpTimeout    time.Duration
	thinkTime      time.Duration
	dryRun         bool
	verbose        bool
}

func parseConfig() config {
	var c config
	flag.StringVar(&c.apiURL, "api-url", envOr("KMAIL_API_URL", "http://localhost:8088"), "BFF base URL")
	flag.StringVar(&c.authToken, "auth-token", envOr("KMAIL_DEV_BEARER", "kmail-dev"), "Bearer token (dev bypass by default)")
	flag.IntVar(&c.tenants, "tenants", 100, "Number of seeded tenants to spread load across")
	flag.IntVar(&c.workers, "workers", 64, "Peak concurrent virtual users")
	flag.DurationVar(&c.rampup, "rampup", time.Minute, "Ramp-up duration (0 -> peak workers)")
	flag.DurationVar(&c.steady, "steady", 10*time.Minute, "Steady-state duration at peak workers")
	flag.DurationVar(&c.cooldown, "cooldown", time.Minute, "Cool-down duration (peak -> 0 workers)")
	flag.DurationVar(&c.bucket, "bucket", 10*time.Second, "Throughput bucket width")
	flag.StringVar(&c.jsonOut, "json-out", "scale-report.json", "Path to write the JSON summary")
	flag.StringVar(&c.slugPrefix, "slug-prefix", "loadtest", "Only target tenants whose slug starts with this prefix")
	flag.Int64Var(&c.attachmentSize, "attachment-size", 5<<20, "Bytes uploaded by the attachment_upload op")
	flag.IntVar(&c.maxSamples, "max-samples", 200000, "Per-op latency reservoir cap (bounds memory)")
	flag.DurationVar(&c.httpTimeout, "http-timeout", 30*time.Second, "Per-request HTTP timeout")
	flag.DurationVar(&c.thinkTime, "think-time", 0, "Optional fixed delay between a worker's requests")
	flag.BoolVar(&c.dryRun, "dry-run", false, "Validate + print plan + write a zero-stat summary, no network")
	flag.BoolVar(&c.verbose, "verbose", false, "Verbose logging")
	flag.Parse()
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func (c config) total() time.Duration { return c.rampup + c.steady + c.cooldown }

func validate(c config) error {
	if c.workers < 1 {
		return fmt.Errorf("--workers must be >= 1, got %d", c.workers)
	}
	if c.tenants < 1 {
		return fmt.Errorf("--tenants must be >= 1, got %d", c.tenants)
	}
	if c.total() <= 0 {
		return fmt.Errorf("rampup+steady+cooldown must be > 0")
	}
	if c.bucket <= 0 {
		return fmt.Errorf("--bucket must be > 0")
	}
	if c.maxSamples < 1 {
		return fmt.Errorf("--max-samples must be >= 1")
	}
	if c.attachmentSize < 0 {
		return fmt.Errorf("--attachment-size must be >= 0")
	}
	sum := 0
	for _, op := range workload {
		sum += op.weight
	}
	if sum != 100 {
		return fmt.Errorf("workload weights must sum to 100, got %d", sum)
	}
	return nil
}

// ---------------------------------------------------------------
// Workload definition
// ---------------------------------------------------------------

type operation struct {
	name   string
	weight int
	run    func(ctx context.Context, c *client, t target) error
}

// workload is the weighted operation mix. Weights must sum to 100
// (enforced by validate).
var workload = []operation{
	{"inbox_open", 40, opInboxOpen},
	{"message_read", 20, opMessageRead},
	{"search", 15, opSearch},
	{"send", 10, opSend},
	{"calendar", 5, opCalendar},
	{"admin_api", 5, opAdminAPI},
	{"attachment_upload", 5, opAttachmentUpload},
}

// weightedPicker resolves a roll in [0,100) to an operation via a
// precomputed cumulative table.
type weightedPicker struct {
	cum []int
}

func newPicker() weightedPicker {
	cum := make([]int, len(workload))
	acc := 0
	for i, op := range workload {
		acc += op.weight
		cum[i] = acc
	}
	return weightedPicker{cum: cum}
}

func (p weightedPicker) pick(rng *mrand.Rand) operation {
	roll := rng.Intn(100)
	for i, c := range p.cum {
		if roll < c {
			return workload[i]
		}
	}
	return workload[len(workload)-1]
}

// ---------------------------------------------------------------
// Targets
// ---------------------------------------------------------------

// target is one (tenant, primary user) pair the load is driven
// against.
type target struct {
	TenantID    string
	AccountID   string
	KChatUserID string
}

// ---------------------------------------------------------------
// JSON summary schema (shared shape with report.go)
// ---------------------------------------------------------------

type meta struct {
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	APIURL      string    `json:"api_url"`
	Tenants     int       `json:"tenants"`
	Targets     int       `json:"targets"`
	Workers     int       `json:"workers"`
	RampupS     float64   `json:"rampup_s"`
	SteadyS     float64   `json:"steady_s"`
	CooldownS   float64   `json:"cooldown_s"`
	BucketS     float64   `json:"bucket_s"`
	AttachBytes int64     `json:"attachment_bytes"`
	DryRun      bool      `json:"dry_run"`
}

type opStat struct {
	Op        string  `json:"op"`
	Weight    int     `json:"weight"`
	N         int64   `json:"n"`
	Errors    int64   `json:"errors"`
	ErrorRate float64 `json:"error_rate_pct"`
	P50ms     float64 `json:"p50_ms"`
	P95ms     float64 `json:"p95_ms"`
	P99ms     float64 `json:"p99_ms"`
	MaxMs     float64 `json:"max_ms"`
	MeanMs    float64 `json:"mean_ms"`
}

type bucketStat struct {
	Index    int     `json:"index"`
	StartS   float64 `json:"start_s"`
	Phase    string  `json:"phase"`
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	RPS      float64 `json:"rps"`
}

type totals struct {
	N         int64   `json:"n"`
	Errors    int64   `json:"errors"`
	ErrorRate float64 `json:"error_rate_pct"`
	RPS       float64 `json:"rps"`
}

type summary struct {
	Meta       meta         `json:"meta"`
	Operations []opStat     `json:"operations"`
	Buckets    []bucketStat `json:"buckets"`
	Totals     totals       `json:"totals"`
}

// ---------------------------------------------------------------
// Metrics collection
// ---------------------------------------------------------------

// opCollector accumulates per-op latency samples (reservoir-bounded)
// and exact counts. One per worker to avoid lock contention; merged
// at the end.
type opCollector struct {
	n       int64
	errors  int64
	sumMs   float64
	maxMs   float64
	samples []float64
	seen    int64 // total offered to the reservoir
}

func (o *opCollector) record(latency time.Duration, isErr bool, cap int, rng *mrand.Rand) {
	o.n++
	ms := float64(latency.Microseconds()) / 1000.0
	o.sumMs += ms
	if ms > o.maxMs {
		o.maxMs = ms
	}
	if isErr {
		o.errors++
		return
	}
	// Reservoir sampling keeps memory bounded for multi-million
	// request runs while preserving an unbiased latency sample.
	o.seen++
	if len(o.samples) < cap {
		o.samples = append(o.samples, ms)
		return
	}
	j := rng.Int63n(o.seen)
	if j < int64(cap) {
		o.samples[j] = ms
	}
}

func (o *opCollector) merge(other *opCollector, cap int, rng *mrand.Rand) {
	o.n += other.n
	o.errors += other.errors
	o.sumMs += other.sumMs
	if other.maxMs > o.maxMs {
		o.maxMs = other.maxMs
	}
	for _, s := range other.samples {
		o.seen++
		if len(o.samples) < cap {
			o.samples = append(o.samples, s)
			continue
		}
		j := rng.Int63n(o.seen)
		if j < int64(cap) {
			o.samples[j] = s
		}
	}
}

func (o *opCollector) finalize(name string, weight int) opStat {
	st := opStat{Op: name, Weight: weight, N: o.n, Errors: o.errors, MaxMs: round1(o.maxMs)}
	succ := o.n - o.errors
	if o.n > 0 {
		st.ErrorRate = round2(100.0 * float64(o.errors) / float64(o.n))
	}
	if succ > 0 {
		st.MeanMs = round1(o.sumMs / float64(succ))
	}
	if len(o.samples) > 0 {
		sort.Float64s(o.samples)
		st.P50ms = round1(percentile(o.samples, 50))
		st.P95ms = round1(percentile(o.samples, 95))
		st.P99ms = round1(percentile(o.samples, 99))
	}
	return st
}

func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	// Nearest-rank: rank = ceil(p/100 * n), 1-indexed.
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func round1(f float64) float64 { return float64(int64(f*10+0.5)) / 10 }
func round2(f float64) float64 { return float64(int64(f*100+0.5)) / 100 }

// ---------------------------------------------------------------
// Main
// ---------------------------------------------------------------

func main() {
	cfg := parseConfig()
	if err := validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "scale-5k: %v\n", err)
		os.Exit(2)
	}

	attachmentSize = cfg.attachmentSize
	printPlan(cfg)

	if cfg.dryRun {
		// Exercise the reporting path end-to-end without network by
		// writing a zero-stat summary the reporter can render.
		s := emptySummary(cfg, 0)
		if err := writeSummary(cfg.jsonOut, s); err != nil {
			fmt.Fprintf(os.Stderr, "scale-5k: write summary: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("scale-5k: DRY RUN wrote zero-stat summary to %s\n", cfg.jsonOut)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cl := &client{
		baseURL: strings.TrimRight(cfg.apiURL, "/"),
		token:   cfg.authToken,
		http:    &http.Client{Timeout: cfg.httpTimeout},
	}

	targets, err := discoverTargets(ctx, cl, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scale-5k: target discovery failed: %v\n", err)
		os.Exit(1)
	}
	if len(targets) == 0 {
		fmt.Fprintf(os.Stderr, "scale-5k: no seeded tenants matching slug prefix %q — run `make scale-test` seeding first\n", cfg.slugPrefix)
		os.Exit(1)
	}
	fmt.Printf("scale-5k: discovered %d targets across %d tenants\n", len(targets), len(targets))

	s := run(ctx, cl, cfg, targets)
	if err := writeSummary(cfg.jsonOut, s); err != nil {
		fmt.Fprintf(os.Stderr, "scale-5k: write summary: %v\n", err)
		os.Exit(1)
	}
	printConsole(s)
	fmt.Printf("scale-5k: JSON summary -> %s (render with: go run ./scripts/loadtest/report.go --in %s)\n", cfg.jsonOut, cfg.jsonOut)
}

// run drives the load and returns the collected summary.
func run(ctx context.Context, cl *client, cfg config, targets []target) summary {
	start := time.Now()
	total := cfg.total()
	nBuckets := int(total/cfg.bucket) + 1
	bucketReq := make([]int64, nBuckets)
	bucketErr := make([]int64, nBuckets)

	picker := newPicker()
	collectors := make([][]opCollector, cfg.workers)

	var wg sync.WaitGroup
	for w := 0; w < cfg.workers; w++ {
		wg.Add(1)
		cols := make([]opCollector, len(workload))
		collectors[w] = cols
		go func(id int, cols []opCollector) {
			defer wg.Done()
			rng := mrand.New(mrand.NewSource(time.Now().UnixNano() + int64(id)*7919))
			tick := 50 * time.Millisecond
			for {
				if ctx.Err() != nil {
					return
				}
				el := time.Since(start)
				if el >= total {
					return
				}
				if id >= targetActive(cfg, el) {
					// Not scheduled at the current concurrency level.
					select {
					case <-ctx.Done():
						return
					case <-time.After(tick):
					}
					continue
				}
				op := picker.pick(rng)
				tgt := targets[rng.Intn(len(targets))]
				reqStart := time.Now()
				err := op.run(ctx, cl, tgt)
				lat := time.Since(reqStart)
				// Requests aborted purely because the run ended are
				// not real errors; don't count them.
				if ctx.Err() != nil && err != nil {
					return
				}
				idx := opIndex(op.name)
				cols[idx].record(lat, err != nil, cfg.maxSamples, rng)

				b := int(time.Since(start) / cfg.bucket)
				if b >= 0 && b < nBuckets {
					atomic.AddInt64(&bucketReq[b], 1)
					if err != nil {
						atomic.AddInt64(&bucketErr[b], 1)
					}
				}
				if cfg.thinkTime > 0 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(cfg.thinkTime):
					}
				}
			}
		}(w, cols)
	}

	// Progress reporter.
	done := make(chan struct{})
	go progressLoop(ctx, cfg, start, bucketReq, done)

	wg.Wait()
	close(done)
	finish := time.Now()

	// Merge per-worker collectors.
	merged := make([]opCollector, len(workload))
	mrng := mrand.New(mrand.NewSource(1))
	for _, cols := range collectors {
		for i := range cols {
			merged[i].merge(&cols[i], cfg.maxSamples, mrng)
		}
	}

	s := summary{Meta: metaFrom(cfg, start, finish, len(targets))}
	var totalN, totalErr int64
	for i, op := range workload {
		st := merged[i].finalize(op.name, op.weight)
		s.Operations = append(s.Operations, st)
		totalN += st.N
		totalErr += st.Errors
	}
	s.Buckets = buildBuckets(cfg, bucketReq, bucketErr)
	dur := finish.Sub(start).Seconds()
	s.Totals = totals{N: totalN, Errors: totalErr}
	if totalN > 0 {
		s.Totals.ErrorRate = round2(100.0 * float64(totalErr) / float64(totalN))
	}
	if dur > 0 {
		s.Totals.RPS = round1(float64(totalN) / dur)
	}
	return s
}

// targetActive returns how many workers should be active at elapsed
// time el: linear ramp 0->W, flat W, linear W->0.
func targetActive(cfg config, el time.Duration) int {
	switch {
	case el < cfg.rampup:
		if cfg.rampup <= 0 {
			return cfg.workers
		}
		n := int(float64(cfg.workers)*float64(el)/float64(cfg.rampup)) + 1
		return clamp(n, 1, cfg.workers)
	case el < cfg.rampup+cfg.steady:
		return cfg.workers
	case el < cfg.total():
		if cfg.cooldown <= 0 {
			return cfg.workers
		}
		remain := cfg.total() - el
		n := int(float64(cfg.workers) * float64(remain) / float64(cfg.cooldown))
		return clamp(n, 1, cfg.workers)
	default:
		return 0
	}
}

func phaseAt(cfg config, el time.Duration) string {
	switch {
	case el < cfg.rampup:
		return "rampup"
	case el < cfg.rampup+cfg.steady:
		return "steady"
	case el < cfg.total():
		return "cooldown"
	default:
		return "done"
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func opIndex(name string) int {
	for i, op := range workload {
		if op.name == name {
			return i
		}
	}
	return 0
}

func progressLoop(ctx context.Context, cfg config, start time.Time, bucketReq []int64, done chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	var last int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-t.C:
			var sum int64
			for i := range bucketReq {
				sum += atomic.LoadInt64(&bucketReq[i])
			}
			el := time.Since(start)
			fmt.Printf("scale-5k: t=%s phase=%s active=%d total_req=%d (+%d)\n",
				el.Round(time.Second), phaseAt(cfg, el), targetActive(cfg, el), sum, sum-last)
			last = sum
		}
	}
}

func buildBuckets(cfg config, req, errs []int64) []bucketStat {
	out := make([]bucketStat, 0, len(req))
	for i := range req {
		r := atomic.LoadInt64(&req[i])
		e := atomic.LoadInt64(&errs[i])
		if r == 0 && e == 0 {
			continue
		}
		startS := float64(i) * cfg.bucket.Seconds()
		out = append(out, bucketStat{
			Index:    i,
			StartS:   round1(startS),
			Phase:    phaseAt(cfg, time.Duration(float64(cfg.bucket)*float64(i))),
			Requests: r,
			Errors:   e,
			RPS:      round1(float64(r) / cfg.bucket.Seconds()),
		})
	}
	return out
}

func metaFrom(cfg config, start, finish time.Time, targets int) meta {
	return meta{
		StartedAt:   start,
		FinishedAt:  finish,
		APIURL:      cfg.apiURL,
		Tenants:     cfg.tenants,
		Targets:     targets,
		Workers:     cfg.workers,
		RampupS:     cfg.rampup.Seconds(),
		SteadyS:     cfg.steady.Seconds(),
		CooldownS:   cfg.cooldown.Seconds(),
		BucketS:     cfg.bucket.Seconds(),
		AttachBytes: cfg.attachmentSize,
		DryRun:      cfg.dryRun,
	}
}

func emptySummary(cfg config, targets int) summary {
	now := time.Now()
	s := summary{Meta: metaFrom(cfg, now, now, targets)}
	s.Meta.DryRun = true
	for _, op := range workload {
		s.Operations = append(s.Operations, opStat{Op: op.name, Weight: op.weight})
	}
	return s
}

func writeSummary(path string, s summary) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func printPlan(cfg config) {
	fmt.Println("scale-5k: scale load test plan")
	fmt.Printf("  target BFF : %s\n", cfg.apiURL)
	fmt.Printf("  tenants    : %d (slug prefix %q)\n", cfg.tenants, cfg.slugPrefix)
	fmt.Printf("  workers    : %d peak\n", cfg.workers)
	fmt.Printf("  phases     : rampup %s -> steady %s -> cooldown %s (total %s)\n",
		cfg.rampup, cfg.steady, cfg.cooldown, cfg.total())
	fmt.Printf("  buckets    : %s\n", cfg.bucket)
	fmt.Printf("  attachment : %d bytes/op\n", cfg.attachmentSize)
	fmt.Println("  workload mix:")
	for _, op := range workload {
		fmt.Printf("    %-18s %3d%%\n", op.name, op.weight)
	}
}

func printConsole(s summary) {
	fmt.Println("------------------------------------------------------------")
	fmt.Printf("scale-5k: run complete — %d requests, %.2f%% errors, %.1f req/s\n",
		s.Totals.N, s.Totals.ErrorRate, s.Totals.RPS)
	fmt.Printf("%-18s %9s %8s %8s %8s %8s %7s\n", "op", "n", "p50ms", "p95ms", "p99ms", "maxms", "err%")
	for _, op := range s.Operations {
		fmt.Printf("%-18s %9d %8.1f %8.1f %8.1f %8.1f %7.2f\n",
			op.Op, op.N, op.P50ms, op.P95ms, op.P99ms, op.MaxMs, op.ErrorRate)
	}
}

// ---------------------------------------------------------------
// Target discovery
// ---------------------------------------------------------------

func discoverTargets(ctx context.Context, cl *client, cfg config) ([]target, error) {
	var tenants []struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	if err := cl.getJSON(ctx, "/api/v1/tenants", "", "", &tenants); err != nil {
		return nil, err
	}
	out := make([]target, 0, cfg.tenants)
	for _, t := range tenants {
		if !strings.HasPrefix(t.Slug, cfg.slugPrefix) {
			continue
		}
		var users []struct {
			KChatUserID       string `json:"kchat_user_id"`
			StalwartAccountID string `json:"stalwart_account_id"`
		}
		if err := cl.getJSON(ctx, "/api/v1/tenants/"+t.ID+"/users", t.ID, "", &users); err != nil {
			if cfg.verbose {
				fmt.Fprintf(os.Stderr, "scale-5k: skip tenant %s: %v\n", t.Slug, err)
			}
			continue
		}
		if len(users) == 0 {
			continue
		}
		out = append(out, target{
			TenantID:    t.ID,
			AccountID:   users[0].StalwartAccountID,
			KChatUserID: users[0].KChatUserID,
		})
		if len(out) >= cfg.tenants {
			break
		}
	}
	return out, nil
}

// ---------------------------------------------------------------
// Operations
// ---------------------------------------------------------------

func opInboxOpen(ctx context.Context, c *client, t target) error {
	return c.jmap(ctx, t, map[string]any{
		"using": jmapMail,
		"methodCalls": [][]any{
			{"Mailbox/get", map[string]any{"accountId": t.AccountID}, "c0"},
		},
	})
}

func opMessageRead(ctx context.Context, c *client, t target) error {
	return c.jmap(ctx, t, map[string]any{
		"using": jmapMail,
		"methodCalls": [][]any{
			{"Email/query", map[string]any{"accountId": t.AccountID, "limit": 20}, "c0"},
			{"Email/get", map[string]any{
				"accountId":  t.AccountID,
				"#ids":       map[string]any{"resultOf": "c0", "name": "Email/query", "path": "/ids"},
				"properties": []string{"id", "subject", "from", "preview", "receivedAt"},
			}, "c1"},
		},
	})
}

func opSearch(ctx context.Context, c *client, t target) error {
	term := searchTerms[mrandShared.Intn(len(searchTerms))]
	return c.jmap(ctx, t, map[string]any{
		"using": jmapMail,
		"methodCalls": [][]any{
			{"Email/query", map[string]any{
				"accountId": t.AccountID,
				"filter":    map[string]any{"text": term},
				"limit":     20,
			}, "c0"},
		},
	})
}

func opSend(ctx context.Context, c *client, t target) error {
	return c.jmap(ctx, t, map[string]any{
		"using": jmapSubmission,
		"methodCalls": [][]any{
			{"Email/set", map[string]any{
				"accountId": t.AccountID,
				"create": map[string]any{
					"draft": map[string]any{
						"mailboxIds": map[string]bool{"drafts": true},
						"keywords":   map[string]bool{"$draft": true},
						"from":       []any{map[string]string{"email": t.AccountID + "@loadtest.kmail.invalid"}},
						"to":         []any{map[string]string{"email": "recipient@loadtest.kmail.invalid"}},
						"subject":    "scale-5k load send",
						"bodyValues": map[string]any{"t": map[string]string{"value": "scale-5k synthetic send body"}},
						"textBody":   []any{map[string]string{"partId": "t", "type": "text/plain"}},
					},
				},
			}, "c0"},
		},
	})
}

func opCalendar(ctx context.Context, c *client, t target) error {
	return c.getJSON(ctx, "/api/v1/calendars/"+t.AccountID, t.TenantID, t.KChatUserID, nil)
}

func opAdminAPI(ctx context.Context, c *client, t target) error {
	return c.getJSON(ctx, "/api/v1/tenants/"+t.TenantID+"/users", t.TenantID, "", nil)
}

func opAttachmentUpload(ctx context.Context, c *client, t target) error {
	size := attachmentSize
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, err := mw.CreateFormFile("file", "scale-5k-blob.bin")
	if err != nil {
		return err
	}
	if _, err := io.CopyN(fw, rand.Reader, size); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/attachments/upload", body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-KMail-Dev-Tenant-Id", t.TenantID)
	req.Header.Set("X-KMail-Dev-Kchat-User-Id", t.KChatUserID)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("upload %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// attachmentSize is set from config before the run starts so the
// op closures (which take no config) can read it.
var attachmentSize int64

// mrandShared is a process-wide RNG for non-latency-critical random
// choices (search terms). Guarded for concurrency.
var mrandShared = newLockedRand()

type lockedRand struct {
	mu sync.Mutex
	r  *mrand.Rand
}

func newLockedRand() *lockedRand {
	return &lockedRand{r: mrand.New(mrand.NewSource(time.Now().UnixNano()))}
}

func (l *lockedRand) Intn(n int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Intn(n)
}

var searchTerms = []string{"invoice", "meeting", "report", "urgent", "loadtest", "project", "update", "review"}

var (
	jmapMail       = []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"}
	jmapSubmission = []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission"}
)

// ---------------------------------------------------------------
// HTTP client
// ---------------------------------------------------------------

type client struct {
	baseURL string
	token   string
	http    *http.Client
}

func (c *client) jmap(ctx context.Context, t target, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/jmap", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-KMail-Dev-Tenant-Id", t.TenantID)
	req.Header.Set("X-KMail-Dev-Kchat-User-Id", t.KChatUserID)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jmap %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *client) getJSON(ctx context.Context, path, tenantID, kchatUserID string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if tenantID != "" {
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
	}
	if kchatUserID != "" {
		req.Header.Set("X-KMail-Dev-Kchat-User-Id", kchatUserID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("GET %s -> %d: %s", path, resp.StatusCode, truncate(string(body), 200))
	}
	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
