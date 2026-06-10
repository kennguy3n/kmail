package export

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

// TestWorkerRunDrivesRealServiceDB wires a real *Service into NewWorker
// (exercising the production constructor + option setters) and lets the
// Run loop claim and complete a seeded job end-to-end.
func TestWorkerRunDrivesRealServiceDB(t *testing.T) {
	svc, tenant := exportService(t)
	svc.WithRunner(fakeRunner{res: Result{
		DownloadURL: "https://dl/y", ArtifactSizeBytes: 10, MessageIDs: []string{"m1"},
	}})
	ctx := context.Background()
	job, err := svc.CreateExportJob(ctx, tenant, "user-1", "mbox", "all", "")
	if err != nil {
		t.Fatalf("CreateExportJob: %v", err)
	}

	w := NewWorker(svc, log.New(io.Discard, "", 0)).
		WithInterval(5 * time.Millisecond).
		WithParallel(1).
		WithMetrics(NewMetrics(nil)).
		WithStaleTimeout(time.Hour)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()

	// Poll until the worker has completed the job.
	deadline := time.After(3 * time.Second)
	for {
		got, err := svc.GetExportJob(ctx, tenant, job.ID)
		if err == nil && got.Status == "completed" {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatalf("job not completed in time: status=%v err=%v", statusOf(got), err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker Run did not stop after cancel")
	}
}

func statusOf(j *Job) string {
	if j == nil {
		return "<nil>"
	}
	return j.Status
}
