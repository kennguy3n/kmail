package search

import (
	"context"
	"testing"
	"time"
)

// TestCutoverWorker_RunLoopStopsOnCancel drives the long-running
// Run loop (immediate tick + ticker) with startup jitter disabled
// so it's deterministic, and asserts it returns promptly on
// context cancel. The tenant is below threshold so the loop has no
// side effects — we're exercising the loop plumbing, not migration.
func TestCutoverWorker_RunLoopStopsOnCancel(t *testing.T) {
	store := newInMemoryStore([]string{"tenant-a"})
	flipper := newFakeFlipper(store)
	now := time.Unix(1_700_000_000, 0)
	sizer := MailboxSizerFunc(func(context.Context, string) (int64, error) { return 1024, nil })
	source := MessageSourceFunc(func(context.Context, string) ([]Message, error) { return nil, nil })

	cfg := CutoverConfig{
		Store:       store,
		Service:     flipper,
		Sizer:       sizer,
		Source:      source,
		Logger:      silentLogger(),
		Threshold:   100_000,
		Interval:    5 * time.Millisecond,
		MaxFailures: 5,
		MaxRetryGap: time.Hour,
		Now:         func() time.Time { return now },
		Sleep:       func(time.Duration) {},
	}
	DisableStartupJitter(&cfg)
	w, err := NewCutoverWorker(cfg)
	if err != nil {
		t.Fatalf("NewCutoverWorker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	// Let the immediate tick plus a few ticker ticks fire.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}

	if got := flipper.setBackendCall["tenant-a"]; got != "" {
		t.Errorf("below-threshold tenant unexpectedly flipped: %q", got)
	}
}
