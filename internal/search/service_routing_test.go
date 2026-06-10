package search

import (
	"context"
	"errors"
	"testing"
)

// recordingBackend records the calls routed to it and returns
// configurable results so the Service routing logic can be tested
// without a live Meilisearch/OpenSearch or a DB (nil pool → the
// default backend is shared_meilisearch).
type recordingBackend struct {
	name      string
	indexed   []Message
	migrated  []Message
	deleted   []string
	searchHit []SearchHit
	exported  []Message
	failOn    string // method name to fail
}

func (r *recordingBackend) Name() string { return r.name }

func (r *recordingBackend) IndexMessage(_ context.Context, m Message) error {
	if r.failOn == "index" {
		return errors.New("boom")
	}
	r.indexed = append(r.indexed, m)
	return nil
}

func (r *recordingBackend) SearchMessages(_ context.Context, _, _ string, _ int) ([]SearchHit, error) {
	if r.failOn == "search" {
		return nil, errors.New("boom")
	}
	return r.searchHit, nil
}

func (r *recordingBackend) DeleteIndex(_ context.Context, t string) error {
	if r.failOn == "delete" {
		return errors.New("boom")
	}
	r.deleted = append(r.deleted, t)
	return nil
}

func (r *recordingBackend) MigrateIndex(_ context.Context, _ string, msgs []Message) error {
	if r.failOn == "migrate" {
		return errors.New("boom")
	}
	r.migrated = append(r.migrated, msgs...)
	return nil
}

func (r *recordingBackend) ExportMessages(context.Context, string) ([]Message, error) {
	if r.failOn == "export" {
		return nil, errors.New("boom")
	}
	return r.exported, nil
}

func newRoutingService(t *testing.T, be *recordingBackend) *Service {
	t.Helper()
	// nil pool ⇒ GetBackend resolves to BackendSharedMeilisearch.
	be.name = BackendSharedMeilisearch
	return NewService(Config{Backends: []SearchBackend{be}})
}

func TestServiceRouting_IndexSearchExport(t *testing.T) {
	ctx := context.Background()
	be := &recordingBackend{searchHit: []SearchHit{{MessageID: "m1"}}, exported: []Message{{MessageID: "x1"}}}
	svc := newRoutingService(t, be)

	if err := svc.IndexMessage(ctx, Message{TenantID: "t1", MessageID: "m1"}); err != nil {
		t.Fatalf("IndexMessage: %v", err)
	}
	if len(be.indexed) != 1 {
		t.Errorf("indexed=%d want 1", len(be.indexed))
	}

	hits, err := svc.Search(ctx, "t1", "q", 0) // limit 0 ⇒ clamps to 50
	if err != nil || len(hits) != 1 {
		t.Fatalf("Search=%+v err=%v", hits, err)
	}

	exp, err := svc.Export(ctx, "t1")
	if err != nil || len(exp) != 1 || exp[0].MessageID != "x1" {
		t.Fatalf("Export=%+v err=%v", exp, err)
	}
}

func TestServiceRouting_ReindexWipesThenMigrates(t *testing.T) {
	ctx := context.Background()
	be := &recordingBackend{}
	svc := newRoutingService(t, be)

	msgs := []Message{{TenantID: "t1", MessageID: "a"}, {TenantID: "t1", MessageID: "b"}}
	if err := svc.Reindex(ctx, "t1", msgs); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	if len(be.deleted) != 1 {
		t.Errorf("expected DeleteIndex before migrate, deleted=%v", be.deleted)
	}
	if len(be.migrated) != 2 {
		t.Errorf("migrated=%d want 2", len(be.migrated))
	}

	// Empty reindex still wipes the index but skips MigrateIndex.
	be.deleted = nil
	be.migrated = nil
	if err := svc.Reindex(ctx, "t1", nil); err != nil {
		t.Fatalf("Reindex empty: %v", err)
	}
	if len(be.deleted) != 1 || len(be.migrated) != 0 {
		t.Errorf("empty reindex deleted=%d migrated=%d want 1,0", len(be.deleted), len(be.migrated))
	}
}

func TestServiceRouting_ReindexToValidation(t *testing.T) {
	ctx := context.Background()
	be := &recordingBackend{}
	svc := newRoutingService(t, be)

	// Unrecognised backend value ⇒ ErrInvalidInput.
	if err := svc.ReindexTo(ctx, "t1", "totally-bogus", nil); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ReindexTo bogus=%v want ErrInvalidInput", err)
	}
	// Valid name but not wired into this Service ⇒ ErrBackendUnavailable.
	if err := svc.ReindexTo(ctx, "t1", BackendDedicatedOpenSearch, nil); !errors.Is(err, ErrBackendUnavailable) {
		t.Errorf("ReindexTo unwired=%v want ErrBackendUnavailable", err)
	}
	// Wired destination ⇒ routes to the backend.
	if err := svc.ReindexTo(ctx, "t1", BackendSharedMeilisearch, []Message{{MessageID: "z"}}); err != nil {
		t.Fatalf("ReindexTo wired: %v", err)
	}
	if len(be.migrated) != 1 {
		t.Errorf("migrated=%d want 1", len(be.migrated))
	}
}

func TestServiceRouting_BackendNotConfigured(t *testing.T) {
	ctx := context.Background()
	// No backends registered ⇒ resolved default isn't wired ⇒ ErrNotFound.
	svc := NewService(Config{})
	if err := svc.IndexMessage(ctx, Message{TenantID: "t1", MessageID: "m1"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("IndexMessage no-backend=%v want ErrNotFound", err)
	}
	if _, err := svc.Search(ctx, "t1", "q", 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("Search no-backend=%v want ErrNotFound", err)
	}
	if _, err := svc.Export(ctx, "t1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Export no-backend=%v want ErrNotFound", err)
	}
}

func TestServiceRouting_GetSetBackendNoPool(t *testing.T) {
	ctx := context.Background()
	be := &recordingBackend{}
	svc := newRoutingService(t, be)

	// nil pool ⇒ default backend, no error.
	got, err := svc.GetBackend(ctx, "t1")
	if err != nil || got != BackendSharedMeilisearch {
		t.Fatalf("GetBackend=%q err=%v", got, err)
	}
	// empty tenant ⇒ ErrInvalidInput.
	if _, err := svc.GetBackend(ctx, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("GetBackend empty=%v want ErrInvalidInput", err)
	}
	// SetBackend to a wired value with nil pool returns nil after the guards.
	if err := svc.SetBackend(ctx, "t1", BackendSharedMeilisearch); err != nil {
		t.Errorf("SetBackend wired no-pool=%v", err)
	}
	// SetBackend invalid value ⇒ ErrInvalidInput.
	if err := svc.SetBackend(ctx, "t1", "nope"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("SetBackend invalid=%v want ErrInvalidInput", err)
	}
}
