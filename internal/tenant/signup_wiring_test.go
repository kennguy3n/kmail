package tenant

import (
	"context"
	"log"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// fakeJMAPSubmitter records dispatched requests and returns canned
// responses keyed by the first method-call name, so we can assert how
// JMAPWelcomeMailer builds its Mailbox/get + Email/set batch.
type fakeJMAPSubmitter struct {
	reqs        []jmap.JmapRequest
	mailboxResp *jmap.JmapResponse
	sendResp    *jmap.JmapResponse
}

func (f *fakeJMAPSubmitter) ResolveAccountID(_ context.Context, _, _ string) (string, error) {
	return "acct-1", nil
}

func (f *fakeJMAPSubmitter) Dispatch(_ context.Context, _, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	f.reqs = append(f.reqs, req)
	name, _ := req.MethodCalls[0][0].(string)
	if name == "Mailbox/get" {
		return f.mailboxResp, nil
	}
	return f.sendResp, nil
}

func mailboxGetResp(mailboxes ...map[string]any) *jmap.JmapResponse {
	list := make([]any, 0, len(mailboxes))
	for _, m := range mailboxes {
		list = append(list, m)
	}
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"Mailbox/get", map[string]any{"list": list}, "m0"},
		},
	}
}

func okSendResp() *jmap.JmapResponse {
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"Email/set", map[string]any{"created": map[string]any{"welcome": map[string]any{"id": "em-1"}}}, "c0"},
			{"EmailSubmission/set", map[string]any{"created": map[string]any{"sub": map[string]any{"id": "sub-1"}}}, "c1"},
		},
	}
}

func newWelcomeMailer(sub jmapSubmitter) *JMAPWelcomeMailer {
	return &JMAPWelcomeMailer{
		submitter:         sub,
		senderTenantID:    "t-sys",
		senderKChatUserID: "u-sys",
		fromAddress:       "welcome@kmail.test",
		logger:            log.Default(),
	}
}

func emailSetCreate(t *testing.T, req jmap.JmapRequest) map[string]any {
	t.Helper()
	for _, call := range req.MethodCalls {
		name, _ := call[0].(string)
		if name != "Email/set" {
			continue
		}
		args, _ := call[1].(map[string]any)
		create, _ := args["create"].(map[string]any)
		welcome, _ := create["welcome"].(map[string]any)
		return welcome
	}
	t.Fatalf("Email/set call not found in request: %+v", req.MethodCalls)
	return nil
}

func TestWelcomeMailer_ResolvesRealDraftsMailbox(t *testing.T) {
	sub := &fakeJMAPSubmitter{
		mailboxResp: mailboxGetResp(
			map[string]any{"id": "mb-inbox", "role": "inbox"},
			map[string]any{"id": "mb-drafts", "role": "drafts"},
		),
		sendResp: okSendResp(),
	}
	m := newWelcomeMailer(sub)

	if err := m.SendWelcome(context.Background(), WelcomeMessage{To: "founder@acme.com", OrgName: "Acme", Plan: "core"}); err != nil {
		t.Fatalf("SendWelcome: %v", err)
	}

	if len(sub.reqs) != 2 {
		t.Fatalf("dispatch calls = %d, want 2 (Mailbox/get then send)", len(sub.reqs))
	}
	welcome := emailSetCreate(t, sub.reqs[1])
	mailboxIDs, ok := welcome["mailboxIds"].(map[string]bool)
	if !ok {
		t.Fatalf("mailboxIds type = %T, want map[string]bool", welcome["mailboxIds"])
	}
	if !mailboxIDs["mb-drafts"] {
		t.Fatalf("mailboxIds = %v, want the resolved drafts id mb-drafts", mailboxIDs)
	}
	// Regression: the literal "$drafts" placeholder must never be used —
	// Stalwart would reject the create and the welcome would silently
	// no-op.
	if _, bad := mailboxIDs["$drafts"]; bad {
		t.Fatal("mailboxIds still contains the invalid \"$drafts\" placeholder")
	}

	// The submission must clean up the transient draft on success.
	var sawDestroy bool
	for _, call := range sub.reqs[1].MethodCalls {
		if name, _ := call[0].(string); name == "EmailSubmission/set" {
			args, _ := call[1].(map[string]any)
			if _, ok := args["onSuccessDestroyEmail"]; ok {
				sawDestroy = true
			}
		}
	}
	if !sawDestroy {
		t.Error("EmailSubmission/set missing onSuccessDestroyEmail")
	}
}

func TestWelcomeMailer_FallsBackWhenNoDraftsRole(t *testing.T) {
	sub := &fakeJMAPSubmitter{
		mailboxResp: mailboxGetResp(
			map[string]any{"id": "mb-only", "role": nil},
		),
		sendResp: okSendResp(),
	}
	m := newWelcomeMailer(sub)
	if err := m.SendWelcome(context.Background(), WelcomeMessage{To: "x@acme.com", OrgName: "Acme", Plan: "core"}); err != nil {
		t.Fatalf("SendWelcome: %v", err)
	}
	welcome := emailSetCreate(t, sub.reqs[1])
	mailboxIDs, _ := welcome["mailboxIds"].(map[string]bool)
	if !mailboxIDs["mb-only"] {
		t.Fatalf("mailboxIds = %v, want fallback to the only mailbox mb-only", mailboxIDs)
	}
}

func TestWelcomeMailer_NoMailboxesIsError(t *testing.T) {
	sub := &fakeJMAPSubmitter{
		mailboxResp: mailboxGetResp(),
		sendResp:    okSendResp(),
	}
	m := newWelcomeMailer(sub)
	err := m.SendWelcome(context.Background(), WelcomeMessage{To: "x@acme.com", OrgName: "Acme", Plan: "core"})
	if err == nil {
		t.Fatal("expected error when the account has no mailboxes")
	}
	if !strings.Contains(err.Error(), "drafts mailbox") {
		t.Fatalf("error = %v, want it to mention drafts mailbox resolution", err)
	}
}

func TestWelcomeMailer_NotCreatedSurfacesError(t *testing.T) {
	sub := &fakeJMAPSubmitter{
		mailboxResp: mailboxGetResp(map[string]any{"id": "mb-drafts", "role": "drafts"}),
		sendResp: &jmap.JmapResponse{
			MethodResponses: [][]any{
				{"Email/set", map[string]any{
					"notCreated": map[string]any{
						"welcome": map[string]any{"type": "invalidProperties", "description": "mailboxIds"},
					},
				}, "c0"},
			},
		},
	}
	m := newWelcomeMailer(sub)
	err := m.SendWelcome(context.Background(), WelcomeMessage{To: "x@acme.com", OrgName: "Acme", Plan: "core"})
	if err == nil {
		t.Fatal("expected error when Email/set reports notCreated")
	}
	if !strings.Contains(err.Error(), "notCreated") {
		t.Fatalf("error = %v, want it to surface the notCreated rejection", err)
	}
}
