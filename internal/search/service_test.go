package search

import (
	"context"
	"errors"
	"testing"
)

// stubBackend is a minimal SearchBackend used to register names
// with a Service in tests that exercise the availability / gating
// logic. The methods are no-ops because the gate is purely a
// map-lookup decision — none of these need to be invoked for the
// tests in this file.
type stubBackend struct {
	name string
}

func (s *stubBackend) Name() string { return s.name }
func (s *stubBackend) IndexMessage(context.Context, Message) error {
	return nil
}
func (s *stubBackend) SearchMessages(context.Context, string, string, int) ([]SearchHit, error) {
	return nil, nil
}
func (s *stubBackend) DeleteIndex(context.Context, string) error { return nil }
func (s *stubBackend) MigrateIndex(context.Context, string, []Message) error {
	return nil
}
func (s *stubBackend) ExportMessages(context.Context, string) ([]Message, error) {
	return nil, nil
}

// TestService_AvailableBackends_ReturnsSortedRegisteredNames
// pins the listAvailableSearchBackends contract used by the
// admin UI gating logic. The Service exposes only the names
// whose Go implementations are wired in, sorted alphabetically
// so the UI render order is stable across BFF restarts.
func TestService_AvailableBackends_ReturnsSortedRegisteredNames(t *testing.T) {
	svc := NewService(Config{Backends: []SearchBackend{
		&stubBackend{name: BackendSharedOpenSearch},
		&stubBackend{name: BackendMeilisearch},
		&stubBackend{name: BackendSharedMeilisearch},
	}})
	got := svc.AvailableBackends()
	want := []string{
		BackendMeilisearch,
		BackendSharedMeilisearch,
		BackendSharedOpenSearch,
	}
	if len(got) != len(want) {
		t.Fatalf("AvailableBackends = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AvailableBackends[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestService_IsBackendAvailable distinguishes the wired-backends
// set from the IsValidBackend syntactic check. `dedicated_opensearch`
// is valid (the CHECK constraint admits it) but not implemented; without
// the gate, a SetBackend onto it would silently succeed and every
// subsequent search would 404.
func TestService_IsBackendAvailable(t *testing.T) {
	svc := NewService(Config{Backends: []SearchBackend{
		&stubBackend{name: BackendSharedMeilisearch},
	}})
	if !svc.IsBackendAvailable(BackendSharedMeilisearch) {
		t.Error("registered backend reported unavailable")
	}
	if svc.IsBackendAvailable(BackendDedicatedOpenSearch) {
		t.Error("unregistered backend reported available")
	}
	if !IsValidBackend(BackendDedicatedOpenSearch) {
		t.Error("dedicated_opensearch is a recognised tenants.search_backend value — IsValidBackend should still return true")
	}
}

// TestService_SetBackend_RejectsUnwiredBackend is the regression
// test for the dedicated_opensearch finding on PR #48 commit
// 6015fea. The admin UI exposes the value as a card; without
// this gate, picking it would flip `tenants.search_backend` to
// a value that no backend in this BFF can serve, and every read
// would 404 with a generic message. The gate fails the SetBackend
// itself with a distinct ErrBackendUnavailable sentinel so the
// admin UI can show a clear "not available in this deployment"
// hint.
func TestService_SetBackend_RejectsUnwiredBackend(t *testing.T) {
	svc := NewService(Config{Backends: []SearchBackend{
		&stubBackend{name: BackendSharedMeilisearch},
	}})
	// pool is nil (metadata-only mode) — the gate must still fire.
	err := svc.SetBackend(context.Background(), "tenant-x", BackendDedicatedOpenSearch)
	if err == nil {
		t.Fatal("SetBackend with unwired backend returned nil; expected ErrBackendUnavailable")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("SetBackend err = %v, want wrapped ErrBackendUnavailable", err)
	}
}

// TestService_ReindexTo_RejectsUnwiredBackend mirrors the
// SetBackend regression for the bulk-import path: the cutover
// worker calls ReindexTo with the destination backend name, and
// a typo or missing impl must surface as ErrBackendUnavailable
// rather than a silent no-op (the auto-promotion worker would
// otherwise mark the row Completed and the destination would be
// empty).
func TestService_ReindexTo_RejectsUnwiredBackend(t *testing.T) {
	svc := NewService(Config{Backends: []SearchBackend{
		&stubBackend{name: BackendSharedMeilisearch},
	}})
	err := svc.ReindexTo(context.Background(), "tenant-x", BackendDedicatedOpenSearch, nil)
	if err == nil {
		t.Fatal("ReindexTo with unwired backend returned nil; expected ErrBackendUnavailable")
	}
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("ReindexTo err = %v, want wrapped ErrBackendUnavailable", err)
	}
}

// TestService_SetBackend_AcceptsWiredBackend_NoPool covers the
// metadata-only happy path: with a nil pool and a registered
// backend, SetBackend short-circuits to a no-op success after
// the gate passes. This is the shape used by handler-level unit
// tests that don't want to spin up a real database.
func TestService_SetBackend_AcceptsWiredBackend_NoPool(t *testing.T) {
	svc := NewService(Config{Backends: []SearchBackend{
		&stubBackend{name: BackendSharedMeilisearch},
	}})
	if err := svc.SetBackend(context.Background(), "tenant-x", BackendSharedMeilisearch); err != nil {
		t.Errorf("SetBackend with wired backend returned %v, want nil", err)
	}
}
