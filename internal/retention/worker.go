package retention

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// EmailEnforcer is the abstraction over Stalwart JMAP that the
// worker calls to enumerate + destroy / archive emails. The
// production wiring uses `jmapHTTPEnforcer` (defined below); tests
// supply a fake.
type EmailEnforcer interface {
	// QueryOlderThan returns the IDs of emails for `tenantID`
	// older than `before`. `appliesTo` and `targetRef` mirror the
	// retention policy fields so the enforcer can scope to a
	// mailbox / label.
	QueryOlderThan(ctx context.Context, tenantID, appliesTo, targetRef string, before time.Time) ([]string, error)
	// Destroy removes the listed email IDs. Implementations
	// batch internally.
	Destroy(ctx context.Context, tenantID string, ids []string) (int, error)
	// Archive moves the listed email IDs to a cold-storage tier
	// via the zk-object-fabric placement API. Returns how many
	// blobs were moved.
	Archive(ctx context.Context, tenantID string, ids []string) (int, error)
}

// ShardResolver is the subset of `tenant.ShardService` the worker
// needs to talk to a tenant's Stalwart shard. Kept narrow so the
// retention package does not pull the tenant package as a
// dependency.
type ShardResolver interface {
	GetTenantShard(ctx context.Context, tenantID string) (string, error)
}

// Worker ticks daily and evaluates retention for every active
// tenant. Pattern matches `billing.QuotaWorker` /
// `tenant.HealthWorker`.
type Worker struct {
	svc      *Service
	logger   *log.Logger
	interval time.Duration
	enforcer EmailEnforcer
	dryRun   bool
}

// NewWorker constructs a Worker.
func NewWorker(svc *Service, logger *log.Logger) *Worker {
	if logger == nil {
		logger = log.Default()
	}
	return &Worker{svc: svc, logger: logger, interval: 24 * time.Hour, dryRun: true}
}

// WithInterval is a test-only override.
func (w *Worker) WithInterval(d time.Duration) *Worker {
	w.interval = d
	return w
}

// WithEnforcer wires the JMAP / fabric enforcer.
func (w *Worker) WithEnforcer(e EmailEnforcer) *Worker {
	w.enforcer = e
	return w
}

// WithDryRun toggles dry-run mode. Defaults to true so the first
// release does not actually destroy mail.
func (w *Worker) WithDryRun(b bool) *Worker {
	w.dryRun = b
	return w
}

// Run loops until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	if w == nil || w.svc == nil {
		return
	}
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.tick(ctx); err != nil {
				w.logger.Printf("retention.worker: %v", err)
			}
		}
	}
}

func (w *Worker) tick(ctx context.Context) error {
	tenants, err := w.svc.ListActiveTenants(ctx)
	if err != nil {
		return err
	}
	for _, id := range tenants {
		policies, err := w.svc.ListPolicies(ctx, id)
		if err != nil {
			w.logger.Printf("retention.worker: tenant %s list: %v", id, err)
			continue
		}
		for _, p := range policies {
			if !p.Enabled {
				continue
			}
			if err := w.enforcePolicy(ctx, id, p); err != nil {
				w.logger.Printf("retention.worker: tenant %s policy %s: %v", id, p.ID, err)
			}
		}
	}
	return nil
}

func (w *Worker) enforcePolicy(ctx context.Context, tenantID string, p Policy) error {
	logID, err := w.startLog(ctx, tenantID, p.ID, w.dryRun)
	if err != nil {
		w.logger.Printf("retention.worker: startLog: %v", err)
	}
	before := time.Now().AddDate(0, 0, -p.RetentionDays)

	if w.enforcer == nil {
		// No enforcer wired (early dev / tests). Record a
		// placeholder log entry so admins can confirm the worker
		// is alive.
		_ = w.completeLog(ctx, logID, 0, 0, 0, "", noopNote(w.dryRun))
		return nil
	}

	ids, err := w.enforcer.QueryOlderThan(ctx, tenantID, p.AppliesTo, p.TargetRef, before)
	if err != nil {
		_ = w.completeLog(ctx, logID, 0, 0, 0, err.Error(), "")
		return err
	}

	processed := len(ids)
	if w.dryRun {
		_ = w.completeLog(ctx, logID, processed, 0, 0, "", "dry_run=true")
		w.logger.Printf("retention.worker: tenant %s policy %s dry-run matched %d emails", tenantID, p.ID, processed)
		return nil
	}

	deleted, archived := 0, 0
	switch p.PolicyType {
	case "delete":
		deleted, err = w.enforcer.Destroy(ctx, tenantID, ids)
	case "archive":
		archived, err = w.enforcer.Archive(ctx, tenantID, ids)
	default:
		err = fmt.Errorf("retention: unsupported policy_type %q", p.PolicyType)
	}
	if err != nil {
		_ = w.completeLog(ctx, logID, processed, deleted, archived, err.Error(), "")
		return err
	}
	_ = w.completeLog(ctx, logID, processed, deleted, archived, "", "")
	return nil
}

func noopNote(dryRun bool) string {
	if dryRun {
		return "dry_run=true,enforcer=noop"
	}
	return "enforcer=noop"
}

func (w *Worker) startLog(ctx context.Context, tenantID, policyID string, dryRun bool) (string, error) {
	if w.svc.pool == nil {
		return "", nil
	}
	var id string
	err := pgx.BeginFunc(ctx, w.svc.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		notes := ""
		if dryRun {
			notes = "dry_run=true"
		}
		return tx.QueryRow(ctx, `
			INSERT INTO retention_enforcement_log (tenant_id, policy_id, notes)
			VALUES ($1::uuid, $2::uuid, $3)
			RETURNING id::text
		`, tenantID, policyID, notes).Scan(&id)
	})
	return id, err
}

func (w *Worker) completeLog(ctx context.Context, logID string, processed, deleted, archived int, errMsg, notes string) error {
	if w.svc.pool == nil || logID == "" {
		return nil
	}
	_, err := w.svc.pool.Exec(ctx, `
		UPDATE retention_enforcement_log
		SET emails_processed = $2, emails_deleted = $3, emails_archived = $4,
		    completed_at = now(), error = COALESCE($5, ''), notes = COALESCE(NULLIF($6, ''), notes)
		WHERE id = $1::uuid
	`, logID, processed, deleted, archived, errMsg, notes)
	return err
}

// ---------------------------------------------------------------
// JMAP-backed enforcer
// ---------------------------------------------------------------

// JMAPEnforcer is the production EmailEnforcer. It speaks JMAP to
// the tenant's Stalwart shard for delete operations; archive ops
// post to the zk-object-fabric placement API to flip the storage
// tier of matched blobs.
type JMAPEnforcer struct {
	Shards     ShardResolver
	HTTP       *http.Client
	Auth       string
	FabricURL  string
	FabricAuth string
	Logger     *log.Logger
}

// NewJMAPEnforcer returns a JMAPEnforcer with sensible defaults.
func NewJMAPEnforcer(shards ShardResolver, httpClient *http.Client, auth, fabricURL, fabricAuth string, logger *log.Logger) *JMAPEnforcer {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if logger == nil {
		logger = log.Default()
	}
	return &JMAPEnforcer{
		Shards:     shards,
		HTTP:       httpClient,
		Auth:       auth,
		FabricURL:  fabricURL,
		FabricAuth: fabricAuth,
		Logger:     logger,
	}
}

// QueryOlderThan asks Stalwart for email IDs older than `before`.
func (e *JMAPEnforcer) QueryOlderThan(ctx context.Context, tenantID, appliesTo, targetRef string, before time.Time) ([]string, error) {
	url, err := e.shardURL(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	filter := map[string]any{
		"before": before.UTC().Format(time.RFC3339),
	}
	if appliesTo == "mailbox" && targetRef != "" {
		filter["inMailbox"] = targetRef
	}
	body := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": [][]any{
			{"Email/query", map[string]any{
				"accountId": tenantID,
				"filter":    filter,
				"limit":     1000,
			}, "c1"},
		},
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := e.jmap(ctx, url, body, &resp); err != nil {
		return nil, err
	}
	if len(resp.MethodResponses) == 0 || len(resp.MethodResponses[0]) < 2 {
		return nil, nil
	}
	var args struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(resp.MethodResponses[0][1], &args); err != nil {
		return nil, err
	}
	return args.IDs, nil
}

// Destroy issues `Email/set` with `destroy` in batches of 100.
func (e *JMAPEnforcer) Destroy(ctx context.Context, tenantID string, ids []string) (int, error) {
	url, err := e.shardURL(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	count := 0
	for i := 0; i < len(ids); i += 100 {
		end := i + 100
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		body := map[string]any{
			"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
			"methodCalls": [][]any{
				{"Email/set", map[string]any{
					"accountId": tenantID,
					"destroy":   batch,
				}, "c1"},
			},
		}
		var resp struct {
			MethodResponses [][]json.RawMessage `json:"methodResponses"`
		}
		if err := e.jmap(ctx, url, body, &resp); err != nil {
			return count, err
		}
		count += len(batch)
	}
	return count, nil
}

// Archive flips the storage tier of matching blobs to cold via the
// zk-object-fabric placement API.
func (e *JMAPEnforcer) Archive(ctx context.Context, tenantID string, ids []string) (int, error) {
	if e.FabricURL == "" {
		return 0, errors.New("retention: fabric url not configured")
	}
	count := 0
	for i := 0; i < len(ids); i += 100 {
		end := i + 100
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		body := map[string]any{
			"tenant_id":    tenantID,
			"object_ids":   batch,
			"target_tier":  "cold",
			"reason":       "retention_archive",
		}
		buf, _ := json.Marshal(body)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			strings.TrimRight(e.FabricURL, "/")+"/placements/move", bytes.NewReader(buf))
		if err != nil {
			return count, err
		}
		req.Header.Set("Content-Type", "application/json")
		if e.FabricAuth != "" {
			req.Header.Set("Authorization", e.FabricAuth)
		}
		resp, err := e.HTTP.Do(req)
		if err != nil {
			return count, err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			return count, fmt.Errorf("retention: fabric placement HTTP %d", resp.StatusCode)
		}
		count += len(batch)
	}
	return count, nil
}

func (e *JMAPEnforcer) shardURL(ctx context.Context, tenantID string) (string, error) {
	if e.Shards == nil {
		return "", errors.New("retention: shards not configured")
	}
	url, err := e.Shards.GetTenantShard(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(url, "/") + "/jmap/api", nil
}

func (e *JMAPEnforcer) jmap(ctx context.Context, url string, payload any, out any) error {
	buf, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if e.Auth != "" {
		req.Header.Set("Authorization", e.Auth)
	}
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("retention: jmap HTTP %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return json.Unmarshal(raw, out)
}
