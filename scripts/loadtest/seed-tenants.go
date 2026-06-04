// seed-tenants provisions a fleet of synthetic tenants for the
// scale load-test harness (see scripts/loadtest/scale-5k.go and
// docs/LOADTEST.md). It drives the public BFF admin API and the
// JMAP proxy exactly the way a real operator / client would, so a
// seeded environment is indistinguishable from an organically
// grown one.
//
// For each tenant it provisions (all counts configurable):
//
//   - 1 tenant                      POST /api/v1/tenants
//   - N domains          (default 3) POST /api/v1/tenants/:id/domains
//   - N users           (default 20) POST /api/v1/tenants/:id/users
//   - N shared inboxes   (default 2) POST /api/v1/tenants/:id/shared-inboxes
//   - N retention policies (default 1) POST /api/v1/tenants/:id/retention
//   - N webhooks         (default 1) POST /api/v1/tenants/:id/webhooks
//   - N messages     (default 10000) JMAP Email/set into user[0]'s inbox
//
// The seeder is **idempotent**: every entity is reconciled against
// the current server state (list-then-create keyed on a natural
// key — slug, domain, email, address, URL — and a message-count
// delta) so a re-run over an already-seeded environment only fills
// the gap rather than duplicating rows. It is **parallel**: tenants
// are reconciled by a bounded goroutine pool (--concurrency) and
// messages are submitted by a per-tenant pool (--msg-concurrency)
// in batched JMAP Email/set calls.
//
// Usage:
//
//	go run ./scripts/loadtest/seed-tenants.go \
//	  --api-url http://localhost:8088 \
//	  --auth-token kmail-dev \
//	  --tenants 100 --users 20 --domains 3 \
//	  --messages 10000 --shared-inboxes 2 \
//	  --retention 1 --webhooks 1 \
//	  --concurrency 16
//
// --dry-run prints the provisioning plan and exits without making
// any network calls, which is what `make scale-test` exercises in
// its self-check path.
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
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// config is the fully-resolved seeder configuration.
type config struct {
	apiURL         string
	authToken      string
	tenants        int
	users          int
	domains        int
	messages       int
	sharedInboxes  int
	retention      int
	webhooks       int
	concurrency    int
	msgConcurrency int
	msgBatch       int
	slugPrefix     string
	domainSuffix   string
	webhookBase    string
	httpTimeout    time.Duration
	dryRun         bool
	verbose        bool
}

func parseConfig() config {
	var c config
	flag.StringVar(&c.apiURL, "api-url", envOr("KMAIL_API_URL", "http://localhost:8088"), "BFF base URL")
	flag.StringVar(&c.authToken, "auth-token", envOr("KMAIL_DEV_BEARER", "kmail-dev"), "Bearer token (dev bypass by default)")
	flag.IntVar(&c.tenants, "tenants", 100, "Number of tenants to provision (1..5000)")
	flag.IntVar(&c.users, "users", 20, "Users per tenant")
	flag.IntVar(&c.domains, "domains", 3, "Domains per tenant")
	flag.IntVar(&c.messages, "messages", 10000, "Messages per tenant (seeded into user[0]'s inbox)")
	flag.IntVar(&c.sharedInboxes, "shared-inboxes", 2, "Shared inboxes per tenant")
	flag.IntVar(&c.retention, "retention", 1, "Retention policies per tenant")
	flag.IntVar(&c.webhooks, "webhooks", 1, "Webhook endpoints per tenant")
	flag.IntVar(&c.concurrency, "concurrency", 16, "Parallel tenant reconciliation workers")
	flag.IntVar(&c.msgConcurrency, "msg-concurrency", 8, "Parallel message-seeding workers per tenant")
	flag.IntVar(&c.msgBatch, "msg-batch", 100, "Email/set creates per JMAP request")
	flag.StringVar(&c.slugPrefix, "slug-prefix", "loadtest", "Tenant slug prefix (slug = <prefix>-NNNN)")
	flag.StringVar(&c.domainSuffix, "domain-suffix", "loadtest.kmail.invalid", "Domain suffix (domain = t<NNNN>-dM.<suffix>)")
	flag.StringVar(&c.webhookBase, "webhook-base", "https://webhook.loadtest.kmail.invalid", "Base URL for seeded webhook endpoints")
	flag.DurationVar(&c.httpTimeout, "http-timeout", 30*time.Second, "Per-request HTTP timeout")
	flag.BoolVar(&c.dryRun, "dry-run", false, "Print the provisioning plan and exit without any network calls")
	flag.BoolVar(&c.verbose, "verbose", false, "Log every reconciliation decision")
	flag.Parse()
	return c
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// stats is the seeder's atomic result tally.
type stats struct {
	tenantsCreated int64
	tenantsReused  int64
	domains        int64
	users          int64
	sharedInboxes  int64
	retention      int64
	webhooks       int64
	messages       int64
	tenantErrors   int64
}

func main() {
	cfg := parseConfig()
	if err := validate(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "seed-tenants: %v\n", err)
		os.Exit(2)
	}

	if cfg.dryRun {
		printPlan(cfg)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cl := &apiClient{
		baseURL: strings.TrimRight(cfg.apiURL, "/"),
		token:   cfg.authToken,
		http:    &http.Client{Timeout: cfg.httpTimeout},
	}

	// Fail fast if the BFF is unreachable: a clear up-front error
	// beats thousands of per-tenant connection failures.
	if err := cl.ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "seed-tenants: BFF not reachable at %s: %v\n", cfg.apiURL, err)
		os.Exit(1)
	}

	var st stats
	start := time.Now()

	// Bounded worker pool over tenant indices.
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < cfg.concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := seedTenant(ctx, cl, cfg, idx, &st); err != nil {
					atomic.AddInt64(&st.tenantErrors, 1)
					fmt.Fprintf(os.Stderr, "seed-tenants: tenant %d failed: %v\n", idx, err)
				}
			}
		}()
	}

	// progressDone stops the progress reporter as soon as the run finishes,
	// rather than relying on process exit to reap the goroutine.
	progressDone := make(chan struct{})
	go func() {
		var last int64
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-progressDone:
				return
			case <-t.C:
				cur := atomic.LoadInt64(&st.tenantsCreated) + atomic.LoadInt64(&st.tenantsReused)
				if cur != last {
					last = cur
					fmt.Printf("seed-tenants: %d/%d tenants reconciled (%d msgs)\n",
						cur, cfg.tenants, atomic.LoadInt64(&st.messages))
				}
			}
		}
	}()

loop:
	for i := 0; i < cfg.tenants; i++ {
		select {
		case <-ctx.Done():
			break loop
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	close(progressDone)

	printSummary(cfg, &st, time.Since(start))
	if atomic.LoadInt64(&st.tenantErrors) > 0 {
		os.Exit(1)
	}
}

func validate(c config) error {
	if c.tenants < 1 || c.tenants > 5000 {
		return fmt.Errorf("--tenants must be in 1..5000, got %d", c.tenants)
	}
	if c.users < 1 {
		return fmt.Errorf("--users must be >= 1, got %d", c.users)
	}
	if c.domains < 0 || c.messages < 0 || c.sharedInboxes < 0 || c.retention < 0 || c.webhooks < 0 {
		return fmt.Errorf("entity counts must be non-negative")
	}
	if c.concurrency < 1 {
		return fmt.Errorf("--concurrency must be >= 1, got %d", c.concurrency)
	}
	if c.msgConcurrency < 1 {
		return fmt.Errorf("--msg-concurrency must be >= 1, got %d", c.msgConcurrency)
	}
	if c.msgBatch < 1 {
		return fmt.Errorf("--msg-batch must be >= 1, got %d", c.msgBatch)
	}
	return nil
}

func printPlan(c config) {
	fmt.Println("seed-tenants: DRY RUN — no network calls will be made")
	fmt.Printf("  target BFF        : %s\n", c.apiURL)
	fmt.Printf("  tenants           : %d (slug %s-0000 .. %s-%04d)\n", c.tenants, c.slugPrefix, c.slugPrefix, c.tenants-1)
	fmt.Printf("  per tenant        : %d users, %d domains, %d shared inboxes, %d retention, %d webhooks\n",
		c.users, c.domains, c.sharedInboxes, c.retention, c.webhooks)
	fmt.Printf("  messages/tenant   : %d (batched %d/request, %d msg workers)\n", c.messages, c.msgBatch, c.msgConcurrency)
	fmt.Printf("  tenant workers    : %d\n", c.concurrency)
	fmt.Println("  reconciliation    : idempotent (list-then-create on natural key + message-count delta)")
	fmt.Println()
	fmt.Printf("  total to provision (worst case, empty environment):\n")
	fmt.Printf("    %d tenants, %d users, %d domains, %d shared inboxes, %d retention policies, %d webhooks, %d messages\n",
		c.tenants, c.tenants*c.users, c.tenants*c.domains, c.tenants*c.sharedInboxes,
		c.tenants*c.retention, c.tenants*c.webhooks, c.tenants*c.messages)
}

func printSummary(c config, st *stats, elapsed time.Duration) {
	fmt.Println("------------------------------------------------------------")
	fmt.Printf("seed-tenants: done in %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("  tenants  : %d created, %d reused (%d errors)\n",
		atomic.LoadInt64(&st.tenantsCreated), atomic.LoadInt64(&st.tenantsReused), atomic.LoadInt64(&st.tenantErrors))
	fmt.Printf("  users    : %d created\n", atomic.LoadInt64(&st.users))
	fmt.Printf("  domains  : %d created\n", atomic.LoadInt64(&st.domains))
	fmt.Printf("  shared   : %d created\n", atomic.LoadInt64(&st.sharedInboxes))
	fmt.Printf("  retention: %d created\n", atomic.LoadInt64(&st.retention))
	fmt.Printf("  webhooks : %d created\n", atomic.LoadInt64(&st.webhooks))
	fmt.Printf("  messages : %d created\n", atomic.LoadInt64(&st.messages))
}

// seedTenant reconciles a single tenant and all of its child
// entities. It is safe to run concurrently with other tenants and
// idempotent across re-runs.
func seedTenant(ctx context.Context, cl *apiClient, c config, idx int, st *stats) error {
	slug := fmt.Sprintf("%s-%04d", c.slugPrefix, idx)
	name := fmt.Sprintf("Loadtest Tenant %04d", idx)

	t, created, err := cl.ensureTenant(ctx, name, slug)
	if err != nil {
		return fmt.Errorf("ensure tenant: %w", err)
	}
	if created {
		atomic.AddInt64(&st.tenantsCreated, 1)
	} else {
		atomic.AddInt64(&st.tenantsReused, 1)
	}

	// Domains.
	existingDomains, err := cl.listDomains(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("list domains: %w", err)
	}
	for d := 0; d < c.domains; d++ {
		dom := fmt.Sprintf("t%04d-d%d.%s", idx, d, c.domainSuffix)
		if existingDomains[dom] {
			continue
		}
		if err := cl.createDomain(ctx, t.ID, dom); err != nil {
			return fmt.Errorf("create domain %s: %w", dom, err)
		}
		atomic.AddInt64(&st.domains, 1)
	}

	// Users. user[0] is the mailbox we seed messages into.
	existingUsers, err := cl.listUsers(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	var primary user
	for u := 0; u < c.users; u++ {
		kchatID := fmt.Sprintf("%s-u%03d", slug, u)
		if got, ok := existingUsers[kchatID]; ok {
			if u == 0 {
				primary = got
			}
			continue
		}
		acct := fmt.Sprintf("%s-acct-%03d", slug, u)
		email := fmt.Sprintf("user%03d@t%04d-d0.%s", u, idx, c.domainSuffix)
		nu, err := cl.createUser(ctx, t.ID, createUserBody{
			KChatUserID:       kchatID,
			StalwartAccountID: acct,
			Email:             email,
			DisplayName:       fmt.Sprintf("Loadtest User %03d", u),
			Role:              roleFor(u),
			QuotaBytes:        2 << 30,
		})
		if err != nil {
			return fmt.Errorf("create user %s: %w", kchatID, err)
		}
		atomic.AddInt64(&st.users, 1)
		if u == 0 {
			primary = nu
		}
	}
	if primary.ID == "" {
		// Tenant had >=1 user already; reuse user[0].
		primary = existingUsers[fmt.Sprintf("%s-u%03d", slug, 0)]
	}

	// Shared inboxes.
	existingShared, err := cl.listSharedInboxes(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("list shared inboxes: %w", err)
	}
	for s := 0; s < c.sharedInboxes; s++ {
		addr := fmt.Sprintf("team%d@t%04d-d0.%s", s, idx, c.domainSuffix)
		if existingShared[addr] {
			continue
		}
		if err := cl.createSharedInbox(ctx, t.ID, addr, fmt.Sprintf("Shared Inbox %d", s)); err != nil {
			return fmt.Errorf("create shared inbox %s: %w", addr, err)
		}
		atomic.AddInt64(&st.sharedInboxes, 1)
	}

	// Retention policies. There is no natural key, so we reconcile
	// purely on count: only create up to the configured number.
	curRetention, err := cl.countRetention(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("list retention: %w", err)
	}
	for p := curRetention; p < c.retention; p++ {
		if err := cl.createRetention(ctx, t.ID); err != nil {
			return fmt.Errorf("create retention: %w", err)
		}
		atomic.AddInt64(&st.retention, 1)
	}

	// Webhooks (natural key: URL).
	existingHooks, err := cl.listWebhooks(ctx, t.ID)
	if err != nil {
		return fmt.Errorf("list webhooks: %w", err)
	}
	for h := 0; h < c.webhooks; h++ {
		url := fmt.Sprintf("%s/%s/hook%d", strings.TrimRight(c.webhookBase, "/"), slug, h)
		if existingHooks[url] {
			continue
		}
		if err := cl.createWebhook(ctx, t.ID, url); err != nil {
			return fmt.Errorf("create webhook %s: %w", url, err)
		}
		atomic.AddInt64(&st.webhooks, 1)
	}

	// Messages — only fill the gap to the configured count.
	if c.messages > 0 && primary.KChatUserID != "" {
		n, err := cl.seedMessages(ctx, c, t.ID, primary)
		if err != nil {
			return fmt.Errorf("seed messages: %w", err)
		}
		atomic.AddInt64(&st.messages, int64(n))
	}

	if c.verbose {
		fmt.Printf("seed-tenants: tenant %s (%s) reconciled\n", slug, t.ID)
	}
	return nil
}

// roleFor makes user[0] the owner and everyone else a member, so a
// seeded tenant has exactly one admin seat like a real org.
func roleFor(u int) string {
	if u == 0 {
		return "owner"
	}
	return "member"
}

// ---------------------------------------------------------------
// API client
// ---------------------------------------------------------------

type apiClient struct {
	baseURL string
	token   string
	http    *http.Client
}

type tenant struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

type user struct {
	ID                string `json:"id"`
	KChatUserID       string `json:"kchat_user_id"`
	StalwartAccountID string `json:"stalwart_account_id"`
}

type createUserBody struct {
	KChatUserID       string `json:"kchat_user_id"`
	StalwartAccountID string `json:"stalwart_account_id"`
	Email             string `json:"email"`
	DisplayName       string `json:"display_name"`
	Role              string `json:"role"`
	QuotaBytes        int64  `json:"quota_bytes"`
}

func (c *apiClient) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/readyz", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode >= 500 {
		return fmt.Errorf("readyz returned %d", resp.StatusCode)
	}
	return nil
}

// do issues a JSON request. tenantID, when non-empty, is sent as
// the dev-bypass tenant scope header so tenant-scoped admin routes
// accept the call. kchatUserID overrides the dev user identity
// (needed when seeding messages as a specific mailbox owner).
func (c *apiClient) do(ctx context.Context, method, path, tenantID, kchatUserID string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
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
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s -> %d: %s", method, path, resp.StatusCode, truncate(string(respBody), 300))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("decode %s %s: %w", method, path, err)
		}
	}
	return nil
}

func (c *apiClient) ensureTenant(ctx context.Context, name, slug string) (tenant, bool, error) {
	var existing []tenant
	if err := c.do(ctx, http.MethodGet, "/api/v1/tenants", "", "", nil, &existing); err != nil {
		return tenant{}, false, err
	}
	for _, t := range existing {
		if t.Slug == slug {
			return t, false, nil
		}
	}
	var created tenant
	body := map[string]string{"name": name, "slug": slug, "plan": "pro"}
	if err := c.do(ctx, http.MethodPost, "/api/v1/tenants", "", "", body, &created); err != nil {
		return tenant{}, false, err
	}
	return created, true, nil
}

func (c *apiClient) listDomains(ctx context.Context, tenantID string) (map[string]bool, error) {
	var out []struct {
		Domain string `json:"domain"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/tenants/"+tenantID+"/domains", tenantID, "", nil, &out); err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(out))
	for _, d := range out {
		m[d.Domain] = true
	}
	return m, nil
}

func (c *apiClient) createDomain(ctx context.Context, tenantID, domain string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/tenants/"+tenantID+"/domains", tenantID, "",
		map[string]string{"domain": domain}, nil)
}

func (c *apiClient) listUsers(ctx context.Context, tenantID string) (map[string]user, error) {
	var out []user
	if err := c.do(ctx, http.MethodGet, "/api/v1/tenants/"+tenantID+"/users", tenantID, "", nil, &out); err != nil {
		return nil, err
	}
	m := make(map[string]user, len(out))
	for _, u := range out {
		m[u.KChatUserID] = u
	}
	return m, nil
}

func (c *apiClient) createUser(ctx context.Context, tenantID string, body createUserBody) (user, error) {
	var out user
	if err := c.do(ctx, http.MethodPost, "/api/v1/tenants/"+tenantID+"/users", tenantID, "", body, &out); err != nil {
		return user{}, err
	}
	return out, nil
}

func (c *apiClient) listSharedInboxes(ctx context.Context, tenantID string) (map[string]bool, error) {
	var out []struct {
		Address string `json:"address"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/tenants/"+tenantID+"/shared-inboxes", tenantID, "", nil, &out); err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(out))
	for _, s := range out {
		m[s.Address] = true
	}
	return m, nil
}

func (c *apiClient) createSharedInbox(ctx context.Context, tenantID, address, displayName string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/tenants/"+tenantID+"/shared-inboxes", tenantID, "",
		map[string]string{"address": address, "display_name": displayName}, nil)
}

func (c *apiClient) countRetention(ctx context.Context, tenantID string) (int, error) {
	var out []json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/api/v1/tenants/"+tenantID+"/retention", tenantID, "", nil, &out); err != nil {
		return 0, err
	}
	return len(out), nil
}

func (c *apiClient) createRetention(ctx context.Context, tenantID string) error {
	body := map[string]any{
		"policy_type":    "archive",
		"retention_days": 365,
		"applies_to":     "all",
		"enabled":        true,
	}
	return c.do(ctx, http.MethodPost, "/api/v1/tenants/"+tenantID+"/retention", tenantID, "", body, nil)
}

func (c *apiClient) listWebhooks(ctx context.Context, tenantID string) (map[string]bool, error) {
	var out []struct {
		URL string `json:"url"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/tenants/"+tenantID+"/webhooks", tenantID, "", nil, &out); err != nil {
		return nil, err
	}
	m := make(map[string]bool, len(out))
	for _, h := range out {
		m[h.URL] = true
	}
	return m, nil
}

func (c *apiClient) createWebhook(ctx context.Context, tenantID, url string) error {
	body := map[string]any{
		"url":             url,
		"events":          []string{"email.received", "email.sent"},
		"signing_version": "v1",
	}
	return c.do(ctx, http.MethodPost, "/api/v1/tenants/"+tenantID+"/webhooks", tenantID, "", body, nil)
}

// ---------------------------------------------------------------
// Message seeding (JMAP)
// ---------------------------------------------------------------

// seedMessages fills the primary user's inbox up to cfg.messages,
// computing the delta from the current count so re-runs are cheap.
// Creates are batched (cfg.msgBatch per request) and submitted by a
// bounded pool (cfg.msgConcurrency).
func (c *apiClient) seedMessages(ctx context.Context, cfg config, tenantID string, u user) (int, error) {
	have, err := c.messageCount(ctx, tenantID, u)
	if err != nil {
		return 0, err
	}
	need := cfg.messages - have
	if need <= 0 {
		return 0, nil
	}

	batches := make(chan [2]int) // [startIndex, count]
	var (
		wg       sync.WaitGroup
		created  int64
		firstErr error
		errOnce  sync.Once
	)
	for w := 0; w < cfg.msgConcurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range batches {
				if ctx.Err() != nil {
					return
				}
				n, err := c.sendBatch(ctx, tenantID, u, b[0], b[1])
				if err != nil {
					errOnce.Do(func() { firstErr = err })
					return
				}
				atomic.AddInt64(&created, int64(n))
			}
		}()
	}

	go func() {
		defer close(batches)
		for off := have; off < cfg.messages; off += cfg.msgBatch {
			n := cfg.msgBatch
			if off+n > cfg.messages {
				n = cfg.messages - off
			}
			select {
			case <-ctx.Done():
				return
			case batches <- [2]int{off, n}:
			}
		}
	}()

	wg.Wait()
	return int(atomic.LoadInt64(&created)), firstErr
}

// messageCount returns the total messages in the user's inbox via
// Email/query with calculateTotal.
func (c *apiClient) messageCount(ctx context.Context, tenantID string, u user) (int, error) {
	payload := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": [][]any{
			{"Email/query", map[string]any{
				"accountId":      u.StalwartAccountID,
				"calculateTotal": true,
				"limit":          0,
			}, "c0"},
		},
	}
	raw, err := c.jmap(ctx, tenantID, u.KChatUserID, payload)
	if err != nil {
		return 0, err
	}
	total, ok := jmapQueryTotal(raw)
	if !ok {
		// A server that doesn't report a total just means we can't
		// dedupe; treat as empty so the gap fill proceeds.
		return 0, nil
	}
	return total, nil
}

// sendBatch creates `n` messages in a single Email/set call,
// returning how many the server reported as created.
func (c *apiClient) sendBatch(ctx context.Context, tenantID string, u user, start, n int) (int, error) {
	creates := make(map[string]any, n)
	now := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < n; i++ {
		seq := start + i
		id := fmt.Sprintf("seed-%06d", seq)
		creates[id] = map[string]any{
			"mailboxIds": map[string]bool{"inbox": true},
			"keywords":   map[string]bool{"$seen": true},
			"from":       []any{map[string]string{"email": "seed@loadtest.kmail.invalid", "name": "Loadtest Seeder"}},
			"to":         []any{map[string]string{"email": u.StalwartAccountID + "@loadtest.kmail.invalid"}},
			"subject":    fmt.Sprintf("Loadtest seed message %06d", seq),
			"receivedAt": now,
			"bodyValues": map[string]any{"text": map[string]string{"value": fmt.Sprintf("Synthetic load-test message #%06d for %s.", seq, u.KChatUserID)}},
			"textBody":   []any{map[string]string{"partId": "text", "type": "text/plain"}},
		}
	}
	payload := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": [][]any{
			{"Email/set", map[string]any{"accountId": u.StalwartAccountID, "create": creates}, "c0"},
		},
	}
	raw, err := c.jmap(ctx, tenantID, u.KChatUserID, payload)
	if err != nil {
		return 0, err
	}
	return jmapCreatedCount(raw, n), nil
}

func (c *apiClient) jmap(ctx context.Context, tenantID, kchatUserID string, payload any) (json.RawMessage, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/jmap", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if tenantID != "" {
		req.Header.Set("X-KMail-Dev-Tenant-Id", tenantID)
	}
	if kchatUserID != "" {
		req.Header.Set("X-KMail-Dev-Kchat-User-Id", kchatUserID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("jmap %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	return body, nil
}

// jmapEnvelope is the minimal shape of a JMAP response we parse.
type jmapEnvelope struct {
	MethodResponses [][]json.RawMessage `json:"methodResponses"`
}

// jmapQueryTotal extracts the `total` field from the first
// Email/query response.
func jmapQueryTotal(raw json.RawMessage) (int, bool) {
	args, ok := firstResponseArgs(raw)
	if !ok {
		return 0, false
	}
	var r struct {
		Total *int `json:"total"`
	}
	if err := json.Unmarshal(args, &r); err != nil || r.Total == nil {
		return 0, false
	}
	return *r.Total, true
}

// jmapCreatedCount counts the entries in the Email/set `created`
// map, falling back to the requested count when the server omits
// it (older Stalwart builds).
func jmapCreatedCount(raw json.RawMessage, requested int) int {
	args, ok := firstResponseArgs(raw)
	if !ok {
		return 0
	}
	var r struct {
		Created    map[string]json.RawMessage `json:"created"`
		NotCreated map[string]json.RawMessage `json:"notCreated"`
	}
	if err := json.Unmarshal(args, &r); err != nil {
		return 0
	}
	if r.Created == nil && r.NotCreated == nil {
		return requested
	}
	return len(r.Created)
}

func firstResponseArgs(raw json.RawMessage) (json.RawMessage, bool) {
	var env jmapEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, false
	}
	if len(env.MethodResponses) == 0 || len(env.MethodResponses[0]) < 2 {
		return nil, false
	}
	return env.MethodResponses[0][1], true
}

// ---------------------------------------------------------------
// helpers
// ---------------------------------------------------------------

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
