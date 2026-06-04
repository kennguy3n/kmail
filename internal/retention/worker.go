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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// Metrics is the Prometheus metric set for the retention worker.
// Exposed so callers can register the collectors with the same
// registry the BFF exposes on `/metrics`.
type Metrics struct {
	Evaluations    prometheus.Counter
	EmailsDeleted  prometheus.Counter
	EmailsArchived prometheus.Counter
	Errors         prometheus.Counter
}

// NewMetrics builds a Metrics set and registers it with `reg`.
// Pass `nil` to skip registration (tests).
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Evaluations: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_retention_evaluations_total",
			Help: "Number of retention policy evaluations performed by the worker.",
		}),
		EmailsDeleted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_retention_emails_deleted_total",
			Help: "Total emails destroyed by retention policies (live mode only).",
		}),
		EmailsArchived: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_retention_emails_archived_total",
			Help: "Total emails archived by retention policies (live mode only).",
		}),
		Errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_retention_errors_total",
			Help: "Errors raised by the retention worker (any phase).",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.Evaluations, m.EmailsDeleted, m.EmailsArchived, m.Errors)
	}
	return m
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
	op       jmap.EmailOperator
	dryRun   bool
	metrics  *Metrics

	// engine is the shared Enforcer, built lazily from the wired
	// options on the first tick and registered on the Service so
	// the worker loop and Service.EvaluateRetention drive the same
	// instance.
	engineOnce sync.Once
	engine     *Enforcer

	// Last enforcement snapshot for the admin UI status card.
	// Read-only outside the worker; updated atomically each tick.
	lastEvaluatedAt   atomic.Int64
	lastDeletedTotal  atomic.Int64
	lastArchivedTotal atomic.Int64
	lastErrorsTotal   atomic.Int64
}

// NewWorker constructs a Worker. Defaults to dry-run; production
// callers flip `WithDryRun(false)` to enforce.
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

// WithEnforcer wires the email operator the enforcement engine
// drives. Production passes a *JMAPEnforcer (which also implements
// ColdMover for archive policies); tests supply a fake operator.
func (w *Worker) WithEnforcer(op jmap.EmailOperator) *Worker {
	w.op = op
	return w
}

// WithDryRun toggles dry-run mode. Defaults to true so the first
// release does not actually destroy mail. Phase 6 flips the
// production default to live; operators opt out via
// `KMAIL_RETENTION_DRY_RUN=true`.
func (w *Worker) WithDryRun(b bool) *Worker {
	w.dryRun = b
	return w
}

// WithMetrics wires a Prometheus metric set into the worker. Pass
// nil to disable metrics emission.
func (w *Worker) WithMetrics(m *Metrics) *Worker {
	w.metrics = m
	return w
}

// DryRun reports whether the worker is in dry-run mode. Used by
// the admin status card.
func (w *Worker) DryRun() bool { return w.dryRun }

// Snapshot returns the most-recent enforcement totals seen by the
// worker. Counters are cumulative (not per-tick) so the admin UI
// can render "X emails deleted since boot".
func (w *Worker) Snapshot() WorkerSnapshot {
	snap := WorkerSnapshot{
		DryRun:         w.dryRun,
		EmailsDeleted:  w.lastDeletedTotal.Load(),
		EmailsArchived: w.lastArchivedTotal.Load(),
		Errors:         w.lastErrorsTotal.Load(),
	}
	if ts := w.lastEvaluatedAt.Load(); ts > 0 {
		t := time.Unix(ts, 0).UTC()
		snap.LastEvaluated = &t
	}
	return snap
}

// WorkerSnapshot is the lightweight read returned by the admin
// status card endpoint. `LastEvaluated` is nil until the first
// tick completes so the admin UI can render "never" instead of
// the Unix epoch.
type WorkerSnapshot struct {
	DryRun         bool       `json:"dry_run"`
	LastEvaluated  *time.Time `json:"last_evaluated_at"`
	EmailsDeleted  int64      `json:"emails_deleted"`
	EmailsArchived int64      `json:"emails_archived"`
	Errors         int64      `json:"errors"`
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

// engineFor builds the shared Enforcer once from the wired options
// and registers it on the Service so direct EvaluateRetention
// callers reuse the same instance. Returns nil when no operator has
// been wired (the worker then logs and skips enforcement).
//
// The sync.Once snapshots the worker's configuration (op, dryRun,
// metrics) at the first tick: this is startup-only config, so every
// With* option must be set before Run starts. There is intentionally
// no runtime reconfiguration path — a config change requires a new
// worker.
func (w *Worker) engineFor() *Enforcer {
	w.engineOnce.Do(func() {
		if w.op == nil {
			return
		}
		e := NewEnforcer(w.op, w.svc.pool, w.logger).
			WithDryRun(w.dryRun).
			WithMetrics(w.metrics)
		// The production *JMAPEnforcer is both the operator and the
		// cold-tier mover; wire the archive path when available.
		if cm, ok := w.op.(ColdMover); ok {
			e = e.WithColdMover(cm)
		}
		w.engine = e
		w.svc.WithEnforcer(e)
	})
	return w.engine
}

func (w *Worker) tick(ctx context.Context) error {
	engine := w.engineFor()
	if engine == nil {
		w.logger.Printf("retention.worker: no email operator wired; skipping tick")
		return nil
	}
	tenants, err := w.svc.ListActiveTenants(ctx)
	if err != nil {
		return err
	}
	for _, id := range tenants {
		// Per-tenant isolation: one tenant's failure (shard down,
		// RLS error, ...) must not abort the sweep for the rest.
		if err := w.evaluateTenant(ctx, engine, id); err != nil {
			w.logger.Printf("retention.worker: tenant %s: %v", id, err)
		}
	}
	return nil
}

func (w *Worker) evaluateTenant(ctx context.Context, engine *Enforcer, tenantID string) error {
	policies, err := w.svc.ListPolicies(ctx, tenantID)
	if err != nil {
		w.incErrors()
		return fmt.Errorf("list policies: %w", err)
	}
	w.lastEvaluatedAt.Store(time.Now().Unix())

	var errs []error
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		w.incEvaluations()
		run, err := engine.EnforcePolicy(ctx, tenantID, p)
		// Account whatever progress the run made even on a partial
		// failure so the admin snapshot stays accurate.
		if run != nil {
			if run.EmailsDeleted > 0 {
				w.lastDeletedTotal.Add(int64(run.EmailsDeleted))
			}
			if run.EmailsArchived > 0 {
				w.lastArchivedTotal.Add(int64(run.EmailsArchived))
			}
		}
		if err != nil {
			w.incErrors()
			errs = append(errs, fmt.Errorf("policy %s: %w", p.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (w *Worker) incEvaluations() {
	if w.metrics != nil {
		w.metrics.Evaluations.Inc()
	}
}

func (w *Worker) incErrors() {
	w.lastErrorsTotal.Add(1)
	if w.metrics != nil {
		w.metrics.Errors.Inc()
	}
}

// ---------------------------------------------------------------
// JMAP-backed enforcer
// ---------------------------------------------------------------

// JMAPEnforcer is the production email operator. It speaks JMAP to
// the tenant's Stalwart shard for query / destroy, and posts to the
// zk-object-fabric placement API to move blobs to the cold tier for
// archive policies. It implements both jmap.EmailOperator and
// ColdMover so the Enforcer can drive delete and archive policies
// through one wired object.
//
// Note: this operator addresses the shard with a single
// tenant-level accountId and treats the IDs it returns as opaque
// round-trip tokens (its own QueryEmailsByDate output feeds straight
// back into DestroyEmails). It does not emit the account-qualified
// IDs that *jmap.StalwartEmailOperator does; the two must not be
// mixed across a query/destroy boundary.
type JMAPEnforcer struct {
	Shards     ShardResolver
	HTTP       *http.Client
	Auth       string
	FabricURL  string
	FabricAuth string
	Logger     *log.Logger
}

var (
	_ jmap.EmailOperator = (*JMAPEnforcer)(nil)
	_ ColdMover          = (*JMAPEnforcer)(nil)
)

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

// QueryEmailsByDate implements jmap.EmailOperator: it asks Stalwart
// for up to `limit` email IDs received before `olderThan`, oldest
// first, optionally scoped to `mailboxID`.
func (e *JMAPEnforcer) QueryEmailsByDate(ctx context.Context, tenantID, mailboxID string, olderThan time.Time, limit int) ([]string, error) {
	if strings.TrimSpace(tenantID) == "" {
		return nil, errors.New("retention: tenantID is required")
	}
	if limit <= 0 {
		limit = queryPageSize
	}
	url, err := e.shardURL(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	filter := map[string]any{
		// JMAP UTCDate: RFC 3339 in UTC, "Z" suffix, no fractional
		// seconds (RFC 8620 §1.4).
		"before": olderThan.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if mailboxID != "" {
		filter["inMailbox"] = mailboxID
	}
	body := map[string]any{
		"using": []string{"urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"},
		"methodCalls": [][]any{
			{"Email/query", map[string]any{
				"accountId": tenantID,
				"filter":    filter,
				"sort": []map[string]any{
					{"property": "receivedAt", "isAscending": true},
				},
				"limit": limit,
			}, "c1"},
		},
	}
	var resp struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := e.jmap(ctx, url, body, &resp); err != nil {
		return nil, err
	}
	// A JMAP method-level error (e.g. ["error",{"type":"accountNotFound"}])
	// must surface as an error: unmarshalling it into the ids struct
	// would yield nil IDs, which the Enforcer paging loop would read as
	// "sweep complete" and record a false success while mail remains.
	rawArgs, err := jmapMethodArgs(resp.MethodResponses, "Email/query", "c1")
	if err != nil {
		return nil, err
	}
	var args struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return nil, fmt.Errorf("retention: decode Email/query args: %w", err)
	}
	return args.IDs, nil
}

// jmapMethodArgs extracts the args object of the first method
// response, returning an error when the response is missing, is a
// JMAP method-level `error`, or is not the expected method. This is
// the single place query/destroy guard against an HTTP-200 JMAP
// failure being mistaken for a valid (empty) result.
func jmapMethodArgs(methodResponses [][]json.RawMessage, expected, callID string) (json.RawMessage, error) {
	if len(methodResponses) == 0 || len(methodResponses[0]) < 2 {
		return nil, fmt.Errorf("retention: missing %s response (%s)", expected, callID)
	}
	var name string
	if err := json.Unmarshal(methodResponses[0][0], &name); err != nil {
		return nil, fmt.Errorf("retention: decode %s response method: %w", expected, err)
	}
	if name == "error" {
		var je struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		}
		_ = json.Unmarshal(methodResponses[0][1], &je)
		if je.Description != "" {
			return nil, fmt.Errorf("retention: %s failed: %s: %s", expected, je.Type, je.Description)
		}
		return nil, fmt.Errorf("retention: %s failed: %s", expected, je.Type)
	}
	if name != expected {
		return nil, fmt.Errorf("retention: unexpected response method for %s: %q", callID, name)
	}
	return methodResponses[0][1], nil
}

// DestroyEmails implements jmap.EmailOperator: it issues
// `Email/set` with `destroy` in batches of 100.
func (e *JMAPEnforcer) DestroyEmails(ctx context.Context, tenantID string, ids []string) error {
	if strings.TrimSpace(tenantID) == "" {
		return errors.New("retention: tenantID is required")
	}
	if len(ids) == 0 {
		return nil
	}
	url, err := e.shardURL(ctx, tenantID)
	if err != nil {
		return err
	}
	for i := 0; i < len(ids); i += destroyChunk {
		end := min(i+destroyChunk, len(ids))
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
			return err
		}
		// HTTP success does not imply JMAP success: Stalwart reports
		// per-message failures in `notDestroyed`. Surface any hard
		// failure (forbidden/serverFail/...) so the run is recorded as
		// failed instead of silently under-deleting while counting the
		// batch as removed.
		if err := checkJMAPDestroy(resp.MethodResponses, "c1"); err != nil {
			return err
		}
	}
	return nil
}

// checkJMAPDestroy inspects an `Email/set` destroy response and
// returns an error only for hard failures. A `notFound` SetError
// means the message was already gone (idempotent destroy) and is
// ignored, mirroring jmap.StalwartEmailOperator. IDs are visited in
// sorted order so the surfaced error is deterministic when a batch
// has several distinct hard failures.
func checkJMAPDestroy(methodResponses [][]json.RawMessage, callID string) error {
	rawArgs, err := jmapMethodArgs(methodResponses, "Email/set", callID)
	if err != nil {
		return err
	}
	var args struct {
		NotDestroyed map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"notDestroyed"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return fmt.Errorf("retention: decode Email/set args: %w", err)
	}
	ids := make([]string, 0, len(args.NotDestroyed))
	for id := range args.NotDestroyed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		setErr := args.NotDestroyed[id]
		if setErr.Type == "notFound" {
			continue
		}
		if setErr.Description != "" {
			return fmt.Errorf("retention: destroy %s failed: %s: %s", id, setErr.Type, setErr.Description)
		}
		return fmt.Errorf("retention: destroy %s failed: %s", id, setErr.Type)
	}
	return nil
}

// MoveToCold implements ColdMover: it flips the storage tier of the
// matching blobs to cold via the zk-object-fabric placement API.
func (e *JMAPEnforcer) MoveToCold(ctx context.Context, tenantID string, ids []string) (int, error) {
	if e.FabricURL == "" {
		return 0, errors.New("retention: fabric url not configured")
	}
	count := 0
	for i := 0; i < len(ids); i += destroyChunk {
		end := min(i+destroyChunk, len(ids))
		batch := ids[i:end]
		body := map[string]any{
			"tenant_id":   tenantID,
			"object_ids":  batch,
			"target_tier": "cold",
			"reason":      "retention_archive",
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
