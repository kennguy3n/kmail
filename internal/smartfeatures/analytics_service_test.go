package smartfeatures

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/kmail/internal/jmap"
	"github.com/kennguy3n/kmail/internal/middleware"
)

// roleDispatcher routes by the first method-call name so a single
// fake can answer both the Mailbox/get role lookup and the
// Email/query window queries that JMAPAnalyticsSource issues.
type roleDispatcher struct {
	mailboxResp *jmap.JmapResponse
	queryResp   *jmap.JmapResponse
	queryErr    error
	calls       int
}

func (d *roleDispatcher) Dispatch(_ context.Context, _, _ string, req jmap.JmapRequest) (*jmap.JmapResponse, error) {
	d.calls++
	method, _ := req.MethodCalls[0][0].(string)
	switch method {
	case "Mailbox/get":
		return d.mailboxResp, nil
	case "Email/query":
		if d.queryErr != nil {
			return nil, d.queryErr
		}
		return d.queryResp, nil
	default:
		return nil, errors.New("unexpected method " + method)
	}
}

// mapFetcher is an in-memory EmailFetcher.
type mapFetcher struct {
	byID map[string]Message
	err  error
}

func (f *mapFetcher) FetchMessages(_ context.Context, _, _ string, ids []string) (map[string]Message, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byID, nil
}

func mailboxGetResponse() *jmap.JmapResponse {
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"Mailbox/get", map[string]any{"list": []any{
				map[string]any{"id": "mb-inbox", "role": "inbox"},
				map[string]any{"id": "mb-sent", "role": "sent"},
				map[string]any{"id": "mb-none"}, // role absent → skipped
			}}, "m0"},
		},
	}
}

func emailQueryResponse(ids ...any) *jmap.JmapResponse {
	return &jmap.JmapResponse{
		MethodResponses: [][]any{
			{"Email/query", map[string]any{"ids": ids}, "q0"},
		},
	}
}

// TestJMAPAnalyticsSourceWindow drives Window → mailboxIDsByRole →
// queryWindow → fetcher end-to-end against fakes, covering the three
// previously-uncovered analytics_service methods.
func TestJMAPAnalyticsSourceWindow(t *testing.T) {
	d := &roleDispatcher{
		mailboxResp: mailboxGetResponse(),
		queryResp:   emailQueryResponse("E1", "E2"),
	}
	f := &mapFetcher{byID: map[string]Message{
		"E1": {ID: "E1"},
		"E2": {ID: "E2"},
	}}
	src := &JMAPAnalyticsSource{client: d, fetcher: f}

	sent, received, err := src.Window(context.Background(), "t1", "u1", time.Now().Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(sent) != 2 || len(received) != 2 {
		t.Fatalf("sent=%d received=%d want 2/2", len(sent), len(received))
	}
	// 1 Mailbox/get + 2 Email/query = 3 dispatches.
	if d.calls != 3 {
		t.Errorf("dispatch calls=%d want 3", d.calls)
	}
}

// TestJMAPAnalyticsSourceWindowQueryError surfaces a query failure.
func TestJMAPAnalyticsSourceWindowQueryError(t *testing.T) {
	d := &roleDispatcher{
		mailboxResp: mailboxGetResponse(),
		queryErr:    errors.New("stalwart unavailable"),
	}
	src := &JMAPAnalyticsSource{client: d, fetcher: &mapFetcher{}}
	if _, _, err := src.Window(context.Background(), "t1", "u1", time.Now(), 10); err == nil {
		t.Error("expected error when Email/query fails")
	}
}

// TestJMAPAnalyticsSourceEmptyWindow returns empty slices when the
// mailbox has no messages in range.
func TestJMAPAnalyticsSourceEmptyWindow(t *testing.T) {
	d := &roleDispatcher{
		mailboxResp: mailboxGetResponse(),
		queryResp:   emailQueryResponse(), // no ids
	}
	src := &JMAPAnalyticsSource{client: d, fetcher: &mapFetcher{byID: map[string]Message{}}}
	sent, received, err := src.Window(context.Background(), "t1", "u1", time.Now(), 10)
	if err != nil {
		t.Fatalf("Window: %v", err)
	}
	if len(sent) != 0 || len(received) != 0 {
		t.Errorf("expected empty windows, got sent=%d received=%d", len(sent), len(received))
	}
}

func TestNewJMAPAnalyticsSourceNilClient(t *testing.T) {
	if _, err := NewJMAPAnalyticsSource(nil); err == nil {
		t.Error("NewJMAPAnalyticsSource(nil) should error")
	}
}

// TestAnalyticsHandlersRegister wires the production Register behind a
// dev-bypass OIDC and exercises the route end-to-end (happy path,
// tz parsing, and the source-error 502).
func TestAnalyticsHandlersRegister(t *testing.T) {
	src := &fakeAnalyticsSource{
		sent:     []Message{{ID: "s1", ReceivedAt: at("2026-01-01T09:00:00Z"), To: []Address{{Email: "a@b.com"}}}},
		received: []Message{{ID: "r1", ReceivedAt: at("2026-01-01T08:00:00Z"), From: []Address{{Email: "c@d.com"}}}},
	}
	h := NewAnalyticsHandlers(src, nil)

	authMW, err := middleware.NewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-token",
		Env:            middleware.EnvDevelopment,
	})
	if err != nil {
		t.Fatalf("NewOIDC: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux, authMW)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/email-analytics?days=30&tz=America/New_York", nil)
	req.Header.Set("Authorization", "Bearer dev-token")
	req.Header.Set("X-KMail-Dev-Tenant-Id", "00000000-0000-0000-0000-000000000001")
	req.Header.Set("X-KMail-Dev-Kchat-User-Id", "u1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register route: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Source error → 502.
	src.err = errors.New("stalwart down")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("source error: code=%d want 502", rec.Code)
	}
}

func TestNewAnalyticsHandlersPanicsOnNil(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewAnalyticsHandlers(nil) should panic")
		}
	}()
	NewAnalyticsHandlers(nil, nil)
}
