package billing

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// multiRecordBody renders an S3 notification with several records,
// including one unknown event class and one unroutable bucket, so the
// webhook's skip branches are exercised alongside the happy path.
func multiRecordBody(tenant string) []byte {
	return []byte(fmt.Sprintf(`{"Records":[
		{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"kmail-%s"},"object":{"key":"a.eml","size":1000}}},
		{"eventName":"s3:ObjectRestore:Post","s3":{"bucket":{"name":"kmail-%s"},"object":{"key":"b.eml","size":50}}},
		{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"not-a-tenant"},"object":{"key":"c.eml","size":70}}}
	]}`, tenant, tenant))
}

// TestStorageWebhook_ServeAuditAndMetricsDB drives a multi-record
// envelope through serve() with Audit and Metrics wired, asserting the
// known record is recorded, the unknown event class and unroutable
// bucket are skipped, and a single audit entry is emitted.
func TestStorageWebhook_ServeAuditAndMetricsDB(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")
	events := NewStorageEventService(pool)

	secret := "zkof_whsec_audit"
	now := time.Now()
	metrics := NewStorageEventMetrics(nil)
	h := NewStorageEventWebhook(StorageEventWebhookConfig{
		Events:     events,
		HMACSecret: secret,
		Audit:      newAuditService(pool),
		Metrics:    metrics,
		Logger:     quietLogger(),
		Now:        func() time.Time { return now },
	})

	body := multiRecordBody(tenant)
	sig := signStorageEvent(secret, now.Unix(), body)
	if rec := postEvents(h, sig, body); rec.Code != http.StatusNoContent {
		t.Fatalf("serve status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Only the single routable ObjectCreated record was recorded.
	if got := countStorageEvents(t, pool, tenant); got != 1 {
		t.Errorf("storage events rows = %d, want 1", got)
	}
	total, err := events.ReconcileTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("ReconcileTenant: %v", err)
	}
	if total != 1000 {
		t.Errorf("reconciled total = %d, want 1000", total)
	}
	// One ingest audit entry for the tenant batch.
	if n := countAudit(t, pool, tenant, "storage.events_ingested"); n != 1 {
		t.Errorf("ingest audit rows = %d, want 1", n)
	}
}

func TestNewStorageEventMetricsRegisters(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewStorageEventMetrics(reg)
	if m == nil {
		t.Fatal("nil metrics")
	}
	m.incIngested(EventObjectCreated)
	m.ReconcileRuns.Inc()
	m.ReconcileErrors.Inc()
	m.WebhookRejected.Inc()

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	want := map[string]bool{
		"kmail_storage_events_ingested_total":        false,
		"kmail_storage_event_reconcile_total":        false,
		"kmail_storage_event_reconcile_errors_total": false,
		"kmail_storage_webhook_rejected_total":       false,
	}
	for _, mf := range mfs {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric %q not registered", name)
		}
	}

	// nil receiver incIngested is a safe no-op.
	var nilM *StorageEventMetrics
	nilM.incIngested(EventObjectCreated)
}
