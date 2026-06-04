package export

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterValue reads the current value of a Prometheus counter without
// pulling in the testutil helper (and its extra transitive deps).
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("counter.Write: %v", err)
	}
	return m.GetCounter().GetValue()
}

// fakeSource is an in-memory jobSource for worker tests: no Postgres
// required. It lets a test pin the outcome of RunExport and observe
// how the worker accounts for it.
type fakeSource struct {
	claimJob  *Job          // returned by the first claimNextJob call
	claimed   bool          // flips after the first claim
	runResult Result        // returned by RunExport
	runErr    error         // returned by RunExport
	started   chan struct{} // closed when RunExport begins (first call)
	release   chan struct{} // RunExport blocks until this is closed
}

func (f *fakeSource) RequeueStaleJobs(context.Context, time.Duration) (int64, error) {
	return 0, nil
}

func (f *fakeSource) claimNextJob(context.Context) (*Job, error) {
	if f.claimed {
		return nil, nil
	}
	f.claimed = true
	return f.claimJob, nil
}

func (f *fakeSource) RunExport(context.Context, Job) (Result, error) {
	if f.started != nil {
		select {
		case <-f.started:
		default:
			close(f.started)
		}
	}
	if f.release != nil {
		<-f.release
	}
	return f.runResult, f.runErr
}

func silentLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func newCountingWorker(t *testing.T, src jobSource) (*Worker, *Metrics) {
	t.Helper()
	// Use an isolated registry so parallel tests don't clash on the
	// global default registerer.
	m := NewMetrics(prometheus.NewRegistry())
	w := &Worker{svc: src, logger: silentLogger(), interval: time.Hour, parallel: 2, metrics: m}
	return w, m
}

// A run the service abandoned because the job was requeued/re-claimed
// mid-flight (errJobNotCurrent) must be counted as neither completed
// nor failed: the authoritative outcome belongs to the retry. This is
// the worker-side half of the markComplete/markFailed run-fencing fix.
func TestRunOneDiscardsRequeuedRun(t *testing.T) {
	w, m := newCountingWorker(t, &fakeSource{runErr: errJobNotCurrent})
	w.runOne(context.Background(), Job{ID: "j1"})

	if got := counterValue(t, m.JobsCompleted); got != 0 {
		t.Fatalf("JobsCompleted = %v, want 0 for a discarded run", got)
	}
	if got := counterValue(t, m.JobsFailed); got != 0 {
		t.Fatalf("JobsFailed = %v, want 0 for a discarded run", got)
	}
	if got := counterValue(t, m.BytesTotal); got != 0 {
		t.Fatalf("BytesTotal = %v, want 0 for a discarded run", got)
	}
}

func TestRunOneCountsSuccess(t *testing.T) {
	w, m := newCountingWorker(t, &fakeSource{runResult: Result{ArtifactSizeBytes: 4096}})
	w.runOne(context.Background(), Job{ID: "j2"})

	if got := counterValue(t, m.JobsCompleted); got != 1 {
		t.Fatalf("JobsCompleted = %v, want 1", got)
	}
	if got := counterValue(t, m.BytesTotal); got != 4096 {
		t.Fatalf("BytesTotal = %v, want 4096", got)
	}
	if got := counterValue(t, m.JobsFailed); got != 0 {
		t.Fatalf("JobsFailed = %v, want 0", got)
	}
}

func TestRunOneCountsRealFailure(t *testing.T) {
	w, m := newCountingWorker(t, &fakeSource{runErr: io.ErrUnexpectedEOF})
	w.runOne(context.Background(), Job{ID: "j3"})

	if got := counterValue(t, m.JobsFailed); got != 1 {
		t.Fatalf("JobsFailed = %v, want 1", got)
	}
	if got := counterValue(t, m.JobsCompleted); got != 0 {
		t.Fatalf("JobsCompleted = %v, want 0", got)
	}
}

// When every worker slot is occupied by a long-running export, the Run
// loop must still observe ctx cancellation promptly instead of blocking
// indefinitely on the semaphore send. This guards the acquire-before-
// claim / ctx-aware send change.
func TestWorkerShutsDownWhileSlotBusy(t *testing.T) {
	src := &fakeSource{
		claimJob: &Job{ID: "busy"},
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	w := &Worker{svc: src, logger: silentLogger(), interval: time.Millisecond, parallel: 1, staleTimeout: 0}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// Wait until the single slot is occupied by the blocked export.
	select {
	case <-src.started:
	case <-time.After(2 * time.Second):
		t.Fatal("export never started")
	}

	cancel() // request shutdown while the only slot is busy
	select {
	case <-done:
		// Returned promptly despite the slot being held — correct.
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not shut down while its slot was busy")
	}
	close(src.release) // unblock the lingering goroutine
}
