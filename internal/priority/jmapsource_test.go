package priority

import (
	"context"
	"testing"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/smartfeatures"
)

// scriptedDispatcher returns a queued response per call, in order,
// so a multi-call flow (Mailbox/get then Email/query then
// Email/get) can be driven deterministically.
type scriptedDispatcher struct {
	responses []*jmap.JmapResponse
	calls     int
}

func (s *scriptedDispatcher) Dispatch(_ context.Context, _, _ string, _ jmap.JmapRequest) (*jmap.JmapResponse, error) {
	r := s.responses[s.calls]
	s.calls++
	return r, nil
}

func resp(name string, args map[string]any, id string) *jmap.JmapResponse {
	return &jmap.JmapResponse{MethodResponses: [][]any{{name, args, id}}}
}

// fakeFetcher serves canned messages, decoupling the ListInbox
// query path from the Email/get hydration path under test.
type fakeFetcher struct {
	byID map[string]smartfeatures.Message
}

func (f fakeFetcher) FetchMessages(_ context.Context, _, _ string, ids []string) (map[string]smartfeatures.Message, error) {
	out := map[string]smartfeatures.Message{}
	for _, id := range ids {
		if m, ok := f.byID[id]; ok {
			out[id] = m
		}
	}
	return out, nil
}

func TestJMAPSource_ListInbox(t *testing.T) {
	disp := &scriptedDispatcher{responses: []*jmap.JmapResponse{
		// inboxMailboxID → Mailbox/get
		resp("Mailbox/get", map[string]any{"list": []any{
			map[string]any{"id": "MB1", "role": "inbox"},
		}}, "m0"),
		// ListInbox → Email/query
		resp("Email/query", map[string]any{"ids": []any{"E1", "E2"}}, "q0"),
	}}
	src := &JMAPSource{client: disp, fetcher: fakeFetcher{byID: map[string]smartfeatures.Message{
		"E1": {ID: "E1", Subject: "one"},
		"E2": {ID: "E2", Subject: "two"},
	}}}

	got, err := src.ListInbox(context.Background(), "t1", "u1", 10)
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	// Query order (E1, E2) must be preserved even though Email/get
	// returned them reversed.
	if len(got) != 2 || got[0].ID != "E1" || got[1].ID != "E2" {
		t.Fatalf("order not preserved: %#v", ids2(got))
	}
}

func TestJMAPSource_UserAddress(t *testing.T) {
	disp := &scriptedDispatcher{responses: []*jmap.JmapResponse{
		resp("Identity/get", map[string]any{"list": []any{
			map[string]any{"id": "I1", "email": "me@acme.com"},
		}}, "i0"),
	}}
	src := &JMAPSource{client: disp}
	addr, err := src.UserAddress(context.Background(), "t1", "u1")
	if err != nil {
		t.Fatalf("UserAddress: %v", err)
	}
	if addr != "me@acme.com" {
		t.Fatalf("addr = %q", addr)
	}
}

func ids2(s []smartfeatures.Message) []string {
	out := make([]string, len(s))
	for i, m := range s {
		out[i] = m.ID
	}
	return out
}
