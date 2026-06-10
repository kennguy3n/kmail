package iamcore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/tenant"
)

// stubTenant is an in-memory TenantService recording the calls the
// webhook receiver makes, so handler behaviour (idempotency,
// provision-on-miss, no-op deletes) can be asserted without a DB.
type stubTenant struct {
	mu sync.Mutex

	tenants map[string]*tenant.Tenant            // id -> tenant
	users   map[string]map[string]*tenant.User   // tenantID -> kchatUserID -> user

	ensureCalls int
	createCalls int
	updateCalls int
	deleteCalls int

	createErr error
}

func newStubTenant() *stubTenant {
	return &stubTenant{
		tenants: map[string]*tenant.Tenant{},
		users:   map[string]map[string]*tenant.User{},
	}
}

func (s *stubTenant) EnsureTenant(_ context.Context, in tenant.EnsureTenantInput) (*tenant.Tenant, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCalls++
	if t, ok := s.tenants[in.ID]; ok {
		return t, false, nil
	}
	t := &tenant.Tenant{ID: in.ID, Name: in.Name, Slug: in.Slug, Plan: in.Plan}
	s.tenants[in.ID] = t
	return t, true, nil
}

func (s *stubTenant) CreateUser(_ context.Context, tenantID string, in tenant.CreateUserInput) (*tenant.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	if s.createErr != nil {
		return nil, s.createErr
	}
	if s.users[tenantID] == nil {
		s.users[tenantID] = map[string]*tenant.User{}
	}
	if _, ok := s.users[tenantID][in.KChatUserID]; ok {
		return nil, errors.New("duplicate key value violates unique constraint")
	}
	u := &tenant.User{
		ID:                "row-" + in.KChatUserID,
		TenantID:          tenantID,
		KChatUserID:       in.KChatUserID,
		StalwartAccountID: in.StalwartAccountID,
		Email:             in.Email,
		DisplayName:       in.DisplayName,
	}
	s.users[tenantID][in.KChatUserID] = u
	return u, nil
}

func (s *stubTenant) GetUserByKChatID(_ context.Context, tenantID, kchatUserID string) (*tenant.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.users[tenantID]; m != nil {
		if u, ok := m[kchatUserID]; ok {
			return u, nil
		}
	}
	return nil, tenant.ErrNotFound
}

func (s *stubTenant) UpdateUser(_ context.Context, tenantID, userID string, in tenant.UpdateUserInput) (*tenant.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateCalls++
	if m := s.users[tenantID]; m != nil {
		for _, u := range m {
			if u.ID == userID {
				if in.DisplayName != nil {
					u.DisplayName = *in.DisplayName
				}
				return u, nil
			}
		}
	}
	return nil, tenant.ErrNotFound
}

func (s *stubTenant) DeleteUser(_ context.Context, tenantID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	if m := s.users[tenantID]; m != nil {
		for k, u := range m {
			if u.ID == userID {
				delete(m, k)
				return nil
			}
		}
	}
	return tenant.ErrNotFound
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func mustEvent(t *testing.T, typ string, data any) Event {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	return Event{ID: "evt-1", Type: typ, CreatedAt: time.Now().Unix(), Data: raw}
}

// ------------------------------------------------------------------
// Signature verification
// ------------------------------------------------------------------

func TestVerifySignature_RoundTrip(t *testing.T) {
	rec := NewWebhookReceiver("shh", newStubTenant(), quietLogger())
	body := []byte(`{"hello":"world"}`)
	sig := SignPayload("shh", time.Now(), body)
	if !rec.VerifySignature(sig, body) {
		t.Fatal("valid signature rejected")
	}
}

func TestVerifySignature_Rejections(t *testing.T) {
	rec := NewWebhookReceiver("shh", newStubTenant(), quietLogger())
	body := []byte(`{"a":1}`)
	good := SignPayload("shh", time.Now(), body)

	t.Run("empty header", func(t *testing.T) {
		if rec.VerifySignature("", body) {
			t.Fatal("empty header accepted")
		}
	})
	t.Run("wrong secret", func(t *testing.T) {
		bad := SignPayload("other", time.Now(), body)
		if rec.VerifySignature(bad, body) {
			t.Fatal("signature from wrong secret accepted")
		}
	})
	t.Run("tampered body", func(t *testing.T) {
		if rec.VerifySignature(good, []byte(`{"a":2}`)) {
			t.Fatal("signature accepted over tampered body")
		}
	})
	t.Run("stale timestamp", func(t *testing.T) {
		stale := SignPayload("shh", time.Now().Add(-10*time.Minute), body)
		if rec.VerifySignature(stale, body) {
			t.Fatal("stale signature accepted (replay window not enforced)")
		}
	})
	t.Run("malformed v1 hex", func(t *testing.T) {
		if rec.VerifySignature("t=123,v1=zzzz", body) {
			t.Fatal("non-hex signature accepted")
		}
	})
	t.Run("missing components", func(t *testing.T) {
		if rec.VerifySignature("v1=abcd", body) {
			t.Fatal("header without timestamp accepted")
		}
	})
}

// ------------------------------------------------------------------
// ServeHTTP
// ------------------------------------------------------------------

func postEvent(t *testing.T, rec *WebhookReceiver, secret string, evt Event) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/iam-core", bytes.NewReader(body))
	if secret != "" {
		req.Header.Set(signatureHeader, SignPayload(secret, time.Now(), body))
	}
	w := httptest.NewRecorder()
	rec.ServeHTTP(w, req)
	return w
}

func TestServeHTTP_RejectsBadSignature(t *testing.T) {
	rec := NewWebhookReceiver("shh", newStubTenant(), quietLogger())
	evt := mustEvent(t, EventTenantCreate, TenantEventData{TenantID: "11111111-1111-1111-1111-111111111111"})
	w := postEvent(t, rec, "wrong-secret", evt)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestServeHTTP_UnconfiguredSecretFailsClosed(t *testing.T) {
	rec := NewWebhookReceiver("", newStubTenant(), quietLogger())
	evt := mustEvent(t, EventTenantCreate, TenantEventData{TenantID: "x"})
	w := postEvent(t, rec, "", evt)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestServeHTTP_UnknownEventAcked(t *testing.T) {
	st := newStubTenant()
	rec := NewWebhookReceiver("shh", st, quietLogger())
	evt := mustEvent(t, "billing.updated", map[string]string{"foo": "bar"})
	w := postEvent(t, rec, "shh", evt)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unknown event ACKed)", w.Code)
	}
	if st.ensureCalls != 0 || st.createCalls != 0 {
		t.Fatal("unknown event should not drive provisioning")
	}
}

func TestServeHTTP_TenantCreateProvisions(t *testing.T) {
	st := newStubTenant()
	rec := NewWebhookReceiver("shh", st, quietLogger())
	id := "11111111-1111-1111-1111-111111111111"
	evt := mustEvent(t, EventTenantCreate, TenantEventData{TenantID: id, Name: "Acme", Slug: "acme", Plan: "pro"})
	w := postEvent(t, rec, "shh", evt)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if _, ok := st.tenants[id]; !ok {
		t.Fatal("tenant not provisioned")
	}
}

// ------------------------------------------------------------------
// Dispatch / handler idempotency
// ------------------------------------------------------------------

func TestDispatch_TenantCreateIdempotent(t *testing.T) {
	st := newStubTenant()
	rec := NewWebhookReceiver("shh", st, quietLogger())
	id := "22222222-2222-2222-2222-222222222222"
	evt := mustEvent(t, EventTenantCreate, TenantEventData{TenantID: id, Slug: "dup"})

	for i := 0; i < 3; i++ {
		if err := rec.Dispatch(context.Background(), evt); err != nil {
			t.Fatalf("dispatch #%d: %v", i, err)
		}
	}
	if st.ensureCalls != 3 {
		t.Errorf("ensureCalls = %d, want 3", st.ensureCalls)
	}
	if len(st.tenants) != 1 {
		t.Errorf("tenants = %d, want 1 (idempotent)", len(st.tenants))
	}
}

func TestDispatch_TenantCreateRequiresID(t *testing.T) {
	rec := NewWebhookReceiver("shh", newStubTenant(), quietLogger())
	evt := mustEvent(t, EventTenantCreate, TenantEventData{Slug: "noid"})
	if err := rec.Dispatch(context.Background(), evt); err == nil {
		t.Fatal("expected error when tenant_id missing")
	}
}

func TestDispatch_UserCreateIdempotent(t *testing.T) {
	st := newStubTenant()
	rec := NewWebhookReceiver("shh", st, quietLogger())
	evt := mustEvent(t, EventUserCreate, UserEventData{
		TenantID: "t1", UserID: "u1", Email: "a@b.com", DisplayName: "A",
	})
	for i := 0; i < 2; i++ {
		if err := rec.Dispatch(context.Background(), evt); err != nil {
			t.Fatalf("dispatch #%d: %v", i, err)
		}
	}
	if got := len(st.users["t1"]); got != 1 {
		t.Errorf("users = %d, want 1 (duplicate create is no-op)", got)
	}
}

func TestDispatch_UserUpdateProvisionsOnMiss(t *testing.T) {
	st := newStubTenant()
	rec := NewWebhookReceiver("shh", st, quietLogger())
	evt := mustEvent(t, EventUserUpdate, UserEventData{
		TenantID: "t1", UserID: "u1", Email: "a@b.com", DisplayName: "A",
	})
	if err := rec.Dispatch(context.Background(), evt); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, ok := st.users["t1"]["u1"]; !ok {
		t.Fatal("user.update for unknown user should provision it")
	}
}

func TestDispatch_UserUpdateMutatesDisplayName(t *testing.T) {
	st := newStubTenant()
	rec := NewWebhookReceiver("shh", st, quietLogger())
	// Seed via create.
	create := mustEvent(t, EventUserCreate, UserEventData{TenantID: "t1", UserID: "u1", Email: "a@b.com", DisplayName: "Old"})
	if err := rec.Dispatch(context.Background(), create); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	update := mustEvent(t, EventUserUpdate, UserEventData{TenantID: "t1", UserID: "u1", DisplayName: "New"})
	if err := rec.Dispatch(context.Background(), update); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := st.users["t1"]["u1"].DisplayName; got != "New" {
		t.Errorf("display name = %q, want New", got)
	}
}

func TestDispatch_UserDeleteUnknownIsNoOp(t *testing.T) {
	st := newStubTenant()
	rec := NewWebhookReceiver("shh", st, quietLogger())
	evt := mustEvent(t, EventUserDelete, UserEventData{TenantID: "t1", UserID: "ghost"})
	if err := rec.Dispatch(context.Background(), evt); err != nil {
		t.Fatalf("delete unknown: %v", err)
	}
}

func TestDispatch_UserDeleteRemoves(t *testing.T) {
	st := newStubTenant()
	rec := NewWebhookReceiver("shh", st, quietLogger())
	create := mustEvent(t, EventUserCreate, UserEventData{TenantID: "t1", UserID: "u1", Email: "a@b.com", DisplayName: "A"})
	if err := rec.Dispatch(context.Background(), create); err != nil {
		t.Fatalf("seed: %v", err)
	}
	del := mustEvent(t, EventUserDelete, UserEventData{TenantID: "t1", UserID: "u1"})
	if err := rec.Dispatch(context.Background(), del); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := st.users["t1"]["u1"]; ok {
		t.Fatal("user not deleted")
	}
}

func TestDispatch_UserEventRequiresIDs(t *testing.T) {
	rec := NewWebhookReceiver("shh", newStubTenant(), quietLogger())
	for _, typ := range []string{EventUserCreate, EventUserUpdate, EventUserDelete} {
		evt := mustEvent(t, typ, UserEventData{TenantID: "t1"}) // no user_id
		if err := rec.Dispatch(context.Background(), evt); err == nil {
			t.Errorf("%s: expected error when user_id missing", typ)
		}
	}
}

// ------------------------------------------------------------------
// Enrichment via Management API
// ------------------------------------------------------------------

func TestEnrichUser_BackfillsFromManagementAPI(t *testing.T) {
	srv, _ := newTestServer(t)
	client := newTestClient(t, srv.URL)
	st := newStubTenant()
	rec := NewWebhookReceiver("shh", st, quietLogger()).WithClient(client)

	// Sparse event: only ids, no email/name. enrichUser should fill
	// them from the mock Management API (ada@acme.com / Ada Lovelace).
	evt := mustEvent(t, EventUserCreate, UserEventData{TenantID: "tenant-a", UserID: "user-1"})
	if err := rec.Dispatch(context.Background(), evt); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	u := st.users["tenant-a"]["user-1"]
	if u == nil {
		t.Fatal("user not provisioned")
	}
	if u.Email != "ada@acme.com" {
		t.Errorf("email = %q, want backfilled ada@acme.com", u.Email)
	}
	if u.DisplayName != "Ada Lovelace" {
		t.Errorf("display = %q, want backfilled Ada Lovelace", u.DisplayName)
	}
}

func TestEnrichUser_EventFieldsWinOverAPI(t *testing.T) {
	srv, _ := newTestServer(t)
	client := newTestClient(t, srv.URL)
	rec := NewWebhookReceiver("shh", newStubTenant(), quietLogger()).WithClient(client)

	d := rec.enrichUser(context.Background(), UserEventData{
		TenantID: "tenant-a", UserID: "user-1",
		Email: "override@x.com", DisplayName: "Override",
	})
	// Both fields already present → no API call, values unchanged.
	if d.Email != "override@x.com" || d.DisplayName != "Override" {
		t.Errorf("event fields should win: %+v", d)
	}
}
