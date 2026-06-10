package sharedinbox

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPMLSGroupManagerEnsureGroup covers the EnsureGroup POST and
// its error propagation.
func TestHTTPMLSGroupManagerEnsureGroup(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/mls/shared-inbox/groups") {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"group_id": "grp_new"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	m := NewHTTPMLSGroupManager(srv.URL, "tok")
	id, err := m.EnsureGroup(context.Background(), "inbox-1", []string{"u1", "u2"})
	if err != nil || id != "grp_new" {
		t.Fatalf("EnsureGroup = %q, %v", id, err)
	}
	if gotBody["inbox_id"] != "inbox-1" {
		t.Errorf("request body inbox_id = %v", gotBody["inbox_id"])
	}

	// Disabled manager → ("", nil).
	if id, err := NewHTTPMLSGroupManager("", "").EnsureGroup(context.Background(), "i", nil); err != nil || id != "" {
		t.Fatalf("disabled EnsureGroup = %q, %v", id, err)
	}
}

// TestHTTPMLSGroupManagerErrorStatus covers the do() HTTP>=400 branch.
func TestHTTPMLSGroupManagerErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	m := NewHTTPMLSGroupManager(srv.URL, "")
	if _, err := m.EnsureGroup(context.Background(), "i", nil); err == nil {
		t.Fatal("expected HTTP 500 to surface as error")
	}
	if _, err := m.Status(context.Background(), "i"); err == nil {
		t.Fatal("expected Status error on HTTP 500")
	}
}

// TestNoopMLSGroupManagerMethods exercises the no-op manager's
// EnsureGroup + Status.
func TestNoopMLSGroupManagerMethods(t *testing.T) {
	n := NewNoopMLSGroupManager()
	if id, err := n.EnsureGroup(context.Background(), "i", []string{"a"}); err != nil || id != "" {
		t.Fatalf("noop EnsureGroup = %q, %v", id, err)
	}
	st, err := n.Status(context.Background(), "i")
	if err != nil || st.Enabled {
		t.Fatalf("noop Status = %+v, %v", st, err)
	}
}

// TestWorkflowServiceWithMLS pins the chainable setter, including the
// nil-receiver guard.
func TestWorkflowServiceWithMLS(t *testing.T) {
	svc := NewService(nil, log.New(io.Discard, "", 0))
	got := svc.WithMLS(NewNoopMLSGroupManager())
	if got != svc || svc.MLS == nil {
		t.Fatal("WithMLS should set manager and return receiver")
	}
	var nilSvc *WorkflowService
	if nilSvc.WithMLS(NewNoopMLSGroupManager()) != nil {
		t.Error("nil receiver WithMLS should return nil")
	}
}

// fakeMLS lets the handler test drive both the success and error
// branches of mlsStatus when a manager is wired.
type fakeMLS struct {
	status *MLSGroupStatus
	err    error
}

func (f fakeMLS) EnsureGroup(context.Context, string, []string) (string, error) { return "", nil }
func (f fakeMLS) RotateGroup(context.Context, string, []string, string) (string, error) {
	return "", nil
}
func (f fakeMLS) Status(context.Context, string) (*MLSGroupStatus, error) {
	return f.status, f.err
}
func (f fakeMLS) Enabled() bool { return true }

func TestMLSStatusHandlerWithManager(t *testing.T) {
	logger := log.New(io.Discard, "", 0)

	// Success path.
	svc := NewService(nil, logger).WithMLS(fakeMLS{status: &MLSGroupStatus{InboxID: "i1", Epoch: 3, Enabled: true}})
	h := NewHandlers(svc, logger)
	rec := httptest.NewRecorder()
	h.mlsStatus(rec, siReq("tenant-1", http.MethodGet, "", map[string]string{"inboxId": "i1"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("mlsStatus success = %d body=%s", rec.Code, rec.Body.String())
	}

	// Manager error → 502 BadGateway.
	svcErr := NewService(nil, logger).WithMLS(fakeMLS{err: io.ErrUnexpectedEOF})
	hErr := NewHandlers(svcErr, logger)
	rec = httptest.NewRecorder()
	hErr.mlsStatus(rec, siReq("tenant-1", http.MethodGet, "", map[string]string{"inboxId": "i1"}))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("mlsStatus error = %d want 502", rec.Code)
	}
}
