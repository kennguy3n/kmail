package sharedinbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGroupManager records RotateGroup invocations so the
// membership-change dispatch can be asserted without an HTTP round
// trip. `enabled` and `rotateErr` drive the skip / failure paths.
type fakeGroupManager struct {
	enabled     bool
	rotateErr   error
	rotateCalls []rotateCall
}

type rotateCall struct {
	inboxID string
	members []string
	reason  string
}

func (f *fakeGroupManager) EnsureGroup(_ context.Context, inboxID string, members []string) (string, error) {
	return "grp", nil
}

func (f *fakeGroupManager) RotateGroup(_ context.Context, inboxID string, members []string, reason string) (string, error) {
	f.rotateCalls = append(f.rotateCalls, rotateCall{inboxID, members, reason})
	if f.rotateErr != nil {
		return "", f.rotateErr
	}
	return "grp", nil
}

func (f *fakeGroupManager) Status(_ context.Context, inboxID string) (*MLSGroupStatus, error) {
	return &MLSGroupStatus{InboxID: inboxID, Enabled: f.enabled}, nil
}

func (f *fakeGroupManager) Enabled() bool { return f.enabled }

// discardLogger keeps test output quiet while still exercising the
// log.Printf calls in HandleMembershipChange.
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestHandleMembershipChange_RotatesOnAddAndRemove(t *testing.T) {
	mgr := &fakeGroupManager{enabled: true}
	s := NewService(nil, discardLogger()).WithMLS(mgr)

	s.HandleMembershipChange(context.Background(), "inbox-1", []string{"u1", "u2"}, "member_added")
	s.HandleMembershipChange(context.Background(), "inbox-1", []string{"u1"}, "member_removed")

	if len(mgr.rotateCalls) != 2 {
		t.Fatalf("expected RotateGroup called on both add and remove, got %d calls", len(mgr.rotateCalls))
	}
	if mgr.rotateCalls[0].reason != "member_added" || mgr.rotateCalls[1].reason != "member_removed" {
		t.Errorf("rotation reasons not propagated: %+v", mgr.rotateCalls)
	}
	if len(mgr.rotateCalls[0].members) != 2 || len(mgr.rotateCalls[1].members) != 1 {
		t.Errorf("post-mutation member set not propagated: %+v", mgr.rotateCalls)
	}
}

func TestHandleMembershipChange_SkipsWhenDisabled(t *testing.T) {
	mgr := &fakeGroupManager{enabled: false}
	s := NewService(nil, discardLogger()).WithMLS(mgr)
	s.HandleMembershipChange(context.Background(), "inbox-1", []string{"u1"}, "member_added")
	if len(mgr.rotateCalls) != 0 {
		t.Errorf("disabled manager must not rotate, got %d calls", len(mgr.rotateCalls))
	}
}

func TestHandleMembershipChange_NilManagerIsNoop(t *testing.T) {
	s := NewService(nil, discardLogger()) // no MLS attached
	// Must not panic.
	s.HandleMembershipChange(context.Background(), "inbox-1", []string{"u1"}, "member_added")
}

func TestHandleMembershipChange_SwallowsRotateError(t *testing.T) {
	mgr := &fakeGroupManager{enabled: true, rotateErr: errors.New("kchat 500")}
	s := NewService(nil, discardLogger()).WithMLS(mgr)
	// Best-effort: a rotation failure must NOT propagate (failing
	// the membership change because KChat is down is worse than
	// briefly running on the prior epoch).
	s.HandleMembershipChange(context.Background(), "inbox-1", []string{"u1"}, "member_removed")
	if len(mgr.rotateCalls) != 1 {
		t.Errorf("expected one rotate attempt even on error, got %d", len(mgr.rotateCalls))
	}
}

func TestNoopMLSGroupManagerEnabledIsFalse(t *testing.T) {
	m := NewNoopMLSGroupManager()
	if m.Enabled() {
		t.Fatal("noop manager should not be enabled")
	}
	id, err := m.RotateGroup(context.Background(), "i1", []string{"u1"}, "test")
	if err != nil || id != "" {
		t.Fatalf("RotateGroup = %q, %v", id, err)
	}
	st, err := m.Status(context.Background(), "i1")
	if err != nil || st.Enabled {
		t.Fatalf("Status = %+v, err=%v", st, err)
	}
}

func TestHTTPMLSGroupManagerRotateAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rotate"):
			_ = json.NewEncoder(w).Encode(map[string]string{"group_id": "grp_42"})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"group_id":     "grp_42",
				"epoch":        7,
				"member_count": 3,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	m := NewHTTPMLSGroupManager(srv.URL, "tok")
	if !m.Enabled() {
		t.Fatal("expected enabled")
	}
	id, err := m.RotateGroup(context.Background(), "i1", []string{"u1", "u2"}, "test")
	if err != nil || id != "grp_42" {
		t.Fatalf("RotateGroup = %q, %v", id, err)
	}
	st, err := m.Status(context.Background(), "i1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Epoch != 7 || st.MemberCount != 3 {
		t.Fatalf("status = %+v", st)
	}
}

func TestHTTPMLSGroupManagerEmptyEndpointDisabled(t *testing.T) {
	m := NewHTTPMLSGroupManager("", "")
	if m.Enabled() {
		t.Fatal("empty endpoint should disable manager")
	}
	if id, err := m.RotateGroup(context.Background(), "i1", nil, "test"); err != nil || id != "" {
		t.Fatalf("got id=%q err=%v", id, err)
	}
}
