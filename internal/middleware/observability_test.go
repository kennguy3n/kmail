package middleware

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestRequestLoggerText(t *testing.T) {
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	h := RequestLogger(lg, "text")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	req := httptest.NewRequest("GET", "/foo", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	if !strings.Contains(out, "GET /foo 418") {
		t.Fatalf("text log missing fields: %q", out)
	}
}

func TestRequestLoggerJSON(t *testing.T) {
	var buf bytes.Buffer
	lg := log.New(&buf, "", 0)
	h := RequestLogger(lg, "json")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	ctx := WithKChatUserID(WithTenantID(context.Background(), "tenant-x"), "user-y")
	ctx = WithRequestID(ctx, "req-123")
	req := httptest.NewRequest("POST", "/api/v1/things", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	out := buf.String()
	for _, want := range []string{`"status":201`, `"tenant_id":"tenant-x"`, `"user_id":"user-y"`, `"request_id":"req-123"`, `"method":"POST"`} {
		if !strings.Contains(out, want) {
			t.Errorf("json log missing %q in %q", want, out)
		}
	}
}

func TestStatusRecorder(t *testing.T) {
	rr := httptest.NewRecorder()
	sr := &statusRecorder{ResponseWriter: rr, status: http.StatusOK}

	// WriteHeader records once; a second call is ignored.
	sr.WriteHeader(http.StatusBadGateway)
	sr.WriteHeader(http.StatusOK)
	if sr.status != http.StatusBadGateway {
		t.Errorf("status=%d want 502", sr.status)
	}

	// Flush + Unwrap should not panic and Unwrap returns the inner writer.
	sr.Flush()
	if sr.Unwrap() != rr {
		t.Error("Unwrap did not return the inner ResponseWriter")
	}

	// Write on a fresh recorder sets wroteHeader implicitly.
	rr2 := httptest.NewRecorder()
	sr2 := &statusRecorder{ResponseWriter: rr2, status: http.StatusOK}
	if _, err := sr2.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !sr2.wroteHeader {
		t.Error("Write did not flip wroteHeader")
	}
}

func TestInitTracing(t *testing.T) {
	// Empty endpoint → no-op shutdown, no error.
	shutdown, err := InitTracing(context.Background(), "kmail-test", "")
	if err != nil || shutdown == nil {
		t.Fatalf("InitTracing empty: shutdown nil=%v err=%v", shutdown == nil, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("noop shutdown: %v", err)
	}

	// URL form with explicit http scheme + path exercises the parsing
	// branches (insecure + WithURLPath). otlptracehttp.New does not dial
	// until first export, so this returns without a live collector.
	shutdown, err = InitTracing(context.Background(), "kmail-test", "http://localhost:4318/v1/traces")
	if err != nil || shutdown == nil {
		t.Fatalf("InitTracing url: shutdown nil=%v err=%v", shutdown == nil, err)
	}
	_ = shutdown(context.Background())

	// host:port form.
	shutdown, err = InitTracing(context.Background(), "kmail-test", "localhost:4318")
	if err != nil || shutdown == nil {
		t.Fatalf("InitTracing host:port: shutdown nil=%v err=%v", shutdown == nil, err)
	}
	_ = shutdown(context.Background())
}

func TestTracingMiddleware(t *testing.T) {
	// A real (in-process) tracer provider so spans carry valid IDs.
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	var sawTrace string
	h := TracingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawTrace = TraceIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	ctx := WithKChatUserID(WithTenantID(context.Background(), "tenant-z"), "user-z")
	req := httptest.NewRequest("GET", "/api/v1/ping", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if sawTrace == "" {
		t.Error("handler context had no trace id")
	}
	if rr.Header().Get("X-Trace-Id") == "" {
		t.Error("response missing X-Trace-Id header")
	}
}

func TestBuildLokiLine(t *testing.T) {
	ctx := WithKChatUserID(WithTenantID(context.Background(), "t1"), "u1")
	ctx = WithRequestID(ctx, "r1")
	req := httptest.NewRequest("GET", "/api/v1/tenants/123e4567-e89b-12d3-a456-426614174000/messages/42", nil).WithContext(ctx)

	line := BuildLokiLine(time.Now(), req, 503, 12*time.Millisecond, LokiLabels{Env: "prod"})
	if line.Job != "kmail-api" {
		t.Errorf("default job=%q", line.Job)
	}
	if line.Route != "/api/v1/tenants/:id/messages/:n" {
		t.Errorf("route=%q", line.Route)
	}
	if line.StatusCls != "5xx" || line.TenantID != "t1" || line.UserID != "u1" || line.RequestID != "r1" || line.Env != "prod" {
		t.Errorf("unexpected line: %+v", line)
	}
	s, err := line.AsJSON()
	if err != nil || !strings.Contains(s, `"status_class":"5xx"`) {
		t.Errorf("AsJSON=%q err=%v", s, err)
	}

	// Custom job label is preserved.
	if l := BuildLokiLine(time.Now(), req, 200, 0, LokiLabels{Job: "kmail-worker"}); l.Job != "kmail-worker" {
		t.Errorf("custom job=%q", l.Job)
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{
		100: "1xx", 200: "2xx", 301: "3xx", 404: "4xx", 500: "5xx", 42: "42",
	}
	for code, want := range cases {
		if got := statusClass(code); got != want {
			t.Errorf("statusClass(%d)=%q want %q", code, got, want)
		}
	}
}

func TestRouteForAndHelpers(t *testing.T) {
	if routeFor("") != "/" {
		t.Error("empty path should map to /")
	}
	if got := routeFor("/a/00000000-0000-0000-0000-000000000000/b/7"); got != "/a/:id/b/:n" {
		t.Errorf("routeFor=%q", got)
	}
	if !isUUID("123e4567-e89b-12d3-a456-426614174000") {
		t.Error("valid uuid not recognized")
	}
	for _, bad := range []string{"short", "123e4567xe89bx12d3xa456x426614174000", "zzze4567-e89b-12d3-a456-426614174000"} {
		if isUUID(bad) {
			t.Errorf("isUUID(%q) should be false", bad)
		}
	}
	if !isAllDigits("12345") || isAllDigits("") || isAllDigits("12a") {
		t.Error("isAllDigits wrong")
	}
}

// stubSLO records the last RecordRequest call.
type stubSLO struct {
	calls   int
	success bool
	tenant  string
}

func (s *stubSLO) RecordRequest(_ context.Context, tenantID string, success bool, _ int64) {
	s.calls++
	s.success = success
	s.tenant = tenantID
}

func TestMetricsMiddleware(t *testing.T) {
	m := NewMetrics()
	slo := &stubSLO{}
	m = m.WithSLO(slo)

	h := m.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	ctx := WithTenantID(context.Background(), "tenant-m")
	req := httptest.NewRequest("GET", "/api/v1/x", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if slo.calls != 1 || slo.success || slo.tenant != "tenant-m" {
		t.Errorf("SLO not recorded correctly: %+v", slo)
	}

	// /metrics handler exposes the counters we just incremented.
	metricsRR := httptest.NewRecorder()
	m.Handler().ServeHTTP(metricsRR, httptest.NewRequest("GET", "/metrics", nil))
	body := metricsRR.Body.String()
	if !strings.Contains(body, "kmail_http_requests_total") {
		t.Errorf("/metrics missing counter: %s", body[:min(len(body), 200)])
	}
}

func TestTruncateLabel(t *testing.T) {
	short := "/api/v1/x"
	if truncateLabel(short) != short {
		t.Error("short label should be unchanged")
	}
	long := strings.Repeat("a", pathLabelLimit+50)
	if len(truncateLabel(long)) != pathLabelLimit {
		t.Errorf("long label not truncated to %d", pathLabelLimit)
	}
}
