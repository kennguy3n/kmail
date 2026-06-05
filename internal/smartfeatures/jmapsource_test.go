package smartfeatures

import (
	"context"
	"testing"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// fakeDispatcher returns a canned JMAP response and records the
// request it was handed so tests can assert on the requested
// properties.
type fakeDispatcher struct {
	resp    *jmap.JmapResponse
	err     error
	lastReq jmap.JmapRequest
}

func (f *fakeDispatcher) Dispatch(_ context.Context, _, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	f.lastReq = req
	return f.resp, f.err
}

func emailGetResponse(list []any) *jmap.JmapResponse {
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"Email/get", map[string]any{"list": list}, "g0"},
		},
	}
}

func TestJMAPFetcher_ParsesMessage(t *testing.T) {
	fd := &fakeDispatcher{resp: emailGetResponse([]any{
		map[string]any{
			"id":                             "E1",
			"threadId":                       "T1",
			"subject":                        "Hello",
			"preview":                        "world",
			"from":                           []any{map[string]any{"name": "Alice", "email": "alice@example.com"}},
			"to":                             []any{map[string]any{"email": "me@example.com"}},
			"keywords":                       map[string]any{"$seen": true},
			"header:List-Unsubscribe:asText": "<https://x/u>",
		},
	})}
	f := &JMAPFetcher{client: fd}

	got, err := f.FetchMessages(context.Background(), "t1", "u1", []string{"E1"})
	if err != nil {
		t.Fatalf("FetchMessages: %v", err)
	}
	m, ok := got["E1"]
	if !ok {
		t.Fatalf("missing E1 in result %#v", got)
	}
	if m.Subject != "Hello" || m.Preview != "world" {
		t.Fatalf("subject/preview wrong: %#v", m)
	}
	if from, ok := m.FirstFrom(); !ok || from.Email != "alice@example.com" {
		t.Fatalf("from wrong: %#v", m.From)
	}
	if m.Header("List-Unsubscribe") != "<https://x/u>" {
		t.Fatalf("header not normalized: %#v", m.Headers)
	}
	if !m.Keywords["$seen"] {
		t.Fatalf("keywords not parsed: %#v", m.Keywords)
	}
}

func TestJMAPFetcher_QualifiedIDKeyedAsRequested(t *testing.T) {
	// Caller passes an account-qualified id; the bare id is sent to
	// JMAP but the result must be keyed by the original qualified id.
	fd := &fakeDispatcher{resp: emailGetResponse([]any{
		map[string]any{"id": "E1", "subject": "Hi"},
	})}
	f := &JMAPFetcher{client: fd}

	got, err := f.FetchMessages(context.Background(), "t1", "u1", []string{"ACC:E1"})
	if err != nil {
		t.Fatalf("FetchMessages: %v", err)
	}
	if _, ok := got["ACC:E1"]; !ok {
		t.Fatalf("result not keyed by qualified id: %#v", got)
	}
	// The bare id should have been sent to Stalwart.
	call := fd.lastReq.MethodCalls[0]
	args := call[1].(map[string]any)
	ids := args["ids"].([]string)
	if len(ids) != 1 || ids[0] != "E1" {
		t.Fatalf("expected bare id sent, got %#v", ids)
	}
}

func TestJMAPFetcher_Empty(t *testing.T) {
	f := &JMAPFetcher{client: &fakeDispatcher{}}
	got, err := f.FetchMessages(context.Background(), "t1", "u1", nil)
	if err != nil {
		t.Fatalf("FetchMessages: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %#v", got)
	}
}
