package migration

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// stubImapsync swaps the package-level runImapsyncCmd with a fake that
// shells out to `sh -c <script>` so runWorker/runImapsync run their
// real scan + cmd.Wait paths deterministically (no real imapsync
// binary, no network). Returns a restore func.
func stubImapsync(t *testing.T, script string) {
	t.Helper()
	orig := runImapsyncCmd
	runImapsyncCmd = func(ctx context.Context, _ string, _ *MigrationJob, _ string) (io.ReadCloser, *exec.Cmd, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return stdout, cmd, nil
	}
	t.Cleanup(func() { runImapsyncCmd = orig })
}

func migrationRouter(svc *Service) *http.ServeMux {
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/migrations", h.createJob)
	mux.HandleFunc("POST /api/v1/migrations/test-connection", h.testConnection)
	mux.HandleFunc("GET /api/v1/migrations", h.listJobs)
	mux.HandleFunc("GET /api/v1/migrations/{jobId}", h.getJob)
	mux.HandleFunc("DELETE /api/v1/migrations/{jobId}", h.cancelJob)
	mux.HandleFunc("POST /api/v1/migrations/{jobId}/pause", h.pauseJob)
	mux.HandleFunc("POST /api/v1/migrations/{jobId}/resume", h.resumeJob)
	return mux
}

func authedReq(method, target, tenant, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	return r.WithContext(middleware.WithTenantID(r.Context(), tenant))
}

// TestMigrationHandlersCRUDDB drives the full REST surface against a
// live DB: create (which kicks the worker), list, get, then cancel.
func TestMigrationHandlersCRUDDB(t *testing.T) {
	stubImapsync(t, "printf '++++ Statistics : Folder [INBOX] Messages 10 of 10 done\\n'; exit 0")
	svc, tenant := newDBService(t)
	mux := migrationRouter(svc)

	// create → 202 (job persisted + StartJob fired).
	body, _ := json.Marshal(CreateJobInput{
		SourceHost: "imap.old.example.com", SourceUser: "alice",
		SourcePassword: "secret", DestUser: "alice@new.example.com",
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodPost, "/api/v1/migrations", tenant, string(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create=%d body=%s", rec.Code, rec.Body.String())
	}
	var created MigrationJob
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	if created.SourcePasswordEncrypted != "" {
		t.Error("create response must not leak the encrypted password")
	}

	// list → 200 containing the job.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodGet, "/api/v1/migrations", tenant, ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.ID) {
		t.Fatalf("list=%d body=%s", rec.Code, rec.Body.String())
	}

	// Wait for the worker to reach the terminal completed state.
	deadline := time.After(3 * time.Second)
	for {
		got, err := svc.GetJob(context.Background(), tenant, created.ID)
		if err == nil && got.Terminal() {
			if got.Status != "completed" {
				t.Fatalf("worker terminal status=%q want completed", got.Status)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not complete in time")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// get → 200.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodGet, "/api/v1/migrations/"+created.ID, tenant, ""))
	if rec.Code != http.StatusOK {
		t.Errorf("get=%d body=%s", rec.Code, rec.Body.String())
	}

	// cancel a completed (terminal) job → 409 Conflict.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodDelete, "/api/v1/migrations/"+created.ID, tenant, ""))
	if rec.Code != http.StatusConflict {
		t.Errorf("cancel terminal=%d want 409", rec.Code)
	}
}

// TestMigrationHandlerGuardsDB covers the auth/validation guard paths.
func TestMigrationHandlerGuardsDB(t *testing.T) {
	svc, tenant := newDBService(t)
	mux := migrationRouter(svc)

	// missing tenant context → 403 on every route.
	for _, tc := range []struct{ method, target string }{
		{http.MethodPost, "/api/v1/migrations"},
		{http.MethodGet, "/api/v1/migrations"},
		{http.MethodGet, "/api/v1/migrations/x"},
		{http.MethodDelete, "/api/v1/migrations/x"},
		{http.MethodPost, "/api/v1/migrations/x/pause"},
		{http.MethodPost, "/api/v1/migrations/x/resume"},
		{http.MethodPost, "/api/v1/migrations/test-connection"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(tc.method, tc.target, strings.NewReader("{}"))
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s no-tenant=%d want 403", tc.method, tc.target, rec.Code)
		}
	}

	// bad JSON → 400.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodPost, "/api/v1/migrations", tenant, "{"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json create=%d want 400", rec.Code)
	}

	// missing required fields → 400 (ErrInvalidInput).
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodPost, "/api/v1/migrations", tenant, `{"source_host":"h"}`))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("incomplete create=%d want 400", rec.Code)
	}

	// get unknown job → 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodGet, "/api/v1/migrations/00000000-0000-0000-0000-000000000000", tenant, ""))
	if rec.Code != http.StatusNotFound {
		t.Errorf("get missing=%d want 404", rec.Code)
	}
}

// TestMigrationTestConnectionHandlerDB drives the test-connection probe
// through the handler: a live fake IMAP server that accepts LOGIN
// yields {"ok":true}; a rejected login yields {"ok":false}.
func TestMigrationTestConnectionHandlerDB(t *testing.T) {
	svc, tenant := newDBService(t)
	mux := migrationRouter(svc)

	srv := newFakeIMAPServer(t, true)
	host, port := srv.addr()
	body, _ := json.Marshal(TestConnectionInput{Host: host, Port: port, Username: "u", Password: "p"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodPost, "/api/v1/migrations/test-connection", tenant, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("test-connection=%d body=%s", rec.Code, rec.Body.String())
	}
	var ok struct {
		OK bool `json:"ok"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ok)
	if !ok.OK {
		t.Errorf("expected ok:true, body=%s", rec.Body.String())
	}

	// Rejected login → still HTTP 200 but ok:false.
	bad := newFakeIMAPServer(t, false)
	bh, bp := bad.addr()
	body, _ = json.Marshal(TestConnectionInput{Host: bh, Port: bp, Username: "u", Password: "p"})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodPost, "/api/v1/migrations/test-connection", tenant, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("rejected test-connection=%d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ok)
	if ok.OK {
		t.Error("expected ok:false for rejected login")
	}
}

// TestMigrationPauseResumeHandlersDB covers the pauseJob/resumeJob
// happy paths (204) and Register mounting all routes on a real mux.
func TestMigrationPauseResumeHandlersDB(t *testing.T) {
	stubImapsync(t, "printf 'Messages 1 of 1 done\\n'; exit 0")
	svc, tenant := newDBService(t)

	// Register on a real mux behind a dev-bypass OIDC to cover Register.
	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{DevBypassToken: "x", Env: middleware.EnvDevelopment})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	regMux := http.NewServeMux()
	NewHandlers(svc, log.New(io.Discard, "", 0)).Register(regMux, authMW)
	rec := httptest.NewRecorder()
	regMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/migrations", nil))
	if rec.Code == http.StatusNotFound {
		t.Fatal("Register did not mount GET /api/v1/migrations")
	}

	mux := migrationRouter(svc)
	job, err := svc.CreateJob(context.Background(), tenant, CreateJobInput{
		SourceHost: "h", SourceUser: "u", SourcePassword: "p", DestUser: "d@x.com",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// pause a pending job → 204.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodPost, "/api/v1/migrations/"+job.ID+"/pause", tenant, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("pause=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, _ := svc.GetJob(context.Background(), tenant, job.ID); got.Status != "paused" {
		t.Fatalf("status=%q want paused", got.Status)
	}

	// resume → 204 (kicks the worker again).
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, authedReq(http.MethodPost, "/api/v1/migrations/"+job.ID+"/resume", tenant, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("resume=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestMigrationStartJobFailureDB covers runWorker's failure branch:
// an imapsync that exits non-zero flips the job to failed with the
// error recorded.
func TestMigrationStartJobFailureDB(t *testing.T) {
	stubImapsync(t, "echo boom >&2; exit 7")
	svc, tenant := newDBService(t)
	ctx := context.Background()
	job, err := svc.CreateJob(ctx, tenant, CreateJobInput{
		SourceHost: "h", SourceUser: "u", SourcePassword: "p", DestUser: "d@x.com",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if err := svc.StartJob(ctx, tenant, job.ID); err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		got, err := svc.GetJob(ctx, tenant, job.ID)
		if err == nil && got.Terminal() {
			if got.Status != "failed" || got.ErrorMsg == nil || *got.ErrorMsg == "" {
				t.Fatalf("want failed+error, got status=%q err=%v", got.Status, got.ErrorMsg)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("worker did not fail in time")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// StartJob on a terminal job → ErrConflict.
	if err := svc.StartJob(ctx, tenant, job.ID); err == nil {
		t.Error("StartJob on terminal job should conflict")
	}
}
