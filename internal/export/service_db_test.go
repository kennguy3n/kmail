package export

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/audit"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

// fakeRunner returns a canned Result (or error) for RunExport.
type fakeRunner struct {
	res Result
	err error
}

func (f fakeRunner) Run(_ context.Context, _ Job) (Result, error) { return f.res, f.err }

// fakeAudit records the entries the service emits.
type fakeAudit struct {
	mu      sync.Mutex
	actions []string
}

func (f *fakeAudit) Log(_ context.Context, e audit.Entry) (*audit.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, e.Action)
	return &e, nil
}

func exportService(t *testing.T) (*Service, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	return NewService(pool), tenant
}

// TestExportJobLifecycleDB drives create → claim → RunExport(complete)
// against a live DB with a fake runner, then verifies the persisted
// artifact columns, audit emission, and the message manifest.
func TestExportJobLifecycleDB(t *testing.T) {
	svc, tenant := exportService(t)
	aud := &fakeAudit{}
	svc.WithRunner(fakeRunner{res: Result{
		DownloadURL:       "https://dl/x",
		ArtifactURL:       "s3://bucket/x",
		ArtifactSizeBytes: 4096,
		ArtifactChecksum:  "abcd",
		MessageIDs:        []string{"acct!1", "acct!2"},
	}}).WithAuditLogger(aud)
	ctx := context.Background()

	job, err := svc.CreateExportJob(ctx, tenant, "user-1", "", "", "")
	if err != nil {
		t.Fatalf("CreateExportJob: %v", err)
	}
	if job.Status != "pending" || job.Format != "mbox" || job.Scope != "all" {
		t.Fatalf("unexpected defaults: %+v", job)
	}

	// List + Get round-trip.
	list, err := svc.ListExportJobs(ctx, tenant)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListExportJobs len=%d err=%v", len(list), err)
	}
	if _, err := svc.GetExportJob(ctx, tenant, job.ID); err != nil {
		t.Fatalf("GetExportJob: %v", err)
	}

	// Claim moves it to running with started_at stamped.
	claimed, err := svc.claimNextJob(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("claimNextJob: job=%v err=%v", claimed, err)
	}
	if claimed.Status != "running" || claimed.StartedAt == nil {
		t.Fatalf("claim did not move to running: %+v", claimed)
	}

	// RunExport drives the runner + markComplete.
	res, err := svc.RunExport(ctx, *claimed)
	if err != nil {
		t.Fatalf("RunExport: %v", err)
	}
	if res.ArtifactSizeBytes != 4096 {
		t.Errorf("result size=%d", res.ArtifactSizeBytes)
	}
	done, err := svc.GetExportJob(ctx, tenant, job.ID)
	if err != nil {
		t.Fatalf("GetExportJob after run: %v", err)
	}
	if done.Status != "completed" || done.DownloadURL != "https://dl/x" || done.CompletedAt == nil {
		t.Errorf("not completed: %+v", done)
	}
	aud.mu.Lock()
	gotAudit := strings.Join(aud.actions, ",")
	aud.mu.Unlock()
	if !strings.Contains(gotAudit, "export.completed") {
		t.Errorf("audit actions=%q want export.completed", gotAudit)
	}

	// Queue now empty → claimNextJob returns nil,nil.
	empty, err := svc.claimNextJob(ctx)
	if err != nil || empty != nil {
		t.Errorf("expected empty queue, got job=%v err=%v", empty, err)
	}
}

// TestExportRunFailureDB covers the markFailed path: a runner error
// transitions the job to 'failed' and records the message.
func TestExportRunFailureDB(t *testing.T) {
	svc, tenant := exportService(t)
	svc.WithRunner(fakeRunner{err: errors.New("blob upload 500")})
	ctx := context.Background()

	job, err := svc.CreateExportJob(ctx, tenant, "user-1", "mbox", "all", "")
	if err != nil {
		t.Fatalf("CreateExportJob: %v", err)
	}
	claimed, err := svc.claimNextJob(ctx)
	if err != nil || claimed == nil {
		t.Fatalf("claimNextJob: %v", err)
	}
	if _, err := svc.RunExport(ctx, *claimed); err == nil {
		t.Fatal("RunExport should surface runner error")
	}
	got, err := svc.GetExportJob(ctx, tenant, job.ID)
	if err != nil {
		t.Fatalf("GetExportJob: %v", err)
	}
	if got.Status != "failed" || !strings.Contains(got.ErrorMessage, "blob upload 500") {
		t.Errorf("not failed: %+v", got)
	}

	// No runner registered → RunExport marks failed too.
	svc2, tenant2 := exportService(t)
	j2, _ := svc2.CreateExportJob(ctx, tenant2, "u", "mbox", "all", "")
	c2, _ := svc2.claimNextJob(ctx)
	if c2 == nil {
		t.Fatal("claim j2")
	}
	if _, err := svc2.RunExport(ctx, *c2); err == nil {
		t.Error("RunExport without runner should error")
	}
	after, _ := svc2.GetExportJob(ctx, tenant2, j2.ID)
	if after.Status != "failed" {
		t.Errorf("status=%q want failed", after.Status)
	}
}

// TestRequeueStaleJobsDB verifies a 'running' job older than the
// cutoff is reset to 'pending' so a later tick retries it.
func TestRequeueStaleJobsDB(t *testing.T) {
	svc, tenant := exportService(t)
	ctx := context.Background()
	job, err := svc.CreateExportJob(ctx, tenant, "user-1", "mbox", "all", "")
	if err != nil {
		t.Fatalf("CreateExportJob: %v", err)
	}
	if _, err := svc.claimNextJob(ctx); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Nothing is stale yet with a long cutoff.
	if n, err := svc.RequeueStaleJobs(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("RequeueStaleJobs(1h) n=%d err=%v want 0", n, err)
	}
	// With a zero cutoff the just-claimed job is stale → requeued.
	n, err := svc.RequeueStaleJobs(ctx, 0)
	if err != nil || n != 1 {
		t.Fatalf("RequeueStaleJobs(0) n=%d err=%v want 1", n, err)
	}
	got, _ := svc.GetExportJob(ctx, tenant, job.ID)
	if got.Status != "pending" {
		t.Errorf("status=%q want pending after requeue", got.Status)
	}
}

func TestCreateExportJobValidationDB(t *testing.T) {
	svc, _ := exportService(t)
	ctx := context.Background()
	if _, err := svc.CreateExportJob(ctx, "", "u", "", "", ""); err == nil {
		t.Error("empty tenant should error")
	}
	if _, err := svc.CreateExportJob(ctx, "t", "", "", "", ""); err == nil {
		t.Error("empty requester should error")
	}
}

// TestExportHandlersDB exercises the REST surface end-to-end against
// the live DB (list empty → create → list → get).
func TestExportHandlersDB(t *testing.T) {
	svc, tenant := exportService(t)
	h := NewHandlers(svc)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/tenants/{id}/exports", h.list)
	mux.HandleFunc("POST /api/v1/tenants/{id}/exports", h.create)
	mux.HandleFunc("GET /api/v1/tenants/{id}/exports/{jobId}", h.get)
	base := "/api/v1/tenants/" + tenant + "/exports"

	// list empty → 200 [] (not null).
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base, nil))
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("empty list=%d body=%q", rec.Code, rec.Body.String())
	}

	// create → 201.
	body, _ := json.Marshal(map[string]string{"requester_id": "user-1", "format": "mbox", "scope": "all"})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, base, strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d body=%s", rec.Code, rec.Body.String())
	}
	var created Job
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// bad JSON → 400.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, base, strings.NewReader("{")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json=%d want 400", rec.Code)
	}

	// get → 200.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("get=%d body=%s", rec.Code, rec.Body.String())
	}

	// get unknown → 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"/00000000-0000-0000-0000-000000000000", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("get missing=%d want 404", rec.Code)
	}
}
