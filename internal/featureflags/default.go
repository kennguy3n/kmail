package featureflags

import (
	"context"
	"sync/atomic"
)

// defaultService holds the process-wide Service so call sites can use
// the ergonomic package-level featureflags.IsEnabled(ctx, "...") form
// the WS4 plan specifies, without threading a *Service through every
// layer. cmd/kmail-api installs it once at startup via SetDefault.
//
// It is an atomic.Pointer so SetDefault during startup races safely
// with any early IsEnabled call (there should be none, but the atomic
// makes the contract explicit and the race detector happy).
var defaultService atomic.Pointer[Service]

// SetDefault installs the process-wide Service used by the package-
// level IsEnabled. Passing nil clears it (used by tests for cleanup).
func SetDefault(s *Service) {
	defaultService.Store(s)
}

// Default returns the installed process-wide Service, or nil if none
// has been set.
func Default() *Service {
	return defaultService.Load()
}

// IsEnabled resolves a flag against the process-wide Service for the
// subject in ctx. When no Service has been installed (e.g. a binary
// that never wired feature flags, or a unit test) it returns false —
// the same fail-safe answer an unregistered flag gets — so a missing
// wiring can never silently enable a gated feature.
func IsEnabled(ctx context.Context, key string) bool {
	s := defaultService.Load()
	if s == nil {
		return false
	}
	return s.IsEnabled(ctx, key)
}
