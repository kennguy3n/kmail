package chatbridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type recordKChat struct {
	posts []struct {
		channelID string
		msg       ChannelMessage
	}
	fail error
}

func (r *recordKChat) PostChannelMessage(_ context.Context, channelID string, msg ChannelMessage) error {
	if r.fail != nil {
		return r.fail
	}
	r.posts = append(r.posts, struct {
		channelID string
		msg       ChannelMessage
	}{channelID, msg})
	return nil
}

func TestShareEmailToChannel_Validates(t *testing.T) {
	svc := NewService(Config{KChat: &recordKChat{}})
	if err := svc.ShareEmailToChannel(context.Background(), "", "e1", "c1", "u1"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestShareEmailToChannel_PostsCard(t *testing.T) {
	// Fake Stalwart JMAP Email/get endpoint — returns one email
	// with subject + preview + mailboxIds.
	stalwart := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"methodResponses":[["Email/get",{"list":[{"subject":"hi","preview":"body","from":[{"email":"a@b.com"}],"mailboxIds":{"mb1":true}}]},"c1"]]}`)
	}))
	defer stalwart.Close()

	kc := &recordKChat{}
	svc := NewService(Config{KChat: kc, StalwartURL: stalwart.URL})
	if err := svc.ShareEmailToChannel(context.Background(), "t1", "e1", "c1", "u1"); err != nil {
		t.Fatalf("ShareEmailToChannel: %v", err)
	}
	if len(kc.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", len(kc.posts))
	}
	if kc.posts[0].channelID != "c1" {
		t.Errorf("channelID = %q, want c1", kc.posts[0].channelID)
	}
	if len(kc.posts[0].msg.Attachments) == 0 || kc.posts[0].msg.Attachments[0].Title != "hi" {
		t.Errorf("unexpected message: %+v", kc.posts[0].msg)
	}
}

func TestConfigureAlertRoute_Validates(t *testing.T) {
	svc := NewService(Config{KChat: &recordKChat{}})
	if _, err := svc.ConfigureAlertRoute(context.Background(), "", "a@b", "c1"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestConfigureAlertRoute_LowercasesAlias(t *testing.T) {
	svc := NewService(Config{KChat: &recordKChat{}})
	route, err := svc.ConfigureAlertRoute(context.Background(), "t1", "Alerts@Tenant.TLD", "c1")
	if err != nil {
		t.Fatalf("ConfigureAlertRoute: %v", err)
	}
	if route.AliasAddress != "alerts@tenant.tld" {
		t.Errorf("AliasAddress = %q, want lowercased", route.AliasAddress)
	}
}

func TestProcessInboundAlert_NoRoute_IsNoop(t *testing.T) {
	svc := NewService(Config{KChat: &recordKChat{}})
	if err := svc.ProcessInboundAlert(context.Background(), "t1", "nobody@tenant.tld", "e1"); err != nil {
		t.Errorf("ProcessInboundAlert: %v", err)
	}
}

func TestKChatHTTP_PostsJSON(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("expected Bearer auth, got %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := &httpKChatClient{cfg: Config{KChatAPIURL: srv.URL, KChatAPIToken: "secret", HTTPClient: &http.Client{}}}
	if err := c.PostChannelMessage(context.Background(), "c1", ChannelMessage{Text: "hi"}); err != nil {
		t.Fatalf("PostChannelMessage: %v", err)
	}
	if path != "/api/v1/channels/c1/messages" {
		t.Errorf("path = %q", path)
	}
}

func TestKChatHTTP_SurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "upstream died")
	}))
	defer srv.Close()

	c := &httpKChatClient{cfg: Config{KChatAPIURL: srv.URL, HTTPClient: &http.Client{}}}
	err := c.PostChannelMessage(context.Background(), "c1", ChannelMessage{})
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}
