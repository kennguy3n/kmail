package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRateLimiter is an in-memory fixed-window counter standing in for
// the Valkey-backed RedisStore.
type fakeRateLimiter struct {
	mu     sync.Mutex
	counts map[string]int64
	err    error
}

func newFakeRateLimiter() *fakeRateLimiter {
	return &fakeRateLimiter{counts: map[string]int64{}}
}

func (f *fakeRateLimiter) IncrWithTTL(_ context.Context, key string, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.counts[key]++
	return f.counts[key], nil
}

func newTestHandlers(t *testing.T, limiter SignupRateLimiter) (*SignupHandlers, *fakeSignupRepo) {
	t.Helper()
	repo := newFakeSignupRepo()
	svc := newTestService(repo, newFakeProvisioner(), &fakeStripe{})
	h := NewSignupHandlers(SignupHandlersConfig{
		Service: svc,
		Limiter: limiter,
		Metrics: NewSignupMetrics(nil),
	})
	return h, repo
}

func doJSON(t *testing.T, h http.Handler, method, target, body, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestSignupHandler_InitiateValid(t *testing.T) {
	h, _ := newTestHandlers(t, nil)
	mux := http.NewServeMux()
	h.Register(mux)

	rr := doJSON(t, mux, http.MethodPost, "/api/v1/signup",
		`{"email":"founder@acme.com","org_name":"Acme Inc","plan":"pro"}`, "1.2.3.4:5555")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rr.Code, rr.Body.String())
	}
	var got SignupRequest
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.CheckoutURL == "" || got.ID == "" {
		t.Fatalf("response missing id/checkout url: %+v", got)
	}
}

func TestSignupHandler_InitiateInvalid(t *testing.T) {
	h, _ := newTestHandlers(t, nil)
	mux := http.NewServeMux()
	h.Register(mux)

	rr := doJSON(t, mux, http.MethodPost, "/api/v1/signup",
		`{"email":"nope","org_name":"Acme","plan":"core"}`, "1.2.3.4:5555")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestSignupHandler_RateLimited(t *testing.T) {
	limiter := newFakeRateLimiter()
	h, _ := newTestHandlers(t, limiter)
	mux := http.NewServeMux()
	h.Register(mux)

	const ip = "9.9.9.9:1000"
	body := `{"email":"a@acme.com","org_name":"Acme","plan":"core"}`
	// First 10 requests admitted (status 201).
	for i := 1; i <= 10; i++ {
		rr := doJSON(t, mux, http.MethodPost, "/api/v1/signup", body, ip)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly rate-limited", i)
		}
	}
	// 11th request in the same window must be rejected with 429.
	rr := doJSON(t, mux, http.MethodPost, "/api/v1/signup", body, ip)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("11th request status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}
}

func TestSignupHandler_RateLimitPerIP(t *testing.T) {
	limiter := newFakeRateLimiter()
	h, _ := newTestHandlers(t, limiter)
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"email":"a@acme.com","org_name":"Acme","plan":"core"}`
	// Exhaust one IP.
	for i := 0; i < 11; i++ {
		doJSON(t, mux, http.MethodPost, "/api/v1/signup", body, "5.5.5.5:1")
	}
	// A different IP must still be admitted.
	rr := doJSON(t, mux, http.MethodPost, "/api/v1/signup", body, "6.6.6.6:1")
	if rr.Code == http.StatusTooManyRequests {
		t.Fatal("second IP should not be rate-limited")
	}
}

func TestSignupHandler_RateLimiterFailsOpen(t *testing.T) {
	limiter := newFakeRateLimiter()
	limiter.err = context.DeadlineExceeded
	h, _ := newTestHandlers(t, limiter)
	mux := http.NewServeMux()
	h.Register(mux)

	rr := doJSON(t, mux, http.MethodPost, "/api/v1/signup",
		`{"email":"a@acme.com","org_name":"Acme","plan":"core"}`, "1.1.1.1:1")
	if rr.Code == http.StatusTooManyRequests {
		t.Fatal("limiter error should fail open, not reject")
	}
}

func TestSignupHandler_XForwardedFor(t *testing.T) {
	limiter := newFakeRateLimiter()
	h, _ := newTestHandlers(t, limiter) // default TrustedProxyDepth = 1
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"email":"a@acme.com","org_name":"Acme","plan":"core"}`
	// Behind a single ingress (depth 1) the ingress appends the real
	// client IP as the rightmost X-Forwarded-For hop. Distinct clients
	// therefore key on distinct rightmost hops and none are limited,
	// even though they share the same transport peer (the ingress).
	for i := 0; i < 12; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/signup", strings.NewReader(body))
		req.RemoteAddr = "10.0.0.1:9999" // ingress
		req.Header.Set("X-Forwarded-For", "203.0.113."+strconv.Itoa(i))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d limited despite distinct client IP", i)
		}
	}
}

// TestSignupHandler_XForwardedFor_SpoofResistant verifies that a single
// client behind the ingress cannot evade the per-IP limit by pre-seeding
// X-Forwarded-For with arbitrary (spoofed) leftmost hops. With one
// trusted proxy, the rate-limit identity is the hop the ingress
// appended (the real client), not the attacker-controlled prefix.
func TestSignupHandler_XForwardedFor_SpoofResistant(t *testing.T) {
	limiter := newFakeRateLimiter()
	h, _ := newTestHandlers(t, limiter) // default TrustedProxyDepth = 1
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"email":"a@acme.com","org_name":"Acme","plan":"core"}`
	// Attacker varies the spoofed leftmost hop every request; the ingress
	// always appends the same real client IP (198.51.100.7) as the last
	// hop. All 11 must share one bucket, so the 11th is rejected.
	var limited bool
	for i := 0; i < 11; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/signup", strings.NewReader(body))
		req.RemoteAddr = "10.0.0.1:9999" // ingress
		req.Header.Set("X-Forwarded-For", "203.0.113."+strconv.Itoa(i)+", 198.51.100.7")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("spoofed X-Forwarded-For prefix evaded the per-IP rate limit")
	}
}

// TestSignupHandler_TrustedProxyDepthZero verifies that an explicit depth
// of 0 (directly internet-facing, as documented on the config field) makes
// the handler ignore X-Forwarded-For entirely and key on the transport
// peer — i.e. a set 0 is honored, not silently promoted to the default 1.
func TestSignupHandler_TrustedProxyDepthZero(t *testing.T) {
	limiter := newFakeRateLimiter()
	repo := newFakeSignupRepo()
	svc := newTestService(repo, newFakeProvisioner(), &fakeStripe{})
	zero := 0
	h := NewSignupHandlers(SignupHandlersConfig{
		Service:           svc,
		Limiter:           limiter,
		Metrics:           NewSignupMetrics(nil),
		TrustedProxyDepth: &zero, // explicit 0 = ignore X-Forwarded-For
	})
	mux := http.NewServeMux()
	h.Register(mux)

	body := `{"email":"a@acme.com","org_name":"Acme","plan":"core"}`
	// Same transport peer, spoofed-distinct XFF: must still be limited.
	var limited bool
	for i := 0; i < 11; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/signup", strings.NewReader(body))
		req.RemoteAddr = "7.7.7.7:1234"
		req.Header.Set("X-Forwarded-For", "203.0.113."+strconv.Itoa(i))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Fatal("depth 0 must key on transport peer and rate-limit it")
	}
}

func TestSignupHandler_Status(t *testing.T) {
	h, repo := newTestHandlers(t, nil)
	mux := http.NewServeMux()
	h.Register(mux)

	// Seed a request via the service so it exists.
	req, err := h.svc.InitiateSignup(context.Background(), "a@acme.com", "Acme", "core")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	_ = repo

	rr := doJSON(t, mux, http.MethodGet, "/api/v1/signup/"+req.ID+"/status", "", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got SignupRequest
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != req.ID || got.Status != "pending" {
		t.Fatalf("got %+v, want id=%s status=pending", got, req.ID)
	}
}

func TestSignupHandler_StatusNotFound(t *testing.T) {
	h, _ := newTestHandlers(t, nil)
	mux := http.NewServeMux()
	h.Register(mux)

	rr := doJSON(t, mux, http.MethodGet, "/api/v1/signup/does-not-exist/status", "", "")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}
