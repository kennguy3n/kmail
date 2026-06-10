package jmap

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// testPool dials the integration database named by
// KMAIL_TEST_DATABASE_URL (or DATABASE_URL) and skips the calling
// test when neither is set. The pool is shared per-process and
// capped to a small connection count so the full jmap suite does
// not exhaust Postgres' client slots.
var (
	jmapPoolOnce sync.Once
	jmapPool     *pgxpool.Pool
	jmapPoolErr  error
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("KMAIL_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		t.Skip("set KMAIL_TEST_DATABASE_URL or DATABASE_URL to run jmap DB tests")
	}
	jmapPoolOnce.Do(func() {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			jmapPoolErr = err
			return
		}
		cfg.MaxConns = 4
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		jmapPool, jmapPoolErr = pgxpool.NewWithConfig(ctx, cfg)
		if jmapPoolErr == nil {
			jmapPoolErr = jmapPool.Ping(ctx)
		}
	})
	if jmapPoolErr != nil {
		t.Skipf("database unreachable (%v); skipping integration test", jmapPoolErr)
	}
	return jmapPool
}

var jmapTenantSeq int64

// seedTenant inserts a tenant and registers cleanup that removes its
// attachment_links / storage credentials (both ON DELETE RESTRICT)
// before deleting the tenant row itself.
func seedTenant(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	n := atomic.AddInt64(&jmapTenantSeq, 1)
	slug := fmt.Sprintf("jmap-test-%d-%d", time.Now().UnixNano(), n)
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO tenants (name, slug, plan, status)
		VALUES ('jmap-test', $1, 'pro', 'active')
		RETURNING id::text
	`, slug).Scan(&id); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM attachment_links WHERE tenant_id = $1::uuid`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenant_storage_credentials WHERE tenant_id = $1::uuid`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenants WHERE id = $1::uuid`, id)
	})
	return id
}

// mockS3 records PUT object keys and lets a test force a non-2xx
// response to exercise the upload error path.
type mockS3 struct {
	srv      *httptest.Server
	mu       sync.Mutex
	puts     []string
	failCode int
}

func newMockS3(t *testing.T) *mockS3 {
	t.Helper()
	m := &mockS3{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			m.mu.Lock()
			m.puts = append(m.puts, r.URL.Path)
			code := m.failCode
			m.mu.Unlock()
			if code != 0 {
				w.WriteHeader(code)
				_, _ = w.Write([]byte("<Error>denied</Error>"))
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockS3) putCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.puts)
}

func attachmentService(t *testing.T, pool *pgxpool.Pool, s3url string) *AttachmentService {
	t.Helper()
	return NewAttachmentService(AttachmentConfig{
		Pool:      pool,
		S3URL:     s3url,
		AccessKey: "AKIAEXAMPLE",
		SecretKey: "secretkey",
		Bucket:    "kmail-global",
		Region:    "us-east-1",
		Expiry:    time.Hour,
	})
}

// TestUploadLargeAttachmentDB drives the full upload path: SigV4 PUT
// to the mock S3 endpoint, attachment_links insert under the tenant
// GUC, and a presigned GET URL in the response.
func TestUploadLargeAttachmentDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	s3 := newMockS3(t)
	svc := attachmentService(t, pool, s3.srv.URL)
	ctx := context.Background()

	body := strings.NewReader("hello attachment payload")
	out, err := svc.UploadLargeAttachment(ctx, tenant, "report final.pdf", "application/pdf", body, int64(body.Len()))
	if err != nil {
		t.Fatalf("UploadLargeAttachment: %v", err)
	}
	if out.ID == "" {
		t.Error("expected attachment_links row ID")
	}
	if !strings.Contains(out.URL, "X-Amz-Signature=") {
		t.Errorf("presigned URL missing signature: %s", out.URL)
	}
	if !strings.Contains(out.URL, "/kmail-global/"+tenant+"/") {
		t.Errorf("object key not under tenant prefix: %s", out.URL)
	}
	if s3.putCount() != 1 {
		t.Errorf("expected 1 S3 PUT, got %d", s3.putCount())
	}

	// The row should round-trip through GetAttachmentLink.
	got, err := svc.GetAttachmentLink(ctx, tenant, out.ID)
	if err != nil {
		t.Fatalf("GetAttachmentLink: %v", err)
	}
	if got.Filename != "report final.pdf" {
		t.Errorf("filename=%q", got.Filename)
	}
}

// TestUploadLargeAttachmentValidation covers the guard clauses and
// the S3 PUT failure path.
func TestUploadLargeAttachmentValidation(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	s3 := newMockS3(t)
	svc := attachmentService(t, pool, s3.srv.URL)
	ctx := context.Background()

	if _, err := svc.UploadLargeAttachment(ctx, "", "f.bin", "", strings.NewReader("x"), 1); err == nil {
		t.Error("missing tenant should error")
	}
	if _, err := svc.UploadLargeAttachment(ctx, tenant, "", "", strings.NewReader("x"), 1); err == nil {
		t.Error("missing filename should error")
	}

	s3.mu.Lock()
	s3.failCode = http.StatusForbidden
	s3.mu.Unlock()
	if _, err := svc.UploadLargeAttachment(ctx, tenant, "f.bin", "", strings.NewReader("x"), 1); err == nil {
		t.Error("S3 PUT 403 should surface as upload error")
	}
}

// TestGetAttachmentLinkStatesDB covers not-found, revoked, and
// expired branches of GetAttachmentLink.
func TestGetAttachmentLinkStatesDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	svc := attachmentService(t, pool, "")
	ctx := context.Background()

	// Validation.
	if _, err := svc.GetAttachmentLink(ctx, "", "x"); err == nil {
		t.Error("missing tenant should error")
	}
	// Not found (random UUID).
	if _, err := svc.GetAttachmentLink(ctx, tenant, "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Error("missing row should error")
	}

	// Insert an active row directly, then a revoked and an expired one.
	active := insertLink(t, pool, tenant, "active.bin", time.Now().Add(time.Hour), false)
	revoked := insertLink(t, pool, tenant, "revoked.bin", time.Now().Add(time.Hour), true)
	expired := insertLink(t, pool, tenant, "expired.bin", time.Now().Add(-time.Hour), false)

	// Active row needs S3 config to presign; configure it now.
	svc = attachmentService(t, pool, "https://s3.example.com")
	if _, err := svc.GetAttachmentLink(ctx, tenant, active); err != nil {
		t.Errorf("active link: %v", err)
	}
	if _, err := svc.GetAttachmentLink(ctx, tenant, revoked); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("revoked link err=%v", err)
	}
	if _, err := svc.GetAttachmentLink(ctx, tenant, expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired link err=%v", err)
	}
}

// TestRevokeAttachmentDB verifies the revoked flag is persisted and
// that a nil-pool service is a no-op.
func TestRevokeAttachmentDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	svc := attachmentService(t, pool, "https://s3.example.com")
	ctx := context.Background()

	id := insertLink(t, pool, tenant, "doc.bin", time.Now().Add(time.Hour), false)
	if err := svc.RevokeAttachment(ctx, tenant, id); err != nil {
		t.Fatalf("RevokeAttachment: %v", err)
	}
	if _, err := svc.GetAttachmentLink(ctx, tenant, id); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("after revoke err=%v", err)
	}

	// Guard clauses.
	if err := svc.RevokeAttachment(ctx, "", id); err == nil {
		t.Error("missing tenant should error")
	}
	if err := NewAttachmentService(AttachmentConfig{}).RevokeAttachment(ctx, tenant, id); err != nil {
		t.Errorf("nil-pool revoke should be no-op, got %v", err)
	}
}

// TestResolveTenantBucketDB confirms a provisioned per-tenant bucket
// is used and that legacy tenants fall back to the global bucket.
func TestResolveTenantBucketDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	svc := attachmentService(t, pool, "https://s3.example.com")
	ctx := context.Background()

	// Legacy tenant (no credentials row) → global bucket.
	if got := svc.resolveTenantBucket(ctx, tenant); got != "kmail-global" {
		t.Errorf("legacy bucket=%q want kmail-global", got)
	}

	// Provision a dedicated bucket.
	if _, err := pool.Exec(ctx, `
		INSERT INTO tenant_storage_credentials (tenant_id, bucket_name, access_key, encrypted_secret_key)
		VALUES ($1::uuid, $2, 'ak', '\x00')
	`, tenant, "kmail-"+tenant); err != nil {
		t.Fatalf("seed storage credentials: %v", err)
	}
	if got := svc.resolveTenantBucket(ctx, tenant); got != "kmail-"+tenant {
		t.Errorf("dedicated bucket=%q", got)
	}
}

// insertLink inserts an attachment_links row under the tenant GUC and
// returns its UUID.
func insertLink(t *testing.T, pool *pgxpool.Pool, tenant, filename string, expiry time.Time, revoked bool) string {
	t.Helper()
	ctx := context.Background()
	var id string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenant); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO attachment_links (tenant_id, object_key, filename, size_bytes, content_type, expiry, revoked)
			VALUES ($1::uuid, $2, $3, 10, 'application/octet-stream', $4, $5)
			RETURNING id::text
		`, tenant, "obj/"+filename, filename, expiry, revoked).Scan(&id)
	})
	if err != nil {
		t.Fatalf("insert link: %v", err)
	}
	return id
}

// --- handler-level tests ---------------------------------------

func authedRequest(method, target, tenant string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	return r.WithContext(middleware.WithTenantID(r.Context(), tenant))
}

func TestAttachmentHandlersUploadDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	s3 := newMockS3(t)
	h := NewAttachmentHandlers(attachmentService(t, pool, s3.srv.URL), nil)

	var buf strings.Builder
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "note.txt")
	_, _ = fw.Write([]byte("attachment body"))
	_ = mw.Close()

	r := authedRequest("POST", "/api/v1/attachments/upload", tenant, strings.NewReader(buf.String()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.upload(rr, r)
	if rr.Code != http.StatusCreated {
		t.Fatalf("upload code=%d body=%s", rr.Code, rr.Body.String())
	}

	// Missing tenant context → 403.
	rr = httptest.NewRecorder()
	h.upload(rr, httptest.NewRequest("POST", "/api/v1/attachments/upload", nil))
	if rr.Code != http.StatusForbidden {
		t.Errorf("no-tenant upload code=%d want 403", rr.Code)
	}

	// Missing multipart "file" field → 400.
	bad := authedRequest("POST", "/api/v1/attachments/upload", tenant, strings.NewReader(""))
	bad.Header.Set("Content-Type", mw.FormDataContentType())
	rr = httptest.NewRecorder()
	h.upload(rr, bad)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("missing file code=%d want 400", rr.Code)
	}
}

func TestAttachmentHandlersLinkRevokeDB(t *testing.T) {
	pool := testPool(t)
	tenant := seedTenant(t, pool)
	h := NewAttachmentHandlers(attachmentService(t, pool, "https://s3.example.com"), nil)
	id := insertLink(t, pool, tenant, "doc.bin", time.Now().Add(time.Hour), false)

	// link: happy path.
	r := authedRequest("GET", "/api/v1/attachments/"+id+"/link", tenant, nil)
	r.SetPathValue("id", id)
	rr := httptest.NewRecorder()
	h.link(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("link code=%d body=%s", rr.Code, rr.Body.String())
	}

	// link: missing tenant → 403.
	rr = httptest.NewRecorder()
	h.link(rr, httptest.NewRequest("GET", "/x", nil))
	if rr.Code != http.StatusForbidden {
		t.Errorf("no-tenant link code=%d want 403", rr.Code)
	}

	// link: unknown id → 404.
	r = authedRequest("GET", "/x", tenant, nil)
	r.SetPathValue("id", "00000000-0000-0000-0000-000000000000")
	rr = httptest.NewRecorder()
	h.link(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown link code=%d want 404", rr.Code)
	}

	// revoke: happy path → 204.
	r = authedRequest("DELETE", "/api/v1/attachments/"+id, tenant, nil)
	r.SetPathValue("id", id)
	rr = httptest.NewRecorder()
	h.revoke(rr, r)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("revoke code=%d body=%s", rr.Code, rr.Body.String())
	}

	// revoke: missing tenant → 403.
	rr = httptest.NewRecorder()
	h.revoke(rr, httptest.NewRequest("DELETE", "/x", nil))
	if rr.Code != http.StatusForbidden {
		t.Errorf("no-tenant revoke code=%d want 403", rr.Code)
	}
}
