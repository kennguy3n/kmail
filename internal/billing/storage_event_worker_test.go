package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signStorageEvent returns an X-Kmail-Signature header value over
// `<ts>.<body>` using `secret`, mimicking the zk-object-fabric signer.
func signStorageEvent(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", ts, string(body))))
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// newWebhook builds a webhook bound to a nil-pool event service so the
// HMAC / parsing paths can be exercised without a database (RecordEvent
// is a no-op on a nil pool).
func newWebhook(secret string, now func() time.Time) *StorageEventWebhook {
	return NewStorageEventWebhook(StorageEventWebhookConfig{
		Events:     NewStorageEventService(nil),
		HMACSecret: secret,
		Logger:     quietLogger(),
		Now:        now,
	})
}

func postEvents(h *StorageEventWebhook, header string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/storage/events", strings.NewReader(string(body)))
	if header != "" {
		req.Header.Set("X-Kmail-Signature", header)
	}
	rec := httptest.NewRecorder()
	h.serve(rec, req)
	return rec
}

func TestStorageWebhook_HMAC(t *testing.T) {
	secret := "zkof_whsec_test"
	now := time.Unix(1_700_000_000, 0)
	h := newWebhook(secret, func() time.Time { return now })
	body := []byte(`{"Records":[]}`)

	t.Run("valid signature accepted", func(t *testing.T) {
		sig := signStorageEvent(secret, now.Unix(), body)
		if rec := postEvents(h, sig, body); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
	})

	t.Run("missing header rejected", func(t *testing.T) {
		if rec := postEvents(h, "", body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("tampered body rejected", func(t *testing.T) {
		sig := signStorageEvent(secret, now.Unix(), body)
		if rec := postEvents(h, sig, []byte(`{"Records":[{"eventName":"x"}]}`)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("wrong secret rejected", func(t *testing.T) {
		sig := signStorageEvent("other-secret", now.Unix(), body)
		if rec := postEvents(h, sig, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("malformed header rejected", func(t *testing.T) {
		if rec := postEvents(h, "not-a-signature", body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("replayed signature rejected", func(t *testing.T) {
		old := now.Add(-2 * storageEventReplayWindow).Unix()
		sig := signStorageEvent(secret, old, body)
		if rec := postEvents(h, sig, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func TestStorageWebhook_DevModeAcceptsUnsigned(t *testing.T) {
	// Empty secret = dev mode = accept everything (parity with the
	// Stripe webhook). Production MUST set the secret.
	h := newWebhook("", time.Now)
	if rec := postEvents(h, "", []byte(`{"Records":[]}`)); rec.Code != http.StatusNoContent {
		t.Fatalf("dev-mode status = %d, want 204", rec.Code)
	}
}

func TestClassifyEventName(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"s3:ObjectCreated:Put":                     {EventObjectCreated, true},
		"s3:ObjectCreated:CompleteMultipartUpload": {EventObjectCreated, true},
		"s3:ObjectRemoved:Delete":                  {EventObjectDeleted, true},
		"s3:ObjectRemoved:DeleteMarkerCreated":     {EventObjectDeleted, true},
		"s3:ObjectRestore:Completed":               {"", false},
		"s3:ReducedRedundancyLostObject":           {"", false},
		"garbage":                                  {"", false},
	}
	for name, want := range cases {
		got, ok := classifyEventName(name)
		if got != want.want || ok != want.ok {
			t.Errorf("classifyEventName(%q) = (%q,%v), want (%q,%v)", name, got, ok, want.want, want.ok)
		}
	}
}

func TestTenantIDFromBucket(t *testing.T) {
	cases := map[string]string{
		"kmail-abc123": "abc123",
		"KMAIL-AbC":    "AbC",
		"  kmail-x  ":  "x",
		// Buckets without the kmail- prefix are not tenant buckets and
		// must map to "" so the webhook skips them instead of feeding a
		// non-UUID into RecordEvent (which would 500 + retry forever).
		"raw-bucket": "",
		"kmail-":     "", // prefix only, no tenant id
		"":           "",
	}
	for in, want := range cases {
		if got := tenantIDFromBucket(in); got != want {
			t.Errorf("tenantIDFromBucket(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeObjectKey(t *testing.T) {
	if got := decodeObjectKey("folder/my%20file.txt"); got != "folder/my file.txt" {
		t.Errorf("decodeObjectKey = %q", got)
	}
	// Invalid escape sequence falls back to the raw key.
	if got := decodeObjectKey("bad%ZZkey"); got != "bad%ZZkey" {
		t.Errorf("decodeObjectKey invalid = %q", got)
	}
}

// --- DB-backed integration tests ---

func TestStorageEventWorker_Reconcile(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")

	events := NewStorageEventService(pool)
	billingSvc := NewService(Config{Pool: pool})
	// 3 creates of 100, 1 delete of 100 → net 200.
	for i := 0; i < 3; i++ {
		mustRecord(t, events, tenant, EventObjectCreated, 100)
	}
	mustRecord(t, events, tenant, EventObjectDeleted, 100)

	w := NewStorageEventWorker(StorageEventWorkerConfig{
		Pool:    pool,
		Billing: billingSvc,
		Events:  events,
		Logger:  quietLogger(),
	})
	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	q, err := billingSvc.GetQuota(ctx, tenant)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.StorageUsedBytes != 200 {
		t.Fatalf("storage_used_bytes = %d, want 200", q.StorageUsedBytes)
	}
}

// TestStorageEventWorker_ClampsNegative ensures a delete-before-create
// (negative event total) clamps to zero instead of violating the
// quotas CHECK (storage_used_bytes >= 0).
func TestStorageEventWorker_ClampsNegative(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")

	events := NewStorageEventService(pool)
	billingSvc := NewService(Config{Pool: pool})
	mustRecord(t, events, tenant, EventObjectDeleted, 500) // orphan delete

	w := NewStorageEventWorker(StorageEventWorkerConfig{
		Pool: pool, Billing: billingSvc, Events: events, Logger: quietLogger(),
	})
	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	q, err := billingSvc.GetQuota(ctx, tenant)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.StorageUsedBytes != 0 {
		t.Fatalf("storage_used_bytes = %d, want 0 (clamped)", q.StorageUsedBytes)
	}
}

// TestStorageEventWorker_AuditsOnlyOnChange verifies the reconcile
// worker audits a tenant's snapshot when it first appears / changes
// but stays silent on subsequent ticks where the total is unchanged,
// so the tamper-evident audit_log is not flooded every 60s.
func TestStorageEventWorker_AuditsOnlyOnChange(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")

	events := NewStorageEventService(pool)
	billingSvc := NewService(Config{Pool: pool})
	mustRecord(t, events, tenant, EventObjectCreated, 100)

	w := NewStorageEventWorker(StorageEventWorkerConfig{
		Pool:    pool,
		Billing: billingSvc,
		Events:  events,
		Audit:   newAuditService(pool),
		Logger:  quietLogger(),
	})

	// First tick: total goes 0 → 100, one audit row expected.
	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if n := countAudit(t, pool, tenant, "storage.usage_reconciled"); n != 1 {
		t.Fatalf("audit rows after first change = %d, want 1", n)
	}

	// Second tick with no new events: total unchanged, no new audit.
	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if n := countAudit(t, pool, tenant, "storage.usage_reconciled"); n != 1 {
		t.Fatalf("audit rows after unchanged tick = %d, want 1 (no flood)", n)
	}

	// New event changes the total: a second audit row is expected.
	mustRecord(t, events, tenant, EventObjectCreated, 50)
	if err := w.tick(ctx); err != nil {
		t.Fatalf("tick 3: %v", err)
	}
	if n := countAudit(t, pool, tenant, "storage.usage_reconciled"); n != 2 {
		t.Fatalf("audit rows after second change = %d, want 2", n)
	}
}

// TestStorageWebhook_IngestRecords drives a signed S3 notification
// through the webhook and asserts it is reconciled end-to-end.
func TestStorageWebhook_IngestRecords(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenant := seedTenant(t, pool, "active")
	events := NewStorageEventService(pool)

	secret := "zkof_whsec_ingest"
	now := time.Now()
	h := NewStorageEventWebhook(StorageEventWebhookConfig{
		Events:     events,
		HMACSecret: secret,
		Logger:     quietLogger(),
		Now:        func() time.Time { return now },
	})

	created := s3Body(tenant, "s3:ObjectCreated:Put", "inbox/a.eml", ptr(int64(900)))
	sig := signStorageEvent(secret, now.Unix(), created)
	if rec := postEvents(h, sig, created); rec.Code != http.StatusNoContent {
		t.Fatalf("create ingest status = %d, want 204", rec.Code)
	}

	// ObjectRemoved omits size, mirroring AWS/zk-object-fabric — the
	// recorded delete therefore has size 0 and the drift sweep is what
	// reconciles the true magnitude later.
	removed := s3Body(tenant, "s3:ObjectRemoved:Delete", "inbox/a.eml", nil)
	sig = signStorageEvent(secret, now.Unix(), removed)
	if rec := postEvents(h, sig, removed); rec.Code != http.StatusNoContent {
		t.Fatalf("remove ingest status = %d, want 204", rec.Code)
	}

	if got := countStorageEvents(t, pool, tenant); got != 2 {
		t.Fatalf("rows = %d, want 2", got)
	}
	total, err := events.ReconcileTenant(ctx, tenant)
	if err != nil {
		t.Fatalf("ReconcileTenant: %v", err)
	}
	if total != 900 { // 900 created, 0 from the size-less delete
		t.Fatalf("reconciled total = %d, want 900", total)
	}
}

func mustRecord(t *testing.T, s *StorageEventService, tenant, eventType string, size int64) {
	t.Helper()
	if err := s.RecordEvent(context.Background(), tenant, eventType, "k", size); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// s3Body renders a single-record S3 notification envelope for a tenant.
func s3Body(tenantID, eventName, key string, size *int64) []byte {
	sizeField := ""
	if size != nil {
		sizeField = fmt.Sprintf(`,"size":%d`, *size)
	}
	return []byte(fmt.Sprintf(
		`{"Records":[{"eventName":%q,"s3":{"bucket":{"name":"kmail-%s"},"object":{"key":%q%s}}}]}`,
		eventName, tenantID, key, sizeField,
	))
}
