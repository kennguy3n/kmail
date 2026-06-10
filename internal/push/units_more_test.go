package push

import (
	"context"
	"io"
	"log"
	"testing"
)

func TestAllowsKindBranches(t *testing.T) {
	p := NotificationPreference{NewEmail: true, CalendarReminder: false, SharedInbox: true}
	cases := map[string]bool{
		"new_email":         true,
		"calendar_reminder": false,
		"shared_inbox":      true,
		"unknown_kind":      true, // default → allowed
	}
	for kind, want := range cases {
		if got := p.allowsKind(kind); got != want {
			t.Errorf("allowsKind(%q)=%v want %v", kind, got, want)
		}
	}
}

func TestParseHMEdges(t *testing.T) {
	for _, bad := range []string{"", "9:00", "24:00", "12:60", "ab:cd", "12-00"} {
		if _, _, ok := parseHM(bad); ok {
			t.Errorf("parseHM(%q) ok=true want false", bad)
		}
	}
	if h, m, ok := parseHM("23:59"); !ok || h != 23 || m != 59 {
		t.Errorf("parseHM(23:59)=%d:%d ok=%v", h, m, ok)
	}
}

func TestLoggingTransport(t *testing.T) {
	tr := NewLoggingTransport(nil)
	if tr == nil {
		t.Fatal("NewLoggingTransport returned nil")
	}
	if err := tr.Send(context.Background(), Subscription{ID: "s", DeviceType: "web"}, Notification{Title: "t", Body: "b"}); err != nil {
		t.Errorf("loggingTransport.Send: %v", err)
	}
}

func TestNewTransportRouterDefaults(t *testing.T) {
	r := NewTransportRouter(nil)
	if r == nil || r.Logger == nil {
		t.Fatal("NewTransportRouter should default the logger")
	}
	web := &recordingTransport{}
	r.Web = web
	if err := r.Send(context.Background(), Subscription{DeviceType: "web"}, Notification{}); err != nil {
		t.Errorf("router.Send web: %v", err)
	}
	if len(web.calls) != 1 {
		t.Errorf("web transport calls=%d want 1", len(web.calls))
	}
	// No transport for ios and no Default ⇒ error.
	if err := r.Send(context.Background(), Subscription{DeviceType: "ios"}, Notification{}); err == nil {
		t.Error("router.Send ios should error with no transport/default")
	}
	// Logging default catches unknown platforms.
	r.Default = NewLoggingTransport(log.New(io.Discard, "", 0))
	if err := r.Send(context.Background(), Subscription{DeviceType: "ios"}, Notification{}); err != nil {
		t.Errorf("router.Send ios with default: %v", err)
	}
}
