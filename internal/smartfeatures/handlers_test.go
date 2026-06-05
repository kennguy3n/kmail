package smartfeatures

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// fakeFetcher serves canned messages keyed by id.
type fakeFetcher struct {
	msgs map[string]Message
	err  error
}

func (f *fakeFetcher) FetchMessages(_ context.Context, _, _ string, ids []string) (map[string]Message, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]Message{}
	for _, id := range ids {
		if m, ok := f.msgs[id]; ok {
			out[id] = m
		}
	}
	return out, nil
}

// recordingOneClick captures the URL it was asked to POST.
type recordingOneClick struct {
	url string
	err error
}

func (r *recordingOneClick) Post(_ context.Context, url string) error {
	r.url = url
	return r.err
}

func authed(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	ctx := middleware.WithKChatUserID(middleware.WithTenantID(context.Background(), "t1"), "u1")
	return r.WithContext(ctx)
}

func authedBody(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	ctx := middleware.WithKChatUserID(middleware.WithTenantID(context.Background(), "t1"), "u1")
	return r.WithContext(ctx)
}

func decode(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", w.Body.String(), err)
	}
	return out
}

func TestSmartRepliesHandler(t *testing.T) {
	h := NewHandlers(HandlersConfig{Fetcher: &fakeFetcher{msgs: map[string]Message{
		"E1": {ID: "E1", Subject: "Thank you!", Preview: "thanks!"},
	}}})
	r := authed(http.MethodGet, "/api/v1/emails/E1/smart-replies")
	r.SetPathValue("id", "E1")
	w := httptest.NewRecorder()
	h.smartReplies(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	out := decode(t, w)
	if out["suggestions"] == nil {
		t.Fatalf("missing suggestions: %v", out)
	}
}

func TestSmartRepliesHandler_NotFound(t *testing.T) {
	h := NewHandlers(HandlersConfig{Fetcher: &fakeFetcher{msgs: map[string]Message{}}})
	r := authed(http.MethodGet, "/api/v1/emails/missing/smart-replies")
	r.SetPathValue("id", "missing")
	w := httptest.NewRecorder()
	h.smartReplies(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

func TestGetUnsubscribeHandler(t *testing.T) {
	store, _ := NewUnsubscribeStore(newTestRedis(t), time.Hour)
	h := NewHandlers(HandlersConfig{
		Fetcher: &fakeFetcher{msgs: map[string]Message{
			"E1": {ID: "E1", Headers: map[string]string{
				"List-Unsubscribe": "<https://list.example/u/abc>",
				"List-Id":          "<promo.example.com>",
			}},
		}},
		Unsub: store,
	})
	r := authed(http.MethodGet, "/api/v1/emails/E1/unsubscribe")
	r.SetPathValue("id", "E1")
	w := httptest.NewRecorder()
	h.getUnsubscribe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	out := decode(t, w)
	if out["unsubscribe"] != true {
		t.Fatalf("expected unsubscribe=true, got %v", out)
	}
	if out["already_done"] != false {
		t.Fatalf("expected already_done=false, got %v", out)
	}
	// The wire shape is flat (matches web/src/api/smart.ts): the
	// preferred http target and list_id are top-level scalars, not
	// nested under an "info" object with array fields.
	if _, nested := out["info"]; nested {
		t.Fatalf("response must not nest an 'info' object, got %v", out)
	}
	if out["http"] != "https://list.example/u/abc" {
		t.Fatalf("expected top-level http target, got %v", out["http"])
	}
	if out["list_id"] != "promo.example.com" {
		t.Fatalf("expected top-level list_id, got %v", out["list_id"])
	}
}

func TestPostUnsubscribe_OneClick(t *testing.T) {
	store, _ := NewUnsubscribeStore(newTestRedis(t), time.Hour)
	oc := &recordingOneClick{}
	h := NewHandlers(HandlersConfig{
		Fetcher: &fakeFetcher{msgs: map[string]Message{
			"E1": {ID: "E1", Headers: map[string]string{
				"List-Unsubscribe":      "<https://list.example/u/abc>",
				"List-Unsubscribe-Post": "List-Unsubscribe=One-Click",
				"List-Id":               "<promo.example.com>",
			}},
		}},
		Unsub:    store,
		OneClick: oc,
	})
	r := authed(http.MethodPost, "/api/v1/emails/E1/unsubscribe")
	r.SetPathValue("id", "E1")
	w := httptest.NewRecorder()
	h.postUnsubscribe(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	out := decode(t, w)
	if out["method"] != "one-click" {
		t.Fatalf("expected one-click method, got %v", out)
	}
	if oc.url != "https://list.example/u/abc" {
		t.Fatalf("one-click posted to %q", oc.url)
	}
	// Should be recorded as unsubscribed.
	done, _ := store.IsUnsubscribed(context.Background(), "t1", "u1", "promo.example.com")
	if !done {
		t.Fatalf("expected unsubscribe to be recorded")
	}
}

func TestPostUnsubscribe_RecordedWhenNoOneClick(t *testing.T) {
	store, _ := NewUnsubscribeStore(newTestRedis(t), time.Hour)
	h := NewHandlers(HandlersConfig{
		Fetcher: &fakeFetcher{msgs: map[string]Message{
			"E1": {ID: "E1", Headers: map[string]string{
				"List-Unsubscribe": "<mailto:u@list.example>",
			}},
		}},
		Unsub: store,
	})
	r := authed(http.MethodPost, "/api/v1/emails/E1/unsubscribe")
	r.SetPathValue("id", "E1")
	w := httptest.NewRecorder()
	h.postUnsubscribe(w, r)
	out := decode(t, w)
	if out["method"] != "recorded" {
		t.Fatalf("expected recorded method, got %v", out)
	}
	if out["mailto"] != "mailto:u@list.example" {
		t.Fatalf("expected mailto passthrough, got %v", out)
	}
	// Intent was persisted (store present, list id derived), so the GET
	// handler will agree on reload — safe to report done.
	if out["already_done"] != true {
		t.Fatalf("expected already_done=true after a persisted record, got %v", out)
	}
}

// When no unsubscribe store is wired (Valkey unavailable) and there's no
// one-click POST, the recorded path persists nothing — so already_done
// must be false. Reporting true would flip-flop the button: the next GET
// (which also has no store) returns already_done=false.
func TestPostUnsubscribe_RecordedNotDoneWithoutStore(t *testing.T) {
	h := NewHandlers(HandlersConfig{
		Fetcher: &fakeFetcher{msgs: map[string]Message{
			"E1": {ID: "E1", Headers: map[string]string{
				"List-Unsubscribe": "<mailto:u@list.example>",
			}},
		}},
		// No Unsub store, no OneClick.
	})
	r := authed(http.MethodPost, "/api/v1/emails/E1/unsubscribe")
	r.SetPathValue("id", "E1")
	w := httptest.NewRecorder()
	h.postUnsubscribe(w, r)
	out := decode(t, w)
	if out["method"] != "recorded" {
		t.Fatalf("expected recorded method, got %v", out)
	}
	if out["already_done"] != false {
		t.Fatalf("expected already_done=false when nothing was persisted, got %v", out)
	}
}

func TestPostUnsubscribe_NoHeader(t *testing.T) {
	h := NewHandlers(HandlersConfig{Fetcher: &fakeFetcher{msgs: map[string]Message{
		"E1": {ID: "E1"},
	}}})
	r := authed(http.MethodPost, "/api/v1/emails/E1/unsubscribe")
	r.SetPathValue("id", "E1")
	w := httptest.NewRecorder()
	h.postUnsubscribe(w, r)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
}

func TestCategoriesHandler(t *testing.T) {
	h := NewHandlers(HandlersConfig{Fetcher: &fakeFetcher{msgs: map[string]Message{
		"E1": {ID: "E1", From: []Address{{Email: "deals@shop.com"}}, Headers: map[string]string{"List-Unsubscribe": "<https://x>"}},
		"E2": {ID: "E2", From: []Address{{Email: "alice@example.com"}}},
	}}})
	r := authedBody(http.MethodPost, "/api/v1/emails/categories", `{"ids":["E1","E2"]}`)
	w := httptest.NewRecorder()
	h.categories(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	out := decode(t, w)
	cats, _ := out["categories"].(map[string]any)
	if cats["E1"] != "promotions" || cats["E2"] != "primary" {
		t.Fatalf("categories wrong: %v", cats)
	}
}

func TestContactsEndpoints(t *testing.T) {
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	h := NewHandlers(HandlersConfig{Fetcher: &fakeFetcher{}, Contacts: tr})

	// Record a send.
	rec := authedBody(http.MethodPost, "/api/v1/contacts/record", `{"recipients":["alice@example.com","bob@example.com"]}`)
	w := httptest.NewRecorder()
	h.recordSend(w, rec)
	if w.Code != http.StatusOK {
		t.Fatalf("record status = %d body=%s", w.Code, w.Body.String())
	}

	// Frequent contacts now lists them.
	w = httptest.NewRecorder()
	h.frequentContacts(w, authed(http.MethodGet, "/api/v1/contacts/frequent"))
	if w.Code != http.StatusOK {
		t.Fatalf("frequent status = %d", w.Code)
	}
	out := decode(t, w)
	if list, _ := out["contacts"].([]any); len(list) != 2 {
		t.Fatalf("expected 2 contacts, got %v", out["contacts"])
	}

	// Co-recipient suggestion for alice → bob.
	w = httptest.NewRecorder()
	h.coRecipients(w, authed(http.MethodGet, "/api/v1/contacts/suggestions?anchor=alice@example.com"))
	if w.Code != http.StatusOK {
		t.Fatalf("suggestions status = %d", w.Code)
	}
}

// TestCoRecipientsExcludeRepeatedParams pins that every repeated
// `exclude` query param is honored (the client sends one param per
// already-added recipient), not just the first.
func TestCoRecipientsExcludeRepeatedParams(t *testing.T) {
	tr, _ := NewContactTracker(newTestRedis(t), time.Hour)
	h := NewHandlers(HandlersConfig{Fetcher: &fakeFetcher{}, Contacts: tr})

	// A message to alice+bob+carol records bob and carol as
	// co-recipients of alice.
	rec := authedBody(http.MethodPost, "/api/v1/contacts/record",
		`{"recipients":["alice@example.com","bob@example.com","carol@example.com"]}`)
	w := httptest.NewRecorder()
	h.recordSend(w, rec)
	if w.Code != http.StatusOK {
		t.Fatalf("record status = %d body=%s", w.Code, w.Body.String())
	}

	// Exclude bob AND carol via two separate params. If only the
	// first were read, carol would leak into the suggestions.
	w = httptest.NewRecorder()
	h.coRecipients(w, authed(http.MethodGet,
		"/api/v1/contacts/suggestions?anchor=alice@example.com&exclude=bob@example.com&exclude=carol@example.com"))
	if w.Code != http.StatusOK {
		t.Fatalf("suggestions status = %d body=%s", w.Code, w.Body.String())
	}
	out := decode(t, w)
	list, _ := out["suggestions"].([]any)
	if len(list) != 0 {
		t.Fatalf("expected both co-recipients excluded, got %v", out["suggestions"])
	}
}

func TestContactsEndpoints_UnavailableWhenNoTracker(t *testing.T) {
	h := NewHandlers(HandlersConfig{Fetcher: &fakeFetcher{}})
	w := httptest.NewRecorder()
	h.frequentContacts(w, authed(http.MethodGet, "/api/v1/contacts/frequent"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

func TestClampLimit(t *testing.T) {
	cases := []struct {
		raw      string
		def, max int
		want     int
	}{
		{"", 10, 50, 10},
		{"5", 10, 50, 5},
		{"999", 10, 50, 50},
		{"-3", 10, 50, 10},
		{"abc", 10, 50, 10},
	}
	for _, c := range cases {
		if got := clampLimit(c.raw, c.def, c.max); got != c.want {
			t.Fatalf("clampLimit(%q,%d,%d) = %d, want %d", c.raw, c.def, c.max, got, c.want)
		}
	}
}
