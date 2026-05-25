package scheduledsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// workerStore is the slice of the Service the worker depends on.
// Defined as an interface so tests can inject an in-memory fake
// without standing up a real Postgres pool. The exported Service
// satisfies the interface implicitly via its claim/mark/retry
// methods.
type workerStore interface {
	claimDue(ctx context.Context) (*ScheduledSend, error)
	markDispatched(ctx context.Context, id string, sentAt time.Time) error
	markFailed(ctx context.Context, id, lastErr string) error
	scheduleRetry(ctx context.Context, id string, nextRetryAt time.Time, lastErr string) error
}

// DispatchWorker dispatches due `scheduled_sends` rows to
// Stalwart via the JMAP `InternalClient`.
//
// One worker per BFF replica is sufficient. The claim path uses
// `SELECT … FOR UPDATE SKIP LOCKED`, so running N replicas in
// parallel does not double-dispatch a row: each replica's
// transaction picks a different unlocked row and holds the lock
// for the duration of the dispatch.
//
// Compare with `undosend.DispatchWorker`: that worker reads from
// a Valkey sorted set with a 1-second tick because Undo Send is
// latency-sensitive (the user is staring at the countdown). The
// scheduled-send tick is 15s — plenty for "send tomorrow at 9am"
// granularity and keeps the worker's DB QPS bounded even on a
// large fleet.
type DispatchWorker struct {
	store       workerStore
	internal    InternalSubmitter
	logger      *log.Logger
	interval    time.Duration
	maxBatch    int
	maxAttempts int
	now         func() time.Time
}

// InternalSubmitter is the slice of the JMAP InternalClient the
// worker depends on. Defined here so the worker test can mock
// the wire path without standing up a Stalwart double.
type InternalSubmitter interface {
	Dispatch(ctx context.Context, tenantID, kchatUserID string, req jmap.JmapRequest) (*jmap.JmapResponse, error)
}

// WorkerConfig configures a DispatchWorker.
type WorkerConfig struct {
	Service  *Service
	Internal InternalSubmitter
	Logger   *log.Logger
	// Interval is the poll cadence. Defaults to 15s. Lower
	// values reduce dispatch latency but increase DB QPS.
	Interval time.Duration
	// MaxBatch caps how many rows the worker processes per tick.
	// Defaults to 50.
	MaxBatch int
	// MaxAttempts is the per-row retry budget. Defaults to
	// DefaultMaxAttempts. Once exhausted the row flips to
	// `failed` and is left for operator inspection.
	MaxAttempts int
	NowFunc     func() time.Time
}

// NewDispatchWorker validates the config and returns a worker.
func NewDispatchWorker(cfg WorkerConfig) (*DispatchWorker, error) {
	if cfg.Service == nil {
		return nil, errors.New("scheduledsend.NewDispatchWorker: Service is required")
	}
	return newDispatchWorkerWithStore(cfg, cfg.Service)
}

// newDispatchWorkerWithStore is the test seam: lets the package's
// own test files inject an in-memory `workerStore` instead of a
// real `*Service`-backed Postgres.
func newDispatchWorkerWithStore(cfg WorkerConfig, store workerStore) (*DispatchWorker, error) {
	if store == nil {
		return nil, errors.New("scheduledsend.newDispatchWorkerWithStore: store is required")
	}
	if cfg.Internal == nil {
		return nil, errors.New("scheduledsend.NewDispatchWorker: Internal is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	maxBatch := cfg.MaxBatch
	if maxBatch <= 0 {
		maxBatch = 50
	}
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	now := cfg.NowFunc
	if now == nil {
		now = time.Now
	}
	return &DispatchWorker{
		store:       store,
		internal:    cfg.Internal,
		logger:      logger,
		interval:    interval,
		maxBatch:    maxBatch,
		maxAttempts: maxAttempts,
		now:         now,
	}, nil
}

// Run ticks every `interval` until ctx is cancelled. Mirrors the
// existing `webhooks.Worker.Run` shape.
func (w *DispatchWorker) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick processes one cycle. Drains up to `maxBatch` rows.
// Exported so tests can drive the worker deterministically.
func (w *DispatchWorker) Tick(ctx context.Context) {
	for i := 0; i < w.maxBatch; i++ {
		more, err := w.dispatchOne(ctx)
		if err != nil {
			w.logger.Printf("scheduledsend.DispatchWorker: tick: %v", err)
			return
		}
		if !more {
			return
		}
	}
}

// dispatchOne claims the next due row, dispatches it, and updates
// the row's state. Returns (more, err) — `more` is true when the
// caller should continue draining the queue this tick.
func (w *DispatchWorker) dispatchOne(ctx context.Context) (bool, error) {
	ss, err := w.store.claimDue(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("claim due: %w", err)
	}

	if err := w.dispatch(ctx, ss); err != nil {
		w.handleErr(ctx, ss, err)
		return true, nil
	}
	if err := w.store.markDispatched(ctx, ss.ID, w.now()); err != nil {
		w.logger.Printf("scheduledsend.DispatchWorker: markDispatched id=%s: %v", ss.ID, err)
	}
	return true, nil
}

func (w *DispatchWorker) dispatch(ctx context.Context, ss *ScheduledSend) error {
	create, err := buildSubmissionCreate(ss)
	if err != nil {
		return fmt.Errorf("build submission create: %w", err)
	}
	createKey := submissionCreateKey(ss)
	req := jmap.JmapRequest{
		Using: []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:mail",
			"urn:ietf:params:jmap:submission",
		},
		MethodCalls: [][]any{
			{
				"EmailSubmission/set",
				map[string]any{
					"accountId": ss.StalwartAccountID,
					"create": map[string]any{
						createKey: create,
					},
					// Drop the draft now that Stalwart owns it.
					// Mirrors the `onSuccessDestroyEmail` the
					// JMAP client sends in its normal Compose
					// batch.
					"onSuccessDestroyEmail": []any{"#" + createKey},
				},
				"0",
			},
		},
	}
	resp, err := w.internal.Dispatch(ctx, ss.TenantID, ss.KChatUserID, req)
	if err != nil {
		return err
	}
	if methodErr := resp.FirstCallError(); methodErr != nil {
		return methodErr
	}
	name, args, ok := resp.CallByID("0")
	if !ok || name != "EmailSubmission/set" {
		return fmt.Errorf("scheduledsend: missing EmailSubmission/set in response")
	}
	if notCreated, _ := args["notCreated"].(map[string]any); len(notCreated) > 0 {
		for k, v := range notCreated {
			entry, _ := v.(map[string]any)
			typ, _ := entry["type"].(string)
			desc, _ := entry["description"].(string)
			return fmt.Errorf("stalwart rejected submission %s: %s: %s", k, typ, desc)
		}
	}
	return nil
}

func (w *DispatchWorker) handleErr(ctx context.Context, ss *ScheduledSend, dispatchErr error) {
	w.logger.Printf("scheduledsend.DispatchWorker: dispatch id=%s attempt=%d err=%v", ss.ID, ss.Attempts, dispatchErr)
	// `Attempts` was bumped inside claimDue, so ss.Attempts
	// already counts the in-progress attempt.
	if ss.Attempts >= w.maxAttempts {
		if err := w.store.markFailed(ctx, ss.ID, dispatchErr.Error()); err != nil {
			w.logger.Printf("scheduledsend.DispatchWorker: markFailed id=%s: %v", ss.ID, err)
		}
		return
	}
	// Exponential backoff: 1m, 5m, 30m, 30m, ... The first retry
	// is intentionally aggressive so a flaky Stalwart blip is
	// recovered before the user notices; later retries back off
	// to keep the worker from hammering a sustained outage.
	backoffs := []time.Duration{
		1 * time.Minute,
		5 * time.Minute,
		30 * time.Minute,
		30 * time.Minute,
		30 * time.Minute,
	}
	idx := ss.Attempts
	if idx >= len(backoffs) {
		idx = len(backoffs) - 1
	}
	next := w.now().Add(backoffs[idx])
	if err := w.store.scheduleRetry(ctx, ss.ID, next, dispatchErr.Error()); err != nil {
		w.logger.Printf("scheduledsend.DispatchWorker: scheduleRetry id=%s: %v", ss.ID, err)
	}
}

// submissionCreateKey returns the create-key the worker uses when
// re-dispatching the JMAP submission. We could persist this on
// the row but every supported caller uses "submission" today, so
// the constant keeps the payload smaller and the schema simpler.
// If a future caller needs a different key we can add a column
// without a migration on the worker.
func submissionCreateKey(_ *ScheduledSend) string {
	return "submission"
}

// buildSubmissionCreate parses the persisted submission payload
// and substitutes the resolved Stalwart Email id into the
// payload's `emailId` field. The proxy hook always normalises
// back-references (the JMAP `#draft` shorthand) before persisting
// so the worker doesn't need to reason about Stalwart's
// reference resolution.
func buildSubmissionCreate(ss *ScheduledSend) (map[string]any, error) {
	if len(ss.SubmissionPayload) == 0 {
		return nil, errors.New("scheduledsend: empty submission payload")
	}
	var payload map[string]any
	if err := json.Unmarshal(ss.SubmissionPayload, &payload); err != nil {
		return nil, fmt.Errorf("decode submission payload: %w", err)
	}
	payload["emailId"] = ss.EmailID
	if strings.TrimSpace(ss.IdentityID) != "" {
		payload["identityId"] = ss.IdentityID
	}
	return payload, nil
}
