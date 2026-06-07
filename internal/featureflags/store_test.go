package featureflags

import (
	"context"
	"testing"
	"time"
)

// TestStoreReadContextAppliesDeadline verifies the control-plane read
// path is bounded: a Store with a positive readTimeout derives a
// context with a deadline (so a stalled Postgres fails fast), while a
// non-positive timeout opts out and returns the caller's context
// unchanged.
func TestStoreReadContextAppliesDeadline(t *testing.T) {
	t.Run("bounded", func(t *testing.T) {
		s := &Store{readTimeout: 50 * time.Millisecond}
		ctx, cancel := s.readContext(context.Background())
		defer cancel()
		dl, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected a deadline when readTimeout > 0")
		}
		if d := time.Until(dl); d <= 0 || d > 50*time.Millisecond+time.Second {
			t.Fatalf("deadline %v not within the configured budget", d)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		s := &Store{readTimeout: 0}
		ctx, cancel := s.readContext(context.Background())
		defer cancel()
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("expected no deadline when readTimeout <= 0")
		}
	})
}

// TestNewStoreDefaultsReadTimeout guards the out-of-the-box behaviour:
// the production constructor must install a finite read budget so a
// caller that never tunes it is still protected from a hanging DB.
func TestNewStoreDefaultsReadTimeout(t *testing.T) {
	s := NewStore(nil)
	if s.readTimeout != defaultReadTimeout {
		t.Fatalf("readTimeout = %v, want default %v", s.readTimeout, defaultReadTimeout)
	}
	if got := s.WithReadTimeout(2 * time.Second); got.readTimeout != 2*time.Second {
		t.Fatalf("WithReadTimeout did not apply: %v", got.readTimeout)
	}
}
