package tenant

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kennguy3n/kmail/internal/jmap"
)

// TestClassifyCreateTenantResult locks in the ordering of the error
// translation in signupProvisioner.CreateTenant: a non-nil tenant (the
// INSERT succeeded) must always be classified as a partial-provisioning
// failure, even when the accompanying error is a Postgres unique violation
// raised by a post-insert hook — otherwise the valid tenant pointer is
// discarded and a freshly-created tenant is misreported as a replay.
func TestClassifyCreateTenantResult(t *testing.T) {
	uniq := fmt.Errorf("billing.OnTenantCreated: %w", &pgconn.PgError{Code: "23505"})
	tenant := &Tenant{ID: "tenant-1", Slug: "acme"}

	t.Run("success passes tenant through", func(t *testing.T) {
		got, err := classifyCreateTenantResult(tenant, nil)
		if err != nil || got != tenant {
			t.Fatalf("got (%v, %v), want (tenant, nil)", got, err)
		}
	})

	t.Run("inserted tenant + hook unique violation is partial provisioning", func(t *testing.T) {
		got, err := classifyCreateTenantResult(tenant, uniq)
		if got != tenant {
			t.Fatalf("tenant = %v, want preserved pointer %v", got, tenant)
		}
		if !errors.Is(err, ErrTenantProvisionIncomplete) {
			t.Fatalf("err = %v, want ErrTenantProvisionIncomplete", err)
		}
		if errors.Is(err, ErrTenantExists) {
			t.Fatal("must not classify an inserted tenant as ErrTenantExists")
		}
	})

	t.Run("failed insert unique violation is tenant exists", func(t *testing.T) {
		got, err := classifyCreateTenantResult(nil, uniq)
		if got != nil {
			t.Fatalf("tenant = %v, want nil", got)
		}
		if !errors.Is(err, ErrTenantExists) {
			t.Fatalf("err = %v, want ErrTenantExists", err)
		}
	})

	t.Run("failed insert other error passes through", func(t *testing.T) {
		boom := errors.New("connection refused")
		got, err := classifyCreateTenantResult(nil, boom)
		if got != nil || !errors.Is(err, boom) {
			t.Fatalf("got (%v, %v), want (nil, boom)", got, err)
		}
	})
}

// fakeJMAPSubmitter records dispatched requests and returns canned
// responses keyed by the first method-call name, so we can assert how
// JMAPWelcomeMailer builds its Mailbox/get + Email/set batch.
type fakeJMAPSubmitter struct {
	reqs         []jmap.JmapRequest
	mailboxResp  *jmap.JmapResponse
	identityResp *jmap.JmapResponse
	sendResp     *jmap.JmapResponse
}

func (f *fakeJMAPSubmitter) ResolveAccountID(_ context.Context, _, _ string) (string, error) {
	return "acct-1", nil
}

func (f *fakeJMAPSubmitter) Dispatch(_ context.Context, _, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	f.reqs = append(f.reqs, req)
	name, _ := req.MethodCalls[0][0].(string)
	switch name {
	case "Mailbox/get":
		return f.mailboxResp, nil
	case "Identity/get":
		if f.identityResp != nil {
			return f.identityResp, nil
		}
		// Default: one identity matching the welcome from address, so
		// tests that don't care about identity resolution still send.
		return identityGetResp(map[string]any{"id": "id-welcome", "email": "welcome@kmail.test"}), nil
	default:
		return f.sendResp, nil
	}
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

func identityGetResp(identities ...map[string]any) *jmap.JmapResponse {
	list := make([]any, 0, len(identities))
	for _, id := range identities {
		list = append(list, id)
	}
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"Identity/get", map[string]any{"list": list}, "i0"},
		},
	}
}

// submissionSetCreate returns the EmailSubmission/set create map for
// the "sub" creation id from the welcome send request (always the last
// dispatched request).
func submissionSetCreate(t *testing.T, sub *fakeJMAPSubmitter) map[string]any {
	t.Helper()
	last := sub.reqs[len(sub.reqs)-1]
	for _, call := range last.MethodCalls {
		if name, _ := call[0].(string); name != "EmailSubmission/set" {
			continue
		}
		args, _ := call[1].(map[string]any)
		create, _ := args["create"].(map[string]any)
		s, _ := create["sub"].(map[string]any)
		return s
	}
	t.Fatalf("EmailSubmission/set call not found in request: %+v", last.MethodCalls)
	return nil
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

	if len(sub.reqs) != 3 {
		t.Fatalf("dispatch calls = %d, want 3 (Mailbox/get, Identity/get, then send)", len(sub.reqs))
	}
	sendReq := sub.reqs[len(sub.reqs)-1]
	welcome := emailSetCreate(t, sendReq)
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
	for _, call := range sendReq.MethodCalls {
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
	welcome := emailSetCreate(t, sub.reqs[len(sub.reqs)-1])
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

func TestWelcomeMailer_UsesConfiguredIdentity(t *testing.T) {
	sub := &fakeJMAPSubmitter{
		mailboxResp: mailboxGetResp(map[string]any{"id": "mb-drafts", "role": "drafts"}),
		sendResp:    okSendResp(),
	}
	m := newWelcomeMailer(sub)
	m.identityID = "id-configured"
	if err := m.SendWelcome(context.Background(), WelcomeMessage{To: "x@acme.com", OrgName: "Acme", Plan: "core"}); err != nil {
		t.Fatalf("SendWelcome: %v", err)
	}
	// A configured identity must short-circuit resolution: only
	// Mailbox/get + send are dispatched, no Identity/get.
	if len(sub.reqs) != 2 {
		t.Fatalf("dispatch calls = %d, want 2 (Mailbox/get then send) when identity is configured", len(sub.reqs))
	}
	if got := submissionSetCreate(t, sub)["identityId"]; got != "id-configured" {
		t.Fatalf("identityId = %v, want the configured id-configured", got)
	}
}

func TestWelcomeMailer_ResolvesIdentityMatchingFromAddress(t *testing.T) {
	sub := &fakeJMAPSubmitter{
		mailboxResp: mailboxGetResp(map[string]any{"id": "mb-drafts", "role": "drafts"}),
		identityResp: identityGetResp(
			map[string]any{"id": "id-other", "email": "noreply@kmail.test"},
			map[string]any{"id": "id-welcome", "email": "welcome@kmail.test"},
		),
		sendResp: okSendResp(),
	}
	m := newWelcomeMailer(sub) // fromAddress welcome@kmail.test, identity unset
	if err := m.SendWelcome(context.Background(), WelcomeMessage{To: "x@acme.com", OrgName: "Acme", Plan: "core"}); err != nil {
		t.Fatalf("SendWelcome: %v", err)
	}
	// RFC 8621 §7: identityId is required — it must be present and match
	// the from address, never omitted.
	if got := submissionSetCreate(t, sub)["identityId"]; got != "id-welcome" {
		t.Fatalf("identityId = %v, want id-welcome (the identity matching the from address)", got)
	}
}

func TestWelcomeMailer_FallsBackToFirstIdentity(t *testing.T) {
	sub := &fakeJMAPSubmitter{
		mailboxResp: mailboxGetResp(map[string]any{"id": "mb-drafts", "role": "drafts"}),
		identityResp: identityGetResp(
			map[string]any{"id": "id-first", "email": "someoneelse@kmail.test"},
		),
		sendResp: okSendResp(),
	}
	m := newWelcomeMailer(sub) // no identity matches the from address
	if err := m.SendWelcome(context.Background(), WelcomeMessage{To: "x@acme.com", OrgName: "Acme", Plan: "core"}); err != nil {
		t.Fatalf("SendWelcome: %v", err)
	}
	if got := submissionSetCreate(t, sub)["identityId"]; got != "id-first" {
		t.Fatalf("identityId = %v, want fallback to the first identity id-first", got)
	}
}

func TestWelcomeMailer_NoIdentityOmitsIdentityIdBestEffort(t *testing.T) {
	sub := &fakeJMAPSubmitter{
		mailboxResp:  mailboxGetResp(map[string]any{"id": "mb-drafts", "role": "drafts"}),
		identityResp: identityGetResp(), // account has no identities
		sendResp:     okSendResp(),
	}
	m := newWelcomeMailer(sub)
	// Best-effort: with no resolvable identity we still send (letting the
	// server auto-select) rather than failing the welcome email.
	if err := m.SendWelcome(context.Background(), WelcomeMessage{To: "x@acme.com", OrgName: "Acme", Plan: "core"}); err != nil {
		t.Fatalf("SendWelcome: %v", err)
	}
	if _, present := submissionSetCreate(t, sub)["identityId"]; present {
		t.Fatal("identityId should be omitted when no identity can be resolved")
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
