package undosend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// DispatchWorker pops pending sends whose deadline has elapsed
// and forwards them to Stalwart via the JMAP `InternalClient`.
//
// The worker is safe to run with multiple replicas: claim() does
// an atomic ZREM, so the worker that observes `removed > 0` is
// the unique owner. The losers see `removed == 0` and skip the
// id without touching Stalwart.
type DispatchWorker struct {
	svc      *Service
	internal InternalSubmitter
	logger   *log.Logger
	interval time.Duration
	maxBatch int
	maxAttempts int
	now      func() time.Time

	mu sync.Mutex
}

// InternalSubmitter is the slice of the JMAP InternalClient the
// worker depends on. Defined here so the worker test can mock
// the wire path without standing up a full Stalwart double.
type InternalSubmitter interface {
	Dispatch(ctx context.Context, tenantID, kchatUserID string, req jmap.JmapRequest) (*jmap.JmapResponse, error)
}

// WorkerConfig configures a DispatchWorker.
type WorkerConfig struct {
	Service     *Service
	Internal    InternalSubmitter
	Logger      *log.Logger
	// Interval is the poll cadence. Defaults to 1s. Smaller values
	// reduce undo-deadline drift but increase Valkey QPS.
	Interval time.Duration
	// MaxBatch caps how many due ids the worker processes per
	// tick. Defaults to 50.
	MaxBatch int
	// MaxAttempts is the total submission attempt budget per
	// pending send. Defaults to 3. After exhaustion the row is
	// pushed to `kmail:failed_sends`.
	MaxAttempts int
	NowFunc func() time.Time
}

// NewDispatchWorker validates the config and returns a worker.
func NewDispatchWorker(cfg WorkerConfig) (*DispatchWorker, error) {
	if cfg.Service == nil {
		return nil, errors.New("undosend.NewDispatchWorker: Service is required")
	}
	if cfg.Internal == nil {
		return nil, errors.New("undosend.NewDispatchWorker: Internal is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = 1 * time.Second
	}
	maxBatch := cfg.MaxBatch
	if maxBatch <= 0 {
		maxBatch = 50
	}
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	now := cfg.NowFunc
	if now == nil {
		now = time.Now
	}
	return &DispatchWorker{
		svc:         cfg.Service,
		internal:    cfg.Internal,
		logger:      logger,
		interval:    interval,
		maxBatch:    maxBatch,
		maxAttempts: maxAttempts,
		now:         now,
	}, nil
}

// Run loops until ctx is cancelled.
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

// Tick processes one cycle. Exported so tests can drive the
// worker deterministically without standing up a real ticker.
func (w *DispatchWorker) Tick(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()

	ids, err := w.svc.dueIDs(ctx, w.now(), int64(w.maxBatch))
	if err != nil {
		w.logger.Printf("undosend.DispatchWorker: read due ids: %v", err)
		return
	}
	for _, id := range ids {
		w.process(ctx, id)
	}
}

func (w *DispatchWorker) process(ctx context.Context, id string) {
	owned, err := w.svc.claim(ctx, id)
	if err != nil {
		w.logger.Printf("undosend.DispatchWorker: claim id=%s: %v", id, err)
		return
	}
	if !owned {
		// Another worker (or a Cancel call) won the race; nothing
		// to do.
		return
	}
	ps, err := w.svc.readByID(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Companion key gone (cancelled between ZREM and GET,
			// or TTL'd). Nothing to dispatch.
			return
		}
		w.logger.Printf("undosend.DispatchWorker: read id=%s: %v", id, err)
		return
	}
	ps.Attempts++
	if err := w.dispatch(ctx, ps); err != nil {
		w.handleErr(ctx, ps, err)
		return
	}
	if err := w.svc.markDispatched(ctx, ps.ID); err != nil {
		w.logger.Printf("undosend.DispatchWorker: markDispatched id=%s: %v", ps.ID, err)
	}
}

func (w *DispatchWorker) dispatch(ctx context.Context, ps *PendingSend) error {
	create, err := buildSubmissionCreate(ps)
	if err != nil {
		return fmt.Errorf("build submission create: %w", err)
	}
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
					"accountId": ps.StalwartAccountID,
					"create": map[string]any{
						submissionCreateKey(ps): create,
					},
					// Drop the draft on success so the inbox / Drafts
					// mailbox isn't littered with the original message
					// once Stalwart has dispatched it. Mirrors the
					// `onSuccessDestroyEmail` the JMAP client sent in
					// its original (pre-hold) batch.
					"onSuccessDestroyEmail": []any{"#" + submissionCreateKey(ps)},
				},
				"0",
			},
		},
	}
	resp, err := w.internal.Dispatch(ctx, ps.TenantID, ps.KChatUserID, req)
	if err != nil {
		return err
	}
	if methodErr := resp.FirstCallError(); methodErr != nil {
		return methodErr
	}
	name, args, ok := resp.CallByID("0")
	if !ok || name != "EmailSubmission/set" {
		return fmt.Errorf("undosend: dispatched response missing EmailSubmission/set: %v", resp.MethodResponses)
	}
	if notCreated, _ := args["notCreated"].(map[string]any); len(notCreated) > 0 {
		// notCreated is keyed by the create key — surface the
		// first entry's type so the failed-sends list is useful.
		for k, v := range notCreated {
			entry, _ := v.(map[string]any)
			typ, _ := entry["type"].(string)
			desc, _ := entry["description"].(string)
			return fmt.Errorf("stalwart rejected submission %s: %s: %s", k, typ, desc)
		}
	}
	return nil
}

func submissionCreateKey(ps *PendingSend) string {
	if strings.TrimSpace(ps.CreateID) != "" {
		return ps.CreateID
	}
	return "submission"
}

// buildSubmissionCreate is what the proxy hook stores in
// SubmissionPayload. The payload is the *value* side of a JMAP
// create map (i.e. `{"emailId": "...", "identityId": "..."}`).
//
// The hook always normalises back-references (the JMAP `#draft`
// shorthand) before persisting so the worker doesn't need to
// reason about Stalwart's reference-resolution semantics — by the
// time we get here EmailID is the real Stalwart id.
func buildSubmissionCreate(ps *PendingSend) (map[string]any, error) {
	if len(ps.SubmissionPayload) == 0 {
		return nil, errors.New("undosend: empty submission payload")
	}
	var payload map[string]any
	if err := json.Unmarshal(ps.SubmissionPayload, &payload); err != nil {
		return nil, fmt.Errorf("decode submission payload: %w", err)
	}
	// Defensive: replace any leftover back-reference with the real
	// Email id. The proxy hook already does this; we do it again
	// so a buggy upstream caller can't bypass the resolution.
	payload["emailId"] = ps.EmailID
	if strings.TrimSpace(ps.IdentityID) != "" {
		payload["identityId"] = ps.IdentityID
	}
	return payload, nil
}

func (w *DispatchWorker) handleErr(ctx context.Context, ps *PendingSend, dispatchErr error) {
	w.logger.Printf("undosend.DispatchWorker: dispatch id=%s attempt=%d err=%v", ps.ID, ps.Attempts, dispatchErr)
	if ps.Attempts >= w.maxAttempts {
		if err := w.svc.markFailed(ctx, ps, dispatchErr.Error()); err != nil {
			w.logger.Printf("undosend.DispatchWorker: markFailed id=%s: %v", ps.ID, err)
		}
		return
	}
	// Exponential-ish backoff: 5s, 15s. We don't actually need the
	// full Webhook-style 30min ladder here because the user is
	// staring at the undo banner — long retries make the UX worse.
	backoff := time.Duration(5*ps.Attempts*ps.Attempts) * time.Second
	if backoff > 30*time.Second {
		backoff = 30 * time.Second
	}
	next := w.now().Add(backoff)
	if err := w.svc.requeue(ctx, ps, next); err != nil {
		w.logger.Printf("undosend.DispatchWorker: requeue id=%s: %v", ps.ID, err)
	}
}
