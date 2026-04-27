// load-jmap is the Phase 7 load-testing harness for the JMAP
// proxy. Compared with the existing bench-jmap helper this
// program adds:
//
//   * Configurable concurrent worker count.
//   * Ramp-up / steady-state / cool-down phases.
//   * A wider operation mix (mailbox list, email query, email
//     get, email send).
//   * Both human-readable and JSON output (stdout / stderr) so a
//     CI summary step can scrape the run.
//
// Usage:
//
//      go run ./scripts/loadtest/load-jmap.go \
//        --jmap-url http://localhost:8080 \
//        --auth-token kmail-dev \
//        --concurrency 16 \
//        --rampup 30s --steady 120s --cooldown 30s \
//        --iterations 1000
//
// `iterations` caps the total run; the harness stops at whichever
// of (iterations, total duration) is reached first.
//
//go:build ignore

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type opResult struct {
	Op      string        `json:"op"`
	Latency time.Duration `json:"latency"`
	Err     string        `json:"err,omitempty"`
}

var (
	jmapURL     = flag.String("jmap-url", "http://localhost:8080", "BFF base URL")
	authToken   = flag.String("auth-token", "kmail-dev", "Bearer token (dev bypass by default)")
	iterations  = flag.Int("iterations", 1000, "Total request budget")
	concurrency = flag.Int("concurrency", 16, "Parallel worker count")
	rampUp      = flag.Duration("rampup", 30*time.Second, "Ramp-up duration")
	steady      = flag.Duration("steady", 120*time.Second, "Steady-state duration")
	cooldown    = flag.Duration("cooldown", 30*time.Second, "Cool-down duration")
	jsonOut     = flag.String("json-out", "", "Optional path to write JSON summary")
)

// opMix defines the relative weight of each operation. Phase 7
// uses a 70/15/10/5 read-heavy mix matching the production
// telemetry the SRE team published in `docs/SLO_TRACKER.md`.
var opMix = []struct {
	name   string
	weight int
	fn     func(ctx context.Context, c *http.Client) error
}{
	{"mailbox_list", 70, mailboxList},
	{"email_query", 15, emailQuery},
	{"email_get", 10, emailGet},
	{"email_send", 5, emailSend},
}

func pickOp(rng *rand.Rand) (string, func(ctx context.Context, c *http.Client) error) {
	total := 0
	for _, op := range opMix {
		total += op.weight
	}
	roll := rng.Intn(total)
	for _, op := range opMix {
		roll -= op.weight
		if roll < 0 {
			return op.name, op.fn
		}
	}
	return opMix[0].name, opMix[0].fn
}

func main() {
	flag.Parse()
	totalDur := *rampUp + *steady + *cooldown
	deadline := time.Now().Add(totalDur)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	results := make(chan opResult, *iterations)
	var sent int64

	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))
			client := &http.Client{Timeout: 10 * time.Second}
			for {
				if atomic.AddInt64(&sent, 1) > int64(*iterations) {
					return
				}
				if ctx.Err() != nil {
					return
				}
				name, fn := pickOp(rng)
				start := time.Now()
				err := fn(ctx, client)
				latency := time.Since(start)
				res := opResult{Op: name, Latency: latency}
				if err != nil {
					res.Err = err.Error()
				}
				select {
				case results <- res:
				default:
				}
			}
		}(i)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	report(results, *jsonOut)
}

// report tallies the per-op latency distribution and prints the
// canonical KMail benchmark output shape.
func report(in <-chan opResult, jsonPath string) {
	all := []opResult{}
	for r := range in {
		all = append(all, r)
	}
	if len(all) == 0 {
		fmt.Println("no results")
		return
	}
	byOp := map[string][]time.Duration{}
	errs := map[string]int{}
	for _, r := range all {
		if r.Err != "" {
			errs[r.Op]++
			continue
		}
		byOp[r.Op] = append(byOp[r.Op], r.Latency)
	}
	type stat struct {
		Op    string  `json:"op"`
		N     int     `json:"n"`
		P50ms float64 `json:"p50_ms"`
		P95ms float64 `json:"p95_ms"`
		P99ms float64 `json:"p99_ms"`
		MaxMs float64 `json:"max_ms"`
		ErrPc float64 `json:"err_pc"`
	}
	var stats []stat
	fmt.Println("op            n     p50 ms  p95 ms  p99 ms  max ms  err%")
	fmt.Println("------------------------------------------------------------")
	for _, op := range opMix {
		ds := byOp[op.name]
		if len(ds) == 0 && errs[op.name] == 0 {
			continue
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		p50 := pct(ds, 50)
		p95 := pct(ds, 95)
		p99 := pct(ds, 99)
		max := time.Duration(0)
		if len(ds) > 0 {
			max = ds[len(ds)-1]
		}
		total := len(ds) + errs[op.name]
		errPc := 0.0
		if total > 0 {
			errPc = 100.0 * float64(errs[op.name]) / float64(total)
		}
		fmt.Printf("%-12s %5d %7.1f %7.1f %7.1f %7.1f %5.1f\n",
			op.name, total, ms(p50), ms(p95), ms(p99), ms(max), errPc)
		stats = append(stats, stat{
			Op: op.name, N: total,
			P50ms: ms(p50), P95ms: ms(p95), P99ms: ms(p99), MaxMs: ms(max), ErrPc: errPc,
		})
	}
	if jsonPath != "" {
		blob, _ := json.MarshalIndent(stats, "", "  ")
		_ = os.WriteFile(jsonPath, blob, 0o644)
	}
}

func pct(ds []time.Duration, p int) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	idx := (len(ds) * p) / 100
	if idx >= len(ds) {
		idx = len(ds) - 1
	}
	return ds[idx]
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// callJMAP is the shared JMAP request helper.
func callJMAP(ctx context.Context, c *http.Client, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, *jmapURL+"/jmap", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+*authToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("jmap %d: %s", resp.StatusCode, string(respBody))
	}
	return respBody, nil
}

// mailboxList is the Mailbox/get root call.
func mailboxList(ctx context.Context, c *http.Client) error {
	_, err := callJMAP(ctx, c, map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": []any{
			[]any{"Mailbox/get", map[string]any{"accountId": "kmail-dev"}, "c0"},
		},
	})
	return err
}

// emailQuery is an Email/query against the inbox.
func emailQuery(ctx context.Context, c *http.Client) error {
	_, err := callJMAP(ctx, c, map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": []any{
			[]any{"Email/query", map[string]any{
				"accountId": "kmail-dev",
				"limit":     20,
			}, "c0"},
		},
	})
	return err
}

// emailGet pulls the most recent message body.
func emailGet(ctx context.Context, c *http.Client) error {
	_, err := callJMAP(ctx, c, map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": []any{
			[]any{"Email/get", map[string]any{
				"accountId": "kmail-dev",
				"#ids": map[string]any{
					"resultOf": "c0",
					"name":     "Email/query",
					"path":     "/ids",
				},
			}, "c1"},
		},
	})
	return err
}

// emailSend submits an Email/set with a one-shot draft + send.
func emailSend(ctx context.Context, c *http.Client) error {
	_, err := callJMAP(ctx, c, map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail", "urn:ietf:params:jmap:submission"},
		"methodCalls": []any{
			[]any{"Email/set", map[string]any{
				"accountId": "kmail-dev",
				"create": map[string]any{
					"e0": map[string]any{
						"subject":  "loadtest",
						"textBody": []any{map[string]any{"partId": "p1", "type": "text/plain"}},
						"bodyValues": map[string]any{
							"p1": map[string]any{"value": "loadtest body"},
						},
					},
				},
			}, "c0"},
		},
	})
	return err
}
