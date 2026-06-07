package migration

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

func newDBService(t *testing.T) (*Service, string) {
	t.Helper()
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	return NewService(Config{Pool: pool}), tenant
}

func TestMigrationCreateGetListDB(t *testing.T) {
	svc, tenant := newDBService(t)
	ctx := context.Background()

	// validation
	if _, err := svc.CreateJob(ctx, tenant, CreateJobInput{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty input: want ErrInvalidInput got %v", err)
	}

	job, err := svc.CreateJob(ctx, tenant, CreateJobInput{
		SourceHost: "imap.old.example.com", SourceUser: "alice",
		SourcePassword: "secret", DestUser: "alice@new.example.com",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if job.Status != "pending" || job.ProgressPct != 0 {
		t.Errorf("new job state wrong: %+v", job)
	}
	// password is encrypted at rest and never the cleartext
	if job.SourcePasswordEncrypted == "secret" || job.SourcePasswordEncrypted == "" {
		t.Errorf("password not encrypted: %q", job.SourcePasswordEncrypted)
	}

	got, err := svc.GetJob(ctx, tenant, job.ID)
	if err != nil || got.ID != job.ID {
		t.Fatalf("GetJob: %v %+v", err, got)
	}
	if _, err := svc.GetJob(ctx, tenant, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetJob missing: want ErrNotFound got %v", err)
	}

	jobs, err := svc.ListJobs(ctx, tenant)
	if err != nil {
		t.Fatalf("ListJobs err=%v", err)
	}
	found := false
	for _, j := range jobs {
		if j.ID == job.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListJobs did not include created job (got %d jobs)", len(jobs))
	}
}

func TestMigrationPauseCancelPendingDB(t *testing.T) {
	svc, tenant := newDBService(t)
	ctx := context.Background()

	mk := func() string {
		j, err := svc.CreateJob(ctx, tenant, CreateJobInput{
			SourceHost: "h", SourceUser: "u", SourcePassword: "p", DestUser: "d@x.com",
		})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		return j.ID
	}

	// Pause a pending job → paused.
	p := mk()
	if err := svc.PauseJob(ctx, tenant, p); err != nil {
		t.Fatalf("PauseJob: %v", err)
	}
	if got, _ := svc.GetJob(ctx, tenant, p); got.Status != "paused" {
		t.Errorf("status=%q want paused", got.Status)
	}

	// Cancel a pending job → cancelled (terminal).
	c := mk()
	if err := svc.CancelJob(ctx, tenant, c); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	got, _ := svc.GetJob(ctx, tenant, c)
	if got.Status != "cancelled" || !got.Terminal() {
		t.Errorf("status=%q terminal=%v want cancelled/terminal", got.Status, got.Terminal())
	}
	// Cancelling a terminal job conflicts.
	if err := svc.CancelJob(ctx, tenant, c); !errors.Is(err, ErrConflict) {
		t.Errorf("cancel terminal: want ErrConflict got %v", err)
	}

	// Resume a non-paused (pending) job is rejected.
	r := mk()
	if err := svc.ResumeJob(ctx, tenant, r); !errors.Is(err, ErrConflict) {
		t.Errorf("resume pending: want ErrConflict got %v", err)
	}
}
