// bench-jmap is a standalone benchmark harness for the KMail
// JMAP proxy. It measures P50/P95/P99 latency for three core
// operations — inbox open (Mailbox/get), message open
// (Email/get), and inbox search (Email/query + Email/get back-
// reference) — against the Go BFF.
//
// Usage:
//
//	go run ./scripts/bench/bench-jmap.go \
//	  --jmap-url http://localhost:8080 \
//	  --auth-token kmail-dev \
//	  --iterations 200 \
//	  --warmup 20 \
//	  --concurrency 4
//
// The output is a human-readable table plus a machine-parseable
// JSON blob on stderr so a CI summary step can scrape it.
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
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

type opResult struct {
	Op      string        `json:"op"`
	Latency time.Duration `json:"latency"`
	Err     string        `json:"err,omitempty"`
}

type summary struct {
	Op    string  `json:"op"`
	N     int     `json:"n"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
	Errs  int     `json:"errors"`
}

func main() {
	var (
		jmapURL     = flag.String("jmap-url", "http://localhost:8080", "JMAP proxy URL")
		token       = flag.String("auth-token", "kmail-dev", "Bearer token")
		iter        = flag.Int("iterations", 100, "Measured iterations per op")
		warm        = flag.Int("warmup", 10, "Warm-up iterations (discarded)")
		concurrency = flag.Int("concurrency", 1, "Concurrent goroutines")
		accountID   = flag.String("account-id", "dev", "JMAP accountId for probes")
	)
	flag.Parse()

	ops := []struct {
		name  string
		build func() ([]byte, error)
	}{
		{"mailbox.get", func() ([]byte, error) { return mailboxGet(*accountID) }},
		{"email.query", func() ([]byte, error) { return emailQuery(*accountID) }},
		{"email.get", func() ([]byte, error) { return emailGet(*accountID) }},
	}

	results := make(map[string][]time.Duration)
	errs := make(map[string]int)
	var mu sync.Mutex

	for _, op := range ops {
		// Warm-up: serial, discarded.
		for i := 0; i < *warm; i++ {
			b, _ := op.build()
			_, _ = doJMAP(context.Background(), *jmapURL, *token, b)
		}

		var wg sync.WaitGroup
		work := make(chan struct{}, *iter)
		for i := 0; i < *iter; i++ {
			work <- struct{}{}
		}
		close(work)

		for g := 0; g < *concurrency; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range work {
					b, _ := op.build()
					start := time.Now()
					_, err := doJMAP(context.Background(), *jmapURL, *token, b)
					d := time.Since(start)
					mu.Lock()
					results[op.name] = append(results[op.name], d)
					if err != nil {
						errs[op.name]++
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
	}

	summaries := make([]summary, 0, len(ops))
	fmt.Println("op\t\tN\tP50 ms\tP95 ms\tP99 ms\tmax ms\terrs")
	for _, op := range ops {
		xs := results[op.name]
		sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
		s := summary{
			Op:    op.name,
			N:     len(xs),
			P50Ms: pctMs(xs, 0.50),
			P95Ms: pctMs(xs, 0.95),
			P99Ms: pctMs(xs, 0.99),
			MaxMs: pctMs(xs, 1.0),
			Errs:  errs[op.name],
		}
		summaries = append(summaries, s)
		fmt.Printf("%s\t%d\t%.1f\t%.1f\t%.1f\t%.1f\t%d\n",
			s.Op, s.N, s.P50Ms, s.P95Ms, s.P99Ms, s.MaxMs, s.Errs)
	}
	out, _ := json.Marshal(summaries)
	fmt.Fprintln(os.Stderr, string(out))
}

func pctMs(xs []time.Duration, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	if p >= 1.0 {
		return float64(xs[len(xs)-1].Microseconds()) / 1000
	}
	idx := int(float64(len(xs)) * p)
	if idx >= len(xs) {
		idx = len(xs) - 1
	}
	return float64(xs[idx].Microseconds()) / 1000
}

func doJMAP(ctx context.Context, url, token string, body []byte) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", url+"/jmap", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	c := &http.Client{Timeout: 10 * time.Second}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, b)
	}
	return b, nil
}

func jmapReq(method, accountID string, args map[string]any) ([]byte, error) {
	args["accountId"] = accountID
	payload := map[string]any{
		"using": []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:mail",
		},
		"methodCalls": [][]any{
			{method, args, "c1"},
		},
	}
	return json.Marshal(payload)
}

func mailboxGet(acc string) ([]byte, error) {
	return jmapReq("Mailbox/get", acc, map[string]any{})
}

func emailQuery(acc string) ([]byte, error) {
	return jmapReq("Email/query", acc, map[string]any{
		"filter": map[string]any{"inMailbox": "inbox"},
		"limit":  20,
	})
}

func emailGet(acc string) ([]byte, error) {
	return jmapReq("Email/get", acc, map[string]any{
		"ids":        []string{"1"},
		"properties": []string{"id", "subject", "from", "receivedAt", "preview"},
	})
}
