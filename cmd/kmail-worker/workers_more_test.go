package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/kennguy3n/kmail/internal/config"
	"github.com/kennguy3n/kmail/internal/tenant"
	"github.com/kennguy3n/kmail/internal/testsupport"
)

func TestGetenvFloat(t *testing.T) {
	if got := getenvFloat("KMAIL_NONEXISTENT_FLOAT_XYZ", 0.85); got != 0.85 {
		t.Errorf("missing key: got %v want fallback 0.85", got)
	}
	t.Setenv("KMAIL_TEST_FLOAT", "0.42")
	if got := getenvFloat("KMAIL_TEST_FLOAT", 0.85); got != 0.42 {
		t.Errorf("set key: got %v want 0.42", got)
	}
	t.Setenv("KMAIL_TEST_FLOAT", "not-a-float")
	if got := getenvFloat("KMAIL_TEST_FLOAT", 0.85); got != 0.85 {
		t.Errorf("bad value: got %v want fallback 0.85", got)
	}
}

// TestReadyzHandlerHealthy exercises the success branch against the
// live test Postgres.
func TestReadyzHandlerHealthy(t *testing.T) {
	pool := testsupport.Pool(t)
	rec := httptest.NewRecorder()
	readyzHandler(pool)(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ready\n" {
		t.Errorf("body = %q want %q", rec.Body.String(), "ready\n")
	}
}

// TestBuildInternalJMAPMalwareAndHTTPSWarn covers the ClamAV-enabled
// branch and the HTTPS-without-mTLS warning path in dev.
func TestBuildInternalJMAPMalwareAndHTTPSWarn(t *testing.T) {
	t.Setenv("KMAIL_CLAMAV_ADDR", "127.0.0.1:3310") // enables malware hook (lazy dial)
	pool := testsupport.Pool(t)
	logger := log.New(io.Discard, "", 0)
	shardSvc := tenant.NewShardService(pool, logger)

	cfg := &config.Config{
		StalwartURL: "https://stalwart.internal:8080", // triggers HTTPS warning
		Env:         "development",
	}
	jc, _, err := buildInternalJMAP(context.Background(), cfg, pool, nil, shardSvc, logger)
	if err != nil {
		t.Fatalf("buildInternalJMAP (dev): %v", err)
	}
	if jc == nil {
		t.Fatal("expected non-nil InternalClient")
	}
}

// TestBuildInternalJMAPPartialMTLSFailClosed verifies the
// fail-closed behaviour: a partial mTLS config in a non-dev env is
// a fatal construction error.
func TestBuildInternalJMAPPartialMTLSFailClosed(t *testing.T) {
	// Use the shared test pool helper (skips when no test DB is
	// configured) rather than a hardcoded DSN, for parity with the
	// rest of the suite. The fail-closed path returns before the pool
	// is ever queried, but a valid pool keeps construction realistic.
	pool := testsupport.Pool(t)
	logger := log.New(io.Discard, "", 0)
	shardSvc := tenant.NewShardService(pool, logger)

	cfg := &config.Config{
		StalwartURL: "http://stalwart:8080",
		Env:         "production",
	}
	// Partial mTLS: cert without key → Validate() returns an error.
	cfg.StalwartMTLS.CertFile = "/tmp/cert.pem"

	if _, _, err := buildInternalJMAP(context.Background(), cfg, pool, nil, shardSvc, logger); err == nil {
		t.Fatal("expected fail-closed error for partial mTLS in production")
	}
}

// TestBuildInternalJMAPSharedBreaker covers the Valkey-reachable
// branch where the shared (Redis-backed) circuit breaker is wired in.
func TestBuildInternalJMAPSharedBreaker(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	defer mr.Close()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = client.Close() }()

	pool := testsupport.Pool(t)
	logger := log.New(io.Discard, "", 0)
	shardSvc := tenant.NewShardService(pool, logger)

	cfg := &config.Config{
		StalwartURL: "http://stalwart.internal:8080",
		ValkeyURL:   "redis://" + mr.Addr(),
		Env:         "development",
	}
	jc, _, err := buildInternalJMAP(context.Background(), cfg, pool, client, shardSvc, logger)
	if err != nil {
		t.Fatalf("buildInternalJMAP (shared breaker): %v", err)
	}
	if jc == nil {
		t.Fatal("expected non-nil InternalClient")
	}
}

// TestBuildInternalJMAPForceShared covers the forceShared branch: the
// Valkey ping fails but KMAIL_BREAKER_SHARED_FORCE=1 still wires the
// shared breaker.
func TestBuildInternalJMAPForceShared(t *testing.T) {
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	addr := mr.Addr()
	mr.Close() // make the client unreachable so Ping fails
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer func() { _ = client.Close() }()

	t.Setenv("KMAIL_BREAKER_SHARED_FORCE", "1")
	pool := testsupport.Pool(t)
	logger := log.New(io.Discard, "", 0)
	shardSvc := tenant.NewShardService(pool, logger)

	cfg := &config.Config{
		StalwartURL: "http://stalwart.internal:8080",
		ValkeyURL:   "redis://" + addr,
		Env:         "development",
	}
	jc, _, err := buildInternalJMAP(context.Background(), cfg, pool, client, shardSvc, logger)
	if err != nil {
		t.Fatalf("buildInternalJMAP (force shared): %v", err)
	}
	if jc == nil {
		t.Fatal("expected non-nil InternalClient")
	}
}
