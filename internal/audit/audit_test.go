package audit

import (
	"strings"
	"testing"
	"time"
)

func TestComputeHash_Deterministic(t *testing.T) {
	e := Entry{
		TenantID:     "t1",
		ActorID:      "u1",
		ActorType:    ActorUser,
		Action:       "tenant.update",
		ResourceType: "tenant",
		ResourceID:   "t1",
		Metadata:     map[string]any{"b": 2, "a": 1},
	}
	a := computeHash("", e)
	b := computeHash("", e)
	if a != b {
		t.Errorf("non-deterministic hash: %s vs %s", a, b)
	}
	if !strings.HasPrefix(a, "") || len(a) != 64 {
		t.Errorf("unexpected hash length: %d", len(a))
	}
}

func TestComputeHash_ChainsOnPrev(t *testing.T) {
	e := Entry{TenantID: "t1", ActorType: ActorAdmin, Action: "user.create"}
	first := computeHash("", e)
	second := computeHash(first, e)
	if first == second {
		t.Error("chain hash did not change with prev")
	}
}

func TestCanonicalJSON_KeyOrderInsensitive(t *testing.T) {
	m1 := map[string]any{"a": 1, "b": 2, "c": 3}
	m2 := map[string]any{"c": 3, "b": 2, "a": 1}
	if canonicalJSON(m1) != canonicalJSON(m2) {
		t.Error("canonicalJSON should be key-order-insensitive")
	}
}

func TestLog_ValidatesRequiredFields(t *testing.T) {
	svc := NewService(nil) // nil pool → in-memory stub path
	if _, err := svc.Log(t.Context(), Entry{TenantID: "", Action: "x", ActorType: ActorAdmin}); err == nil {
		t.Error("expected error on missing tenantID")
	}
	if _, err := svc.Log(t.Context(), Entry{TenantID: "t1", Action: "", ActorType: ActorAdmin}); err == nil {
		t.Error("expected error on missing action")
	}
	if _, err := svc.Log(t.Context(), Entry{TenantID: "t1", Action: "x", ActorType: ""}); err == nil {
		t.Error("expected error on missing actorType")
	}
}

func TestLog_InMemory_ComputesHash(t *testing.T) {
	svc := NewService(nil)
	out, err := svc.Log(t.Context(), Entry{
		TenantID: "t1", ActorID: "u1", ActorType: ActorUser,
		Action: "tenant.update", ResourceType: "tenant", ResourceID: "t1",
	})
	if err != nil {
		t.Fatalf("Log: %v", err)
	}
	if len(out.EntryHash) != 64 {
		t.Errorf("expected 64-hex hash, got %q", out.EntryHash)
	}
	if out.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}
}

func TestExport_UnknownFormat(t *testing.T) {
	svc := NewService(nil)
	_, err := svc.Export(t.Context(), "t1", "xml", time.Time{}, time.Time{})
	if err == nil {
		t.Error("expected error for unknown format")
	}
}
