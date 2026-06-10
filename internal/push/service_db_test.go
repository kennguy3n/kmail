package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/middleware"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func dbService(t *testing.T) (*Service, *recordingTransport, string) {
	t.Helper()
	pool := testsupport.RLSPool(t)
	admin := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, admin, "pro", "active")
	tr := &recordingTransport{}
	svc := NewService(Config{Pool: pool, Transport: tr, Logger: log.New(io.Discard, "", 0)})
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM push_subscriptions WHERE tenant_id=$1::uuid`, tenant)
		_, _ = admin.Exec(context.Background(), `DELETE FROM notification_preferences WHERE tenant_id=$1::uuid`, tenant)
	})
	return svc, tr, tenant
}

func TestPushServiceLifecycleDB(t *testing.T) {
	svc, tr, tenant := dbService(t)
	ctx := context.Background()
	const user = "user-1"

	// Subscribe (defaults device_type to web).
	sub, err := svc.Subscribe(ctx, tenant, user, Subscription{PushEndpoint: "https://push.example.com/ep1"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if sub.ID == "" || sub.DeviceType != "web" {
		t.Errorf("Subscribe out=%+v", sub)
	}

	// Re-subscribe same endpoint upserts (still one row).
	if _, err := svc.Subscribe(ctx, tenant, user, Subscription{PushEndpoint: "https://push.example.com/ep1", DeviceType: "ios", AuthKey: "a", P256DHKey: "p"}); err != nil {
		t.Fatalf("re-subscribe: %v", err)
	}
	subs, err := svc.ListSubscriptions(ctx, tenant, user)
	if err != nil || len(subs) != 1 {
		t.Fatalf("ListSubscriptions=%d err=%v", len(subs), err)
	}
	if subs[0].DeviceType != "ios" {
		t.Errorf("upsert device_type=%q want ios", subs[0].DeviceType)
	}

	// Preferences: default (no row) then update.
	prefs, err := svc.GetPreferences(ctx, tenant, user)
	if err != nil || !prefs.NewEmail || !prefs.CalendarReminder || !prefs.SharedInbox {
		t.Fatalf("default prefs=%+v err=%v", prefs, err)
	}
	updated, err := svc.UpdatePreferences(ctx, tenant, user, NotificationPreference{NewEmail: false, CalendarReminder: true, SharedInbox: false, QuietHoursStart: "22:00", QuietHoursEnd: "07:00"})
	if err != nil || updated.NewEmail {
		t.Fatalf("UpdatePreferences=%+v err=%v", updated, err)
	}
	got, _ := svc.GetPreferences(ctx, tenant, user)
	if got.NewEmail || !got.CalendarReminder || got.SharedInbox || got.QuietHoursStart != "22:00" {
		t.Errorf("persisted prefs=%+v", got)
	}

	// SendNotification: new_email disabled ⇒ suppressed (no transport call).
	if err := svc.SendNotification(ctx, tenant, user, Notification{Kind: "new_email", Title: "x"}); err != nil {
		t.Fatalf("send new_email: %v", err)
	}
	if len(tr.calls) != 0 {
		t.Errorf("disabled kind should not dispatch, calls=%v", tr.calls)
	}
	// calendar_reminder enabled + bypasses quiet hours ⇒ dispatched.
	if err := svc.SendNotification(ctx, tenant, user, Notification{Kind: "calendar_reminder", Title: "meeting"}); err != nil {
		t.Fatalf("send reminder: %v", err)
	}
	if len(tr.calls) != 1 {
		t.Errorf("calendar_reminder should dispatch once, calls=%v", tr.calls)
	}

	// Unsubscribe removes the row; second delete ⇒ ErrNotFound.
	if err := svc.Unsubscribe(ctx, tenant, user, sub.ID); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if err := svc.Unsubscribe(ctx, tenant, user, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("re-unsubscribe=%v want ErrNotFound", err)
	}
}

func TestPushServiceValidationDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	ctx := context.Background()
	if _, err := svc.Subscribe(ctx, "", "u", Subscription{PushEndpoint: "x"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("missing tenant: %v", err)
	}
	if _, err := svc.Subscribe(ctx, tenant, "u", Subscription{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("missing endpoint: %v", err)
	}
	if _, err := svc.Subscribe(ctx, tenant, "u", Subscription{PushEndpoint: "x", DeviceType: "toaster"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad device_type: %v", err)
	}
	if err := svc.Unsubscribe(ctx, tenant, "", "id"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unsubscribe missing user: %v", err)
	}
	if _, err := svc.ListSubscriptions(ctx, "", "u"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("list missing tenant: %v", err)
	}
	if _, err := svc.GetPreferences(ctx, tenant, ""); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("getprefs missing user: %v", err)
	}
	if _, err := svc.UpdatePreferences(ctx, "", "u", NotificationPreference{}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("updateprefs missing tenant: %v", err)
	}
}

func TestPushServiceNilPool(t *testing.T) {
	svc := NewService(Config{Logger: log.New(io.Discard, "", 0)})
	ctx := context.Background()
	if sub, err := svc.Subscribe(ctx, "t", "u", Subscription{PushEndpoint: "x"}); err != nil || sub.ID != "stub" {
		t.Errorf("nil-pool subscribe=%+v err=%v", sub, err)
	}
	if err := svc.Unsubscribe(ctx, "t", "u", "id"); err != nil {
		t.Errorf("nil-pool unsubscribe: %v", err)
	}
	if subs, err := svc.ListSubscriptions(ctx, "t", "u"); err != nil || subs != nil {
		t.Errorf("nil-pool list=%v err=%v", subs, err)
	}
	if _, err := svc.UpdatePreferences(ctx, "t", "u", NotificationPreference{}); err != nil {
		t.Errorf("nil-pool updateprefs: %v", err)
	}
	if svc.StalwartURL() != "" {
		t.Errorf("StalwartURL should be empty")
	}
}

// --- handlers ---

func pushReq(tenant, account, method, body string, pv map[string]string) *http.Request {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, "/x", strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, "/x", nil)
	}
	ctx := r.Context()
	if tenant != "" {
		ctx = middleware.WithTenantID(ctx, tenant)
	}
	if account != "" {
		ctx = middleware.WithStalwartAccountID(ctx, account)
	}
	r = r.WithContext(ctx)
	for k, v := range pv {
		r.SetPathValue(k, v)
	}
	return r
}

func TestPushHandlersDB(t *testing.T) {
	svc, _, tenant := dbService(t)
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	const user = "acct-1"

	// subscribe happy ⇒ 201.
	rec := httptest.NewRecorder()
	h.subscribe(rec, pushReq(tenant, user, http.MethodPost, `{"push_endpoint":"https://p/ep","device_type":"web"}`, nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("subscribe=%d body=%s", rec.Code, rec.Body.String())
	}
	var created Subscription
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	// subscribe bad JSON ⇒ 400.
	rec = httptest.NewRecorder()
	h.subscribe(rec, pushReq(tenant, user, http.MethodPost, `{bad`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("subscribe bad-json=%d want 400", rec.Code)
	}

	// subscribe invalid input (no endpoint) ⇒ 400 via statusFor.
	rec = httptest.NewRecorder()
	h.subscribe(rec, pushReq(tenant, user, http.MethodPost, `{}`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("subscribe no-endpoint=%d want 400", rec.Code)
	}

	// missing tenant ⇒ 403.
	rec = httptest.NewRecorder()
	h.subscribe(rec, pushReq("", user, http.MethodPost, `{}`, nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("subscribe no-tenant=%d want 403", rec.Code)
	}

	// missing user ⇒ 403.
	rec = httptest.NewRecorder()
	h.subscribe(rec, pushReq(tenant, "", http.MethodPost, `{}`, nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("subscribe no-user=%d want 403", rec.Code)
	}

	// list ⇒ 200.
	rec = httptest.NewRecorder()
	h.list(rec, pushReq(tenant, user, http.MethodGet, "", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "subscriptions") {
		t.Errorf("list=%d body=%s", rec.Code, rec.Body.String())
	}

	// getPrefs ⇒ 200.
	rec = httptest.NewRecorder()
	h.getPrefs(rec, pushReq(tenant, user, http.MethodGet, "", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("getPrefs=%d", rec.Code)
	}

	// setPrefs happy ⇒ 200.
	rec = httptest.NewRecorder()
	h.setPrefs(rec, pushReq(tenant, user, http.MethodPut, `{"new_email":false,"calendar_reminder":true,"shared_inbox":true}`, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("setPrefs=%d body=%s", rec.Code, rec.Body.String())
	}

	// setPrefs bad JSON ⇒ 400.
	rec = httptest.NewRecorder()
	h.setPrefs(rec, pushReq(tenant, user, http.MethodPut, `{bad`, nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("setPrefs bad-json=%d want 400", rec.Code)
	}

	// unsubscribe ⇒ 204.
	rec = httptest.NewRecorder()
	h.unsubscribe(rec, pushReq(tenant, user, http.MethodDelete, "", map[string]string{"id": created.ID}))
	if rec.Code != http.StatusNoContent {
		t.Errorf("unsubscribe=%d", rec.Code)
	}

	// unsubscribe missing row ⇒ 404.
	rec = httptest.NewRecorder()
	h.unsubscribe(rec, pushReq(tenant, user, http.MethodDelete, "", map[string]string{"id": created.ID}))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unsubscribe missing=%d want 404", rec.Code)
	}
}

func TestPushHandlersRegisterDB(t *testing.T) {
	svc, _, _ := dbService(t)
	h := NewHandlers(svc, log.New(io.Discard, "", 0))
	authMW := middleware.MustNewOIDC(middleware.OIDCConfig{
		DevBypassToken: "dev-secret",
		Env:            middleware.EnvDevelopment,
	})
	mux := http.NewServeMux()
	h.Register(mux, authMW)
	for _, p := range []string{
		"/api/v1/push/subscribe",
		"/api/v1/push/subscriptions",
		"/api/v1/push/preferences",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("route %s not mounted", p)
		}
	}
}

func TestPushStatusFor(t *testing.T) {
	if statusFor(ErrInvalidInput) != http.StatusBadRequest {
		t.Error("ErrInvalidInput → 400")
	}
	if statusFor(ErrNotFound) != http.StatusNotFound {
		t.Error("ErrNotFound → 404")
	}
	if statusFor(errors.New("x")) != http.StatusInternalServerError {
		t.Error("other → 500")
	}
}
