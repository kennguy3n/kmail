// report renders the JSON summary produced by
// scripts/loadtest/scale-5k.go into a Markdown report containing:
//
//   - run metadata (phases, workers, tenants),
//   - a per-operation latency table (P50/P95/P99/max, error rate),
//   - throughput over time (per bucket),
//   - an SLO compliance table with a PASS/FAIL verdict per op,
//   - an overall pass/fail verdict.
//
// SLO thresholds default to the scale targets documented in
// docs/LOADTEST.md and can be overridden with a JSON file
// (--slo-file) shaped like:
//
//	{"error_budget_pct": 0.5,
//	 "ops": {"inbox_open": 150, "search": 300}}
//
// where the per-op values are P95 latency ceilings in milliseconds.
//
// Usage:
//
//	go run ./scripts/loadtest/report.go \
//	  --in scale-report.json --out scale-report.md
//
// With --out omitted the report is written to stdout. The process
// exits non-zero when the verdict is FAIL (disable with
// --fail-on-violation=false), so it can gate a CI job.
//
//go:build ignore

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------
// JSON summary schema (shared shape with scale-5k.go)
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
// SLOs
// ---------------------------------------------------------------

// slo holds the P95 latency ceilings (ms) per operation plus the
// global error budget. Defaults match docs/LOADTEST.md scale
// targets.
type slo struct {
	ErrorBudgetPct float64            `json:"error_budget_pct"`
	Ops            map[string]float64 `json:"ops"`
}

func defaultSLO() slo {
	return slo{
		ErrorBudgetPct: 0.5,
		Ops: map[string]float64{
			"inbox_open":        150,
			"message_read":      200,
			"search":            300,
			"send":              400,
			"calendar":          250,
			"admin_api":         250,
			"attachment_upload": 2000,
		},
	}
}

// sloOverride mirrors slo but takes the error budget as a pointer so an
// explicit "error_budget_pct": 0 (a valid "zero errors tolerated" policy) is
// distinguishable from the field being omitted — a plain float64 would treat
// both as 0 and silently fall back to the default.
type sloOverride struct {
	ErrorBudgetPct *float64           `json:"error_budget_pct"`
	Ops            map[string]float64 `json:"ops"`
}

func loadSLO(path string) (slo, error) {
	s := defaultSLO()
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	var override sloOverride
	if err := json.Unmarshal(b, &override); err != nil {
		return s, err
	}
	if override.ErrorBudgetPct != nil {
		if *override.ErrorBudgetPct < 0 {
			return s, fmt.Errorf("error_budget_pct must be >= 0, got %g", *override.ErrorBudgetPct)
		}
		s.ErrorBudgetPct = *override.ErrorBudgetPct
	}
	for k, v := range override.Ops {
		s.Ops[k] = v
	}
	return s, nil
}

// ---------------------------------------------------------------
// Main
// ---------------------------------------------------------------

func main() {
	in := flag.String("in", "scale-report.json", "Path to the JSON summary from scale-5k.go")
	out := flag.String("out", "", "Path to write the Markdown report (default: stdout)")
	sloFile := flag.String("slo-file", "", "Optional JSON file overriding SLO thresholds")
	failOnViolation := flag.Bool("fail-on-violation", true, "Exit non-zero when the verdict is FAIL")
	flag.Parse()

	raw, err := os.ReadFile(*in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "report: read %s: %v\n", *in, err)
		os.Exit(2)
	}
	var s summary
	if err := json.Unmarshal(raw, &s); err != nil {
		fmt.Fprintf(os.Stderr, "report: parse %s: %v\n", *in, err)
		os.Exit(2)
	}
	thresholds, err := loadSLO(*sloFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "report: load slo: %v\n", err)
		os.Exit(2)
	}

	md, verdict := render(s, thresholds)

	if *out == "" {
		fmt.Print(md)
	} else {
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "report: write %s: %v\n", *out, err)
			os.Exit(2)
		}
		fmt.Printf("report: wrote %s (verdict: %s)\n", *out, verdict)
	}

	if verdict == verdictFail && *failOnViolation {
		os.Exit(1)
	}
}

const (
	verdictPass   = "PASS"
	verdictFail   = "FAIL"
	verdictNoData = "NO DATA"
)

// render returns the Markdown report and the overall verdict.
func render(s summary, th slo) (string, string) {
	var b strings.Builder
	b.WriteString("# KMail Scale Load-Test Report\n\n")

	// Metadata.
	b.WriteString("## Run metadata\n\n")
	b.WriteString("| Field | Value |\n|---|---|\n")
	if !s.Meta.StartedAt.IsZero() {
		b.WriteString(row("Started", s.Meta.StartedAt.UTC().Format(time.RFC3339)))
		b.WriteString(row("Finished", s.Meta.FinishedAt.UTC().Format(time.RFC3339)))
		dur := s.Meta.FinishedAt.Sub(s.Meta.StartedAt)
		b.WriteString(row("Wall-clock", dur.Round(time.Second).String()))
	}
	b.WriteString(row("BFF URL", s.Meta.APIURL))
	b.WriteString(row("Tenants targeted", fmt.Sprintf("%d (%d resolved)", s.Meta.Tenants, s.Meta.Targets)))
	b.WriteString(row("Peak workers", fmt.Sprintf("%d", s.Meta.Workers)))
	b.WriteString(row("Phases", fmt.Sprintf("ramp %.0fs → steady %.0fs → cooldown %.0fs", s.Meta.RampupS, s.Meta.SteadyS, s.Meta.CooldownS)))
	b.WriteString(row("Attachment size", fmt.Sprintf("%d bytes", s.Meta.AttachBytes)))
	if s.Meta.DryRun {
		b.WriteString(row("Mode", "**DRY RUN** (no traffic generated)"))
	}
	b.WriteString("\n")

	// Totals.
	b.WriteString("## Totals\n\n")
	b.WriteString("| Metric | Value |\n|---|---|\n")
	b.WriteString(row("Requests", fmt.Sprintf("%d", s.Totals.N)))
	b.WriteString(row("Errors", fmt.Sprintf("%d (%.2f%%)", s.Totals.Errors, s.Totals.ErrorRate)))
	b.WriteString(row("Throughput", fmt.Sprintf("%.1f req/s", s.Totals.RPS)))
	b.WriteString("\n")

	// Latency table.
	b.WriteString("## Per-operation latency\n\n")
	b.WriteString("| Operation | Weight | N | Errors | Err % | P50 ms | P95 ms | P99 ms | Max ms | Mean ms |\n")
	b.WriteString("|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, op := range s.Operations {
		b.WriteString(fmt.Sprintf("| %s | %d%% | %d | %d | %.2f | %.1f | %.1f | %.1f | %.1f | %.1f |\n",
			op.Op, op.Weight, op.N, op.Errors, op.ErrorRate, op.P50ms, op.P95ms, op.P99ms, op.MaxMs, op.MeanMs))
	}
	b.WriteString("\n")

	// Throughput over time.
	b.WriteString("## Throughput over time\n\n")
	if len(s.Buckets) == 0 {
		b.WriteString("_No throughput buckets recorded._\n\n")
	} else {
		b.WriteString("| t (s) | Phase | Requests | Errors | RPS |\n|---:|---|---:|---:|---:|\n")
		for _, bk := range s.Buckets {
			b.WriteString(fmt.Sprintf("| %.0f | %s | %d | %d | %.1f |\n",
				bk.StartS, bk.Phase, bk.Requests, bk.Errors, bk.RPS))
		}
		b.WriteString("\n")
	}

	// SLO compliance.
	b.WriteString("## SLO compliance\n\n")
	b.WriteString(fmt.Sprintf("Error budget: **≤ %.2f%%** overall. P95 ceilings per operation:\n\n", th.ErrorBudgetPct))
	b.WriteString("| Operation | P95 ms | Target ms | Verdict |\n|---|---:|---:|:--:|\n")

	verdict := verdictPass
	anyData := s.Totals.N > 0

	// Sort op names for stable output.
	ops := make([]opStat, len(s.Operations))
	copy(ops, s.Operations)
	sort.Slice(ops, func(i, j int) bool { return ops[i].Op < ops[j].Op })

	for _, op := range ops {
		target, ok := th.Ops[op.Op]
		switch {
		case !ok:
			b.WriteString(fmt.Sprintf("| %s | %.1f | _n/a_ | — |\n", op.Op, op.P95ms))
		case op.N == 0:
			b.WriteString(fmt.Sprintf("| %s | — | %.0f | _no data_ |\n", op.Op, target))
		case op.P95ms <= target:
			b.WriteString(fmt.Sprintf("| %s | %.1f | %.0f | ✅ PASS |\n", op.Op, op.P95ms, target))
		default:
			b.WriteString(fmt.Sprintf("| %s | %.1f | %.0f | ❌ FAIL |\n", op.Op, op.P95ms, target))
			verdict = verdictFail
		}
	}
	b.WriteString("\n")

	// Error budget check.
	errBudgetOK := s.Totals.ErrorRate <= th.ErrorBudgetPct
	if anyData && !errBudgetOK {
		verdict = verdictFail
	}
	b.WriteString(fmt.Sprintf("Overall error rate: **%.2f%%** (budget %.2f%%) — %s\n\n",
		s.Totals.ErrorRate, th.ErrorBudgetPct, passFail(errBudgetOK || !anyData)))

	if !anyData {
		verdict = verdictNoData
	}

	b.WriteString("## Verdict\n\n")
	switch verdict {
	case verdictPass:
		b.WriteString("**✅ PASS** — all operations within SLO and error budget.\n")
	case verdictFail:
		b.WriteString("**❌ FAIL** — one or more SLOs breached (see tables above).\n")
	default:
		b.WriteString("**⚠️ NO DATA** — no requests were recorded (dry run or empty target set).\n")
	}

	return b.String(), verdict
}

func row(k, v string) string { return fmt.Sprintf("| %s | %s |\n", k, v) }

func passFail(ok bool) string {
	if ok {
		return "✅ PASS"
	}
	return "❌ FAIL"
}
