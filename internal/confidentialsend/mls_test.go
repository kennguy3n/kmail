package confidentialsend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockDeriver is an in-memory MLSKeyDeriver for exercising the
// service wiring without an HTTP round-trip.
type mockDeriver struct {
	wrapKey    string
	rekeyKey   string
	err        error
	enabled    bool
	wrapCalls  []wrapCall
	rekeyCalls []rekeyCall
}

type wrapCall struct {
	senderLeafKey       string
	recipientCredential string
}

type rekeyCall struct {
	messageID    string
	participants []string
}

func (m *mockDeriver) DeriveWrappingKey(_ context.Context, senderLeafKey, recipientCredential string) (string, error) {
	m.wrapCalls = append(m.wrapCalls, wrapCall{senderLeafKey, recipientCredential})
	if m.err != nil {
		return "", m.err
	}
	return m.wrapKey, nil
}

func (m *mockDeriver) RekeyConfidentialMessage(_ context.Context, messageID string, participants []string) (string, error) {
	m.rekeyCalls = append(m.rekeyCalls, rekeyCall{messageID, participants})
	if m.err != nil {
		return "", m.err
	}
	return m.rekeyKey, nil
}

// enabledMockService wires a mock deriver that always reports
// enabled (the *HTTPKeyDeriver special-case in MLSEnabled does not
// apply to a non-HTTP deriver, so any non-nil deriver is enabled).
func enabledMockService(d MLSKeyDeriver) *Service {
	return NewService(nil).WithMLS(d)
}

func TestResolveCreateWrapping_LinkOnlyWhenNoMLSInputs(t *testing.T) {
	d := &mockDeriver{wrapKey: "deadbeef"}
	s := enabledMockService(d)
	wr, err := s.resolveCreateWrapping(context.Background(), CreateRequest{
		TenantID: "t", SenderID: "s", EncryptedBlobRef: "ref",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wr.wrapped {
		t.Error("expected link-only (not wrapped) when no MLS inputs supplied")
	}
	if len(d.wrapCalls) != 0 {
		t.Errorf("deriver should not be called for a link-only send, got %d calls", len(d.wrapCalls))
	}
}

func TestResolveCreateWrapping_PartialRequest(t *testing.T) {
	s := enabledMockService(&mockDeriver{wrapKey: "k"})
	cases := []CreateRequest{
		{SenderLeafKey: "leaf"},              // recipients missing
		{Recipients: []string{"bob@x.test"}}, // leaf missing
	}
	for i, req := range cases {
		if _, err := s.resolveCreateWrapping(context.Background(), req); !errors.Is(err, ErrMLSPartialRequest) {
			t.Errorf("case %d: expected ErrMLSPartialRequest, got %v", i, err)
		}
	}
}

func TestResolveCreateWrapping_RequestedButMLSDisabled(t *testing.T) {
	s := NewService(nil) // no deriver wired => MLS disabled
	_, err := s.resolveCreateWrapping(context.Background(), CreateRequest{
		SenderLeafKey: "leaf", Recipients: []string{"bob@x.test"},
	})
	if !errors.Is(err, ErrMLSDisabled) {
		t.Fatalf("expected ErrMLSDisabled when MLS requested but unconfigured, got %v", err)
	}
}

func TestResolveCreateWrapping_DerivesKey(t *testing.T) {
	d := &mockDeriver{wrapKey: "ab12cd34"}
	s := enabledMockService(d)
	wr, err := s.resolveCreateWrapping(context.Background(), CreateRequest{
		SenderLeafKey: "leaf-xyz",
		Recipients:    []string{"bob@x.test", "carol@x.test"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !wr.wrapped || wr.wrappingKey != "ab12cd34" {
		t.Fatalf("expected wrapped with key, got %+v", wr)
	}
	if wr.senderLeaf != "leaf-xyz" {
		t.Errorf("sender leaf not retained: %q", wr.senderLeaf)
	}
	if len(wr.participants) != 2 {
		t.Errorf("participant set not retained: %v", wr.participants)
	}
	// Wrapping targets the FIRST recipient credential.
	if len(d.wrapCalls) != 1 || d.wrapCalls[0].recipientCredential != "bob@x.test" {
		t.Errorf("expected single wrap call targeting first recipient, got %+v", d.wrapCalls)
	}
}

func TestResolveCreateWrapping_DeriveErrorPropagates(t *testing.T) {
	s := enabledMockService(&mockDeriver{err: errors.New("mls boom")})
	_, err := s.resolveCreateWrapping(context.Background(), CreateRequest{
		SenderLeafKey: "leaf", Recipients: []string{"bob@x.test"},
	})
	if err == nil {
		t.Fatal("expected derive error to propagate")
	}
}

// mockMLSServer stands in for the KChat MLS credential service so
// the HTTP deriver can be integration-tested end-to-end.
func mockMLSServer(t *testing.T, wantToken, wrapKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); wantToken != "" && got != "Bearer "+wantToken {
			t.Errorf("auth header = %q, want %q", got, "Bearer "+wantToken)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/mls/wrap":
			var in map[string]string
			if err := json.Unmarshal(body, &in); err != nil {
				t.Errorf("wrap body: %v", err)
			}
			if in["sender_leaf_key"] == "" || in["recipient_credential"] == "" {
				http.Error(w, "missing fields", http.StatusBadRequest)
				return
			}
		case "/mls/rekey":
			var in map[string]any
			if err := json.Unmarshal(body, &in); err != nil {
				t.Errorf("rekey body: %v", err)
			}
			if in["message_id"] == "" {
				http.Error(w, "missing message_id", http.StatusBadRequest)
				return
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"wrapping_key": wrapKey})
	}))
}

func TestHTTPKeyDeriver_WrapAndRekey(t *testing.T) {
	srv := mockMLSServer(t, "secret-token", "fd00fd00")
	defer srv.Close()
	d := NewHTTPKeyDeriver(srv.URL, "secret-token")

	if !d.Enabled() {
		t.Fatal("expected deriver enabled with a configured endpoint")
	}
	key, err := d.DeriveWrappingKey(context.Background(), "leaf", "bob@x.test")
	if err != nil {
		t.Fatalf("DeriveWrappingKey: %v", err)
	}
	if key != "fd00fd00" {
		t.Errorf("wrap key = %q, want fd00fd00", key)
	}
	rekey, err := d.RekeyConfidentialMessage(context.Background(), "msg-1", []string{"bob@x.test", "carol@x.test"})
	if err != nil {
		t.Fatalf("RekeyConfidentialMessage: %v", err)
	}
	if rekey != "fd00fd00" {
		t.Errorf("rekey key = %q, want fd00fd00", rekey)
	}
}

func TestHTTPKeyDeriver_Disabled(t *testing.T) {
	d := NewHTTPKeyDeriver("", "")
	if d.Enabled() {
		t.Fatal("empty endpoint must be disabled")
	}
	if _, err := d.DeriveWrappingKey(context.Background(), "leaf", "bob"); !errors.Is(err, ErrMLSDisabled) {
		t.Errorf("expected ErrMLSDisabled, got %v", err)
	}
	if _, err := d.RekeyConfidentialMessage(context.Background(), "m", []string{"bob"}); !errors.Is(err, ErrMLSDisabled) {
		t.Errorf("expected ErrMLSDisabled on rekey, got %v", err)
	}
}

func TestHTTPKeyDeriver_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusBadGateway)
	}))
	defer srv.Close()
	d := NewHTTPKeyDeriver(srv.URL, "")
	if _, err := d.DeriveWrappingKey(context.Background(), "leaf", "bob"); err == nil {
		t.Fatal("expected error on non-200 MLS response")
	}
}

func TestHTTPKeyDeriver_EmptyKeyRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"wrapping_key": ""})
	}))
	defer srv.Close()
	d := NewHTTPKeyDeriver(srv.URL, "")
	if _, err := d.DeriveWrappingKey(context.Background(), "leaf", "bob"); err == nil {
		t.Fatal("expected error when MLS returns an empty wrapping_key")
	}
}

func TestHTTPKeyDeriver_RequiresInputs(t *testing.T) {
	srv := mockMLSServer(t, "", "key")
	defer srv.Close()
	d := NewHTTPKeyDeriver(srv.URL, "")
	if _, err := d.DeriveWrappingKey(context.Background(), "", "bob"); err == nil {
		t.Error("expected error with empty sender leaf key")
	}
	if _, err := d.RekeyConfidentialMessage(context.Background(), "", []string{"bob"}); err == nil {
		t.Error("expected error with empty message id")
	}
}

func TestService_RekeyConfidentialMessage_Validation(t *testing.T) {
	// MLS disabled.
	if _, err := NewService(nil).RekeyConfidentialMessage(context.Background(), "t", "l", []string{"bob"}); !errors.Is(err, ErrMLSDisabled) {
		t.Fatalf("expected ErrMLSDisabled, got %v", err)
	}
	s := enabledMockService(&mockDeriver{rekeyKey: "k"})
	// Missing identifiers.
	if _, err := s.RekeyConfidentialMessage(context.Background(), "", "l", []string{"bob"}); err == nil {
		t.Error("expected error on empty tenant")
	}
	if _, err := s.RekeyConfidentialMessage(context.Background(), "t", "", []string{"bob"}); err == nil {
		t.Error("expected error on empty link id")
	}
	// Empty participant set.
	if _, err := s.RekeyConfidentialMessage(context.Background(), "t", "l", nil); err == nil {
		t.Error("expected error on empty participants")
	}
	// Valid args but no pool configured (DB persistence step).
	if _, err := s.RekeyConfidentialMessage(context.Background(), "t", "l", []string{"bob"}); err == nil {
		t.Error("expected pool-not-configured error")
	}
}

func TestMLSEnabled(t *testing.T) {
	if NewService(nil).MLSEnabled() {
		t.Error("service without deriver must report MLS disabled")
	}
	if !enabledMockService(&mockDeriver{}).MLSEnabled() {
		t.Error("service with non-HTTP deriver must report MLS enabled")
	}
	if NewService(nil).WithMLS(NewHTTPKeyDeriver("", "")).MLSEnabled() {
		t.Error("service with disabled HTTP deriver must report MLS disabled")
	}
}
