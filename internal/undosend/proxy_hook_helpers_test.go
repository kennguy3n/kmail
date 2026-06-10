package undosend

import (
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/jmap"
)

func TestCopyStringMap(t *testing.T) {
	if copyStringMap(nil) != nil {
		t.Error("nil map should copy to nil")
	}
	in := map[string]string{"a": "1", "b": "2"}
	out := copyStringMap(in)
	if len(out) != 2 || out["a"] != "1" || out["b"] != "2" {
		t.Fatalf("copy mismatch: %v", out)
	}
	out["a"] = "mutated"
	if in["a"] != "1" {
		t.Error("copy should be independent of source")
	}
}

func TestParseJMAPRequest(t *testing.T) {
	if _, err := parseJMAPRequest([]byte("{not json")); err == nil {
		t.Error("malformed JSON should error")
	}
	jr, err := parseJMAPRequest([]byte(`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[]}`))
	if err != nil {
		t.Fatalf("valid request: %v", err)
	}
	if len(jr.Using) != 1 {
		t.Errorf("using = %v", jr.Using)
	}
}

func TestWriteJMAPResponse(t *testing.T) {
	w := httptest.NewRecorder()
	resp := &jmap.JmapResponse{MethodResponses: [][]any{}, SessionState: "s1"}
	ok, err := writeJMAPResponse(w, resp, 200)
	if !ok || err != nil {
		t.Fatalf("writeJMAPResponse = %v, %v", ok, err)
	}
	if w.Code != 200 {
		t.Errorf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
}
