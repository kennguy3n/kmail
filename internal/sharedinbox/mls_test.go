package sharedinbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
