package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/kennguy3n/kmail/internal/audit"
)

// storageEventReplayWindow bounds how stale an X-Kmail-Signature
// timestamp may be before the webhook rejects the request as a
// replay. Matches the Stripe webhook's 5-minute window.
const storageEventReplayWindow = 5 * time.Minute

// bucketPrefix is the fixed prefix `tenant.BucketNameFor` prepends to
// a tenant UUID when it provisions the per-tenant zk-object-fabric
// bucket (`kmail-{tenant_id}`). The webhook reverses it to map an S3
// notification's bucket name back to the owning tenant. Kept as a
// local constant rather than importing `internal/tenant` to avoid a
// dependency cycle (tenant provisioning already references billing).
const bucketPrefix = "kmail-"

// maxWebhookBody caps the storage-event webhook request body. S3
// notification documents are a few KB; 1 MiB is generous headroom
// while denying an unbounded-body DoS.
const maxWebhookBody = 1 << 20

// StorageEventMetrics is the Prometheus metric set for the
// event-sourced storage-accounting pipeline (webhook ingestion +
// reconciliation worker). Register the collectors with the same
// registry the BFF exposes on `/metrics`; pass a nil Registerer to
// skip registration (tests).
type StorageEventMetrics struct {
	Ingested        *prometheus.CounterVec
	ReconcileRuns   prometheus.Counter
	ReconcileErrors prometheus.Counter
	WebhookRejected prometheus.Counter
}

// NewStorageEventMetrics builds the metric set and registers it with
// `reg`. Pass nil to skip registration.
func NewStorageEventMetrics(reg prometheus.Registerer) *StorageEventMetrics {
	m := &StorageEventMetrics{
		Ingested: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kmail_storage_events_ingested_total",
			Help: "Storage lifecycle events recorded via the webhook, by event_type.",
		}, []string{"event_type"}),
		ReconcileRuns: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_storage_event_reconcile_total",
			Help: "Tenant storage reconciliations folded into quotas.storage_used_bytes.",
		}),
		ReconcileErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_storage_event_reconcile_errors_total",
			Help: "Errors raised while reconciling event-sourced storage totals.",
		}),
		WebhookRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "kmail_storage_webhook_rejected_total",
			Help: "Storage-event webhook requests rejected (bad signature / malformed).",
		}),
	}
	if reg != nil {
		reg.MustRegister(m.Ingested, m.ReconcileRuns, m.ReconcileErrors, m.WebhookRejected)
	}
	return m
}

func (m *StorageEventMetrics) incIngested(eventType string) {
	if m == nil {
		return
	}
	m.Ingested.WithLabelValues(eventType).Inc()
}

// StorageEventWorkerConfig wires the reconciliation worker.
type StorageEventWorkerConfig struct {
	// Pool enumerates active tenants. Required.
	Pool *pgxpool.Pool
	// Billing folds the reconciled total into `quotas`. Required.
	Billing *Service
	// Events supplies the event-sourced per-tenant total. Required.
	Events *StorageEventService
	// Interval between reconciliation sweeps. Defaults to 60s.
	Interval time.Duration
	// Audit, when set, records a system audit entry each time a
	// tenant's snapshot is updated from the event stream.
	Audit *audit.Service
	// Metrics, when set, counts reconciliation runs / errors.
	Metrics *StorageEventMetrics
	Logger  *log.Logger
}

// StorageEventWorker periodically reconciles every active tenant's
// event-sourced storage total (created minus deleted bytes) and folds
// it into `quotas.storage_used_bytes`. It is the steady-state writer
// in event mode; the QuotaWorker's hourly drift sweep then compares
// this event-sourced total against an authoritative S3 scan and
// surfaces any discrepancy (`kmail_storage_event_drift_bytes`).
type StorageEventWorker struct {
	cfg StorageEventWorkerConfig
}

// NewStorageEventWorker builds a worker with sensible defaults.
func NewStorageEventWorker(cfg StorageEventWorkerConfig) *StorageEventWorker {
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &StorageEventWorker{cfg: cfg}
}

// Run loops until `ctx` is cancelled, reconciling on each tick.
func (w *StorageEventWorker) Run(ctx context.Context) {
	if w.cfg.Pool == nil || w.cfg.Billing == nil || w.cfg.Events == nil {
		w.cfg.Logger.Printf("storage event worker: not configured, exiting")
		return
	}
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	if err := w.tick(ctx); err != nil {
		w.cfg.Logger.Printf("storage event worker first tick: %v", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.tick(ctx); err != nil {
				w.cfg.Logger.Printf("storage event worker tick: %v", err)
			}
		}
	}
}

func (w *StorageEventWorker) tick(ctx context.Context) error {
	ids, err := listActiveTenantIDs(ctx, w.cfg.Pool)
	if err != nil {
		return err
	}
	for _, id := range ids {
		total, err := w.cfg.Events.ReconcileTenant(ctx, id)
		if err != nil {
			if w.cfg.Metrics != nil {
				w.cfg.Metrics.ReconcileErrors.Inc()
			}
			w.cfg.Logger.Printf("storage event worker: reconcile tenant %s: %v", id, err)
			continue
		}
		// `quotas.storage_used_bytes` is CHECK (>= 0); a transient
		// negative event-sourced total (delete observed before its
		// create) clamps to zero. The drift sweep will reconcile the
		// true value against the S3 scan on the next hourly pass.
		if total < 0 {
			total = 0
		}
		if err := w.cfg.Billing.SetStorageUsage(ctx, id, total); err != nil {
			if w.cfg.Metrics != nil {
				w.cfg.Metrics.ReconcileErrors.Inc()
			}
			w.cfg.Logger.Printf("storage event worker: set usage tenant %s: %v", id, err)
			continue
		}
		if w.cfg.Metrics != nil {
			w.cfg.Metrics.ReconcileRuns.Inc()
		}
		w.auditSnapshot(ctx, id, total)
	}
	return nil
}

func (w *StorageEventWorker) auditSnapshot(ctx context.Context, tenantID string, total int64) {
	if w.cfg.Audit == nil {
		return
	}
	if _, err := w.cfg.Audit.Log(ctx, audit.Entry{
		TenantID:     tenantID,
		ActorID:      "storage-event-worker",
		ActorType:    audit.ActorSystem,
		Action:       "storage.usage_reconciled",
		ResourceType: "quota",
		ResourceID:   tenantID,
		Metadata:     map[string]any{"storage_used_bytes": total},
	}); err != nil {
		w.cfg.Logger.Printf("storage event worker: audit tenant %s: %v", tenantID, err)
	}
}

// StorageEventWebhookConfig wires the S3-notification webhook.
type StorageEventWebhookConfig struct {
	// Events records each parsed notification. Required.
	Events *StorageEventService
	// HMACSecret is the shared secret zk-object-fabric signs
	// notifications with. Empty = dev mode = accept everything
	// (parity with the Stripe webhook / OIDC dev-bypass). Production
	// deployments MUST set it.
	HMACSecret string
	// Audit, when set, records one system entry per tenant per
	// delivered batch.
	Audit   *audit.Service
	Metrics *StorageEventMetrics
	Logger  *log.Logger
	// Now overrides time.Now for deterministic replay-window tests.
	Now func() time.Time
}

// StorageEventWebhook ingests S3-compatible object lifecycle
// notifications from zk-object-fabric and records them as
// `storage_events` rows. It authenticates each request with an
// HMAC-SHA256 signature over the raw body.
type StorageEventWebhook struct {
	cfg StorageEventWebhookConfig
}

// NewStorageEventWebhook builds the handler with sensible defaults.
func NewStorageEventWebhook(cfg StorageEventWebhookConfig) *StorageEventWebhook {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &StorageEventWebhook{cfg: cfg}
}

// Register mounts the webhook. Intentionally unauthenticated by the
// OIDC middleware — the request is authenticated by its HMAC
// signature, not a bearer token (parity with the Stripe webhook).
func (h *StorageEventWebhook) Register(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/storage/events", http.HandlerFunc(h.serve))
}

// s3NotificationEnvelope is the top-level S3 event-notification
// document (`{"Records":[...]}`) zk-object-fabric delivers — one
// record per POST, matching AWS's per-destination fan-out.
type s3NotificationEnvelope struct {
	Records []s3NotificationRecord `json:"Records"`
}

type s3NotificationRecord struct {
	EventName string `json:"eventName"`
	S3        struct {
		Bucket struct {
			Name string `json:"name"`
		} `json:"bucket"`
		Object struct {
			Key string `json:"key"`
			// Size is a pointer because zk-object-fabric (like AWS)
			// only emits it on ObjectCreated records; ObjectRemoved
			// records omit it. nil therefore means "size unknown".
			Size *int64 `json:"size,omitempty"`
		} `json:"object"`
	} `json:"s3"`
}

func (h *StorageEventWebhook) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		h.reject(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := h.verify(r, body); err != nil {
		h.cfg.Logger.Printf("billing.storage_webhook: signature: %v", err)
		h.reject(w, http.StatusUnauthorized, "invalid signature")
		return
	}
	var env s3NotificationEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		h.reject(w, http.StatusBadRequest, "decode: "+err.Error())
		return
	}

	// Tally per-tenant so a delivered batch produces one audit entry
	// per tenant instead of one per object.
	type tally struct {
		created, deleted int
		netBytes         int64
	}
	tallies := map[string]*tally{}
	var failed bool
	for _, rec := range env.Records {
		eventType, ok := classifyEventName(rec.EventName)
		if !ok {
			// Unknown S3 event class (e.g. ObjectRestore) — ignore so
			// the producer is not forced into a retry loop.
			continue
		}
		tenantID := tenantIDFromBucket(rec.S3.Bucket.Name)
		if tenantID == "" {
			h.cfg.Logger.Printf("billing.storage_webhook: empty tenant for bucket %q", rec.S3.Bucket.Name)
			continue
		}
		var size int64
		if rec.S3.Object.Size != nil {
			size = *rec.S3.Object.Size
		}
		key := decodeObjectKey(rec.S3.Object.Key)
		if err := h.cfg.Events.RecordEvent(r.Context(), tenantID, eventType, key, size); err != nil {
			h.cfg.Logger.Printf("billing.storage_webhook: record %s/%s: %v", tenantID, eventType, err)
			failed = true
			continue
		}
		h.cfg.Metrics.incIngested(eventType)
		t := tallies[tenantID]
		if t == nil {
			t = &tally{}
			tallies[tenantID] = t
		}
		if eventType == EventObjectCreated {
			t.created++
			t.netBytes += size
		} else {
			t.deleted++
			t.netBytes -= size
		}
	}

	if h.cfg.Audit != nil {
		for tenantID, t := range tallies {
			if _, err := h.cfg.Audit.Log(r.Context(), audit.Entry{
				TenantID:     tenantID,
				ActorID:      "zk-object-fabric",
				ActorType:    audit.ActorSystem,
				Action:       "storage.events_ingested",
				ResourceType: "storage_events",
				Metadata: map[string]any{
					"created":   t.created,
					"deleted":   t.deleted,
					"net_bytes": t.netBytes,
				},
			}); err != nil {
				h.cfg.Logger.Printf("billing.storage_webhook: audit tenant %s: %v", tenantID, err)
			}
		}
	}

	if failed {
		// At-least-once: surface a 5xx so the gateway retries. Any
		// resulting duplicate events are bounded and corrected by the
		// QuotaWorker's hourly drift sweep against the S3 scan.
		http.Error(w, "one or more events failed to record", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *StorageEventWebhook) reject(w http.ResponseWriter, status int, msg string) {
	if h.cfg.Metrics != nil {
		h.cfg.Metrics.WebhookRejected.Inc()
	}
	http.Error(w, msg, status)
}

// verify authenticates the request via the X-Kmail-Signature header,
// formatted `t=<unix-seconds>,v1=<hex HMAC-SHA256>` over
// `<t>.<rawBody>`. Empty secret = dev mode = accept. The timestamp is
// bound into the signed payload and checked against the replay window
// so a captured notification cannot be replayed to re-inflate (or
// deflate) a tenant's usage.
func (h *StorageEventWebhook) verify(r *http.Request, body []byte) error {
	if h.cfg.HMACSecret == "" {
		return nil
	}
	header := r.Header.Get("X-Kmail-Signature")
	if header == "" {
		return errors.New("missing X-Kmail-Signature header")
	}
	var t, v1 string
	for _, kv := range strings.Split(header, ",") {
		parts := strings.SplitN(strings.TrimSpace(kv), "=", 2)
		if len(parts) != 2 {
			continue
		}
		switch parts[0] {
		case "t":
			t = parts[1]
		case "v1":
			v1 = parts[1]
		}
	}
	if t == "" || v1 == "" {
		return errors.New("malformed X-Kmail-Signature header")
	}
	mac := hmac.New(sha256.New, []byte(h.cfg.HMACSecret))
	mac.Write([]byte(t + "." + string(body)))
	expected := mac.Sum(nil)
	// Decode the supplied hex signature to raw bytes and compare with
	// the constant-time hmac.Equal. Decoding first means a malformed
	// or wrong-length signature is rejected without leaking timing.
	sig, err := hex.DecodeString(v1)
	if err != nil {
		return errors.New("malformed X-Kmail-Signature value")
	}
	if !hmac.Equal(expected, sig) {
		return errors.New("signature mismatch")
	}
	tsSeconds, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return errors.New("malformed X-Kmail-Signature timestamp")
	}
	if delta := h.cfg.Now().Sub(time.Unix(tsSeconds, 0)); delta > storageEventReplayWindow || delta < -storageEventReplayWindow {
		return errors.New("X-Kmail-Signature timestamp outside replay window")
	}
	return nil
}

// classifyEventName maps an S3 event name (`s3:ObjectCreated:Put`,
// `s3:ObjectRemoved:Delete`, …) to a canonical storage event type.
// The second return is false for event classes we do not account for.
func classifyEventName(name string) (string, bool) {
	switch {
	case strings.HasPrefix(name, "s3:ObjectCreated:"):
		return EventObjectCreated, true
	case strings.HasPrefix(name, "s3:ObjectRemoved:"):
		return EventObjectDeleted, true
	default:
		return "", false
	}
}

// tenantIDFromBucket reverses `tenant.BucketNameFor`, stripping the
// `kmail-` prefix to recover the tenant UUID from a notification's
// bucket name. The match is case-insensitive because bucket names are
// lowercased at provisioning time.
func tenantIDFromBucket(bucket string) string {
	b := strings.TrimSpace(bucket)
	if len(b) >= len(bucketPrefix) && strings.EqualFold(b[:len(bucketPrefix)], bucketPrefix) {
		return b[len(bucketPrefix):]
	}
	return b
}

// decodeObjectKey best-effort URL-decodes an S3 object key (S3
// notification keys are URL-encoded, spaces as '+'). On decode failure
// the raw key is returned — the key is informational for accounting.
func decodeObjectKey(key string) string {
	if dec, err := url.QueryUnescape(key); err == nil {
		return dec
	}
	return key
}
