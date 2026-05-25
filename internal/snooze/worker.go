package snooze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// InternalSubmitter is the subset of `jmap.InternalClient` the
// worker uses. The full client implements `Dispatch`; this
// interface keeps the worker testable with an in-memory stub.
type InternalSubmitter interface {
	Dispatch(ctx context.Context, tenantID, kchatUserID string, req jmap.JmapRequest) (*jmap.JmapResponse, error)
}

// workerStore is the slice of Service that the worker needs.
// Tests inject an in-memory fake so the worker can be exercised
// without a real Postgres pool. Mirrors
// `scheduledsend.workerStore` for symmetry.
type workerStore interface {
	claimDue(ctx context.Context) (*Snooze, error)
	markUnsnoozed(ctx context.Context, id string, wokenAt time.Time) error
	markFailed(ctx context.Context, id, lastErr string) error
	scheduleRetry(ctx context.Context, id string, nextRetryAt time.Time, lastErr string) error
}

// DispatchWorker polls the snooze queue and wakes due rows by
// patching the email's `mailboxIds` back to the original set.
type DispatchWorker struct {
	store       workerStore
	internal    InternalSubmitter
	logger      *log.Logger
	interval    time.Duration
	maxBatch    int
	maxAttempts int
	now         func() time.Time
}

// WorkerConfig configures DispatchWorker. `Service` and
// `Internal` are mandatory.
type WorkerConfig struct {
	Service     *Service
	Internal    InternalSubmitter
	Logger      *log.Logger
	Interval    time.Duration
	MaxBatch    int
	MaxAttempts int
	NowFunc     func() time.Time
}

const (
	// DefaultInterval is the worker poll interval. 30s mirrors
	// the plan — snoozes are usually hours/days/weeks, so a
	// 30-second resolution is fine and keeps the worker idle
	// most of the time.
	DefaultInterval = 30 * time.Second

	// DefaultMaxBatch caps how many rows one tick may dispatch.
	// 25 keeps a single replica from monopolising the queue if
	// a backlog builds up; remaining rows are picked up next
	// tick by the same replica or a sibling.
	DefaultMaxBatch = 25
)

// NewDispatchWorker validates the config and constructs a
// DispatchWorker bound to a real Service.
func NewDispatchWorker(cfg WorkerConfig) (*DispatchWorker, error) {
	if cfg.Service == nil {
		return nil, errors.New("snooze.NewDispatchWorker: Service is required")
	}
	return newDispatchWorkerWithStore(cfg, cfg.Service)
}

// newDispatchWorkerWithStore is the inner constructor: the
// store dependency is abstract so tests can pass an in-memory
// fake. Same pattern as scheduledsend.newDispatchWorkerWithStore.
func newDispatchWorkerWithStore(cfg WorkerConfig, store workerStore) (*DispatchWorker, error) {
	if store == nil {
		return nil, errors.New("snooze.newDispatchWorker: store is required")
	}
	if cfg.Internal == nil {
		return nil, errors.New("snooze.NewDispatchWorker: Internal is required")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = log.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	maxBatch := cfg.MaxBatch
	if maxBatch <= 0 {
		maxBatch = DefaultMaxBatch
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

// Run drives the worker until ctx is cancelled. Each tick drains
// up to MaxBatch rows. Errors per row are logged + retried; the
// worker never exits except via ctx cancellation.
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

// Tick drains up to MaxBatch due rows. Exposed for tests so we
// can step through without spinning up the ticker.
func (w *DispatchWorker) Tick(ctx context.Context) {
	for i := 0; i < w.maxBatch; i++ {
		if err := w.dispatchOne(ctx); err != nil {
			if errors.Is(err, ErrNotFound) {
				return // queue drained
			}
			w.logger.Printf("snooze worker tick error: %v", err)
			return
		}
	}
}

func (w *DispatchWorker) dispatchOne(ctx context.Context) error {
	row, err := w.store.claimDue(ctx)
	if err != nil {
		return err
	}
	if row == nil {
		return ErrNotFound
	}
	if err := w.dispatch(ctx, row); err != nil {
		return w.handleErr(ctx, row, err)
	}
	if err := w.store.markUnsnoozed(ctx, row.ID, w.now()); err != nil {
		return fmt.Errorf("snooze: mark unsnoozed: %w", err)
	}
	return nil
}

// dispatch builds the JMAP `Email/set update` patch that
// restores the email's original mailboxIds (and optionally
// clears the seen flag so the email re-surfaces as new) and
// dispatches it via the internal client.
//
// We patch BOTH mailboxIds (removing the snoozed folder, adding
// back the originals) AND optionally keywords.$seen in a single
// Email/set call — JMAP guarantees the operations are atomic
// against the per-email lock so the user never sees an
// intermediate state where the email is in both the snoozed
// folder and the original.
func (w *DispatchWorker) dispatch(ctx context.Context, ss *Snooze) error {
	patch, err := buildWakePatch(ss)
	if err != nil {
		return err
	}
	req := jmap.JmapRequest{
		Using: []string{
			"urn:ietf:params:jmap:core",
			"urn:ietf:params:jmap:mail",
		},
		MethodCalls: [][]any{
			{
				"Email/set",
				map[string]any{
					"accountId": ss.StalwartAccountID,
					"update": map[string]any{
						ss.EmailID: patch,
					},
				},
				"wake",
			},
		},
	}
	resp, err := w.internal.Dispatch(ctx, ss.TenantID, ss.KChatUserID, req)
	if err != nil {
		return fmt.Errorf("dispatch wake: %w", err)
	}
	if resp == nil || len(resp.MethodResponses) == 0 {
		return errors.New("snooze: empty wake response")
	}
	name, args, ok := resp.CallByID("wake")
	if !ok {
		return errors.New("snooze: wake response missing client id")
	}
	if name != "Email/set" {
		return fmt.Errorf("snooze: unexpected wake response method %q", name)
	}
	// Inspect the response body: a NotUpdated entry on the
	// patched id means Stalwart refused the mailbox move (e.g.
	// the original mailbox has been deleted in the meantime).
	// Treat that as a terminal failure on the snooze row — the
	// email itself is fine, but we can't reverse the snooze
	// automatically.
	if notUpdated, ok := args["notUpdated"].(map[string]any); ok {
		if reason, ok := notUpdated[ss.EmailID]; ok {
			return fmt.Errorf("snooze: wake refused by stalwart: %v", reason)
		}
	}
	return nil
}

// buildWakePatch encodes the `Email/set update` patch that
// restores the email's pre-snooze mailboxIds and (optionally)
// clears `$seen`.
//
// JMAP patch syntax uses null to unset a mailbox membership and
// true to add one. We always remove the snoozed mailbox membership
// and re-add the original ones; the union is the correct outcome
// even if the snoozed mailbox was somehow also in the original
// set (which it shouldn't be).
func buildWakePatch(ss *Snooze) (map[string]any, error) {
	var orig map[string]bool
	if err := json.Unmarshal(ss.OriginalMailboxIDs, &orig); err != nil {
		return nil, fmt.Errorf("snooze: parse original mailbox ids: %w", err)
	}
	if len(orig) == 0 {
		return nil, errors.New("snooze: empty original mailbox ids")
	}
	patch := make(map[string]any, len(orig)+2)
	// Drop snoozed mailbox membership.
	patch["mailboxIds/"+ss.SnoozedMailboxID] = nil
	// Restore each original mailbox membership.
	for mb := range orig {
		if mb == "" {
			continue
		}
		patch["mailboxIds/"+mb] = true
	}
	if ss.MarkUnreadOnWake {
		// Clear the seen flag so the email shows as new on
		// re-surface. The patch uses the JMAP keyword syntax.
		patch["keywords/$seen"] = nil
	}
	return patch, nil
}

// handleErr decides between marking a row failed (terminal),
// scheduling a retry (transient), or surfacing an unrelated DB
// error. Exponential backoff mirrors `scheduledsend.handleErr`:
// 1m → 5m → 30m → 30m → 30m, capped at MaxAttempts.
func (w *DispatchWorker) handleErr(ctx context.Context, ss *Snooze, dispatchErr error) error {
	w.logger.Printf("snooze: wake error id=%s attempt=%d err=%v", ss.ID, ss.Attempts, dispatchErr)
	if ss.Attempts >= w.maxAttempts {
		if err := w.store.markFailed(ctx, ss.ID, dispatchErr.Error()); err != nil {
			return fmt.Errorf("snooze: mark failed: %w", err)
		}
		return nil
	}
	backoff := snoozeBackoff(ss.Attempts)
	next := w.now().Add(backoff)
	if err := w.store.scheduleRetry(ctx, ss.ID, next, dispatchErr.Error()); err != nil {
		return fmt.Errorf("snooze: schedule retry: %w", err)
	}
	return nil
}

func snoozeBackoff(attempts int) time.Duration {
	// attempts is the count POST-increment (see claimDue),
	// so attempts=1 is the first failure.
	switch {
	case attempts <= 1:
		return 1 * time.Minute
	case attempts == 2:
		return 5 * time.Minute
	default:
		return 30 * time.Minute
	}
}
