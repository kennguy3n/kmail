package jmap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestStalwartVersionString(t *testing.T) {
	// Raw is echoed verbatim when present.
	if got := (StalwartVersion{Raw: "v1.2.3-rc1", Major: 1, Minor: 2, Patch: 3}).String(); got != "v1.2.3-rc1" {
		t.Errorf("String with Raw=%q", got)
	}
	// Otherwise rendered from components.
	if got := (StalwartVersion{Major: 1, Minor: 0, Patch: 5}).String(); got != "1.0.5" {
		t.Errorf("String=%q want 1.0.5", got)
	}
}

func TestAdaptErrorEnvelope(t *testing.T) {
	a := Adapter{}
	// code present, type absent → type synthesised from code.
	out := a.AdaptErrorEnvelope(map[string]any{"code": 429, "description": "slow down"})
	if out["type"] != "429" {
		t.Errorf("type=%v want 429", out["type"])
	}
	if out["description"] != "slow down" {
		t.Errorf("description not preserved: %v", out["description"])
	}
	// type already present → preserved.
	out = a.AdaptErrorEnvelope(map[string]any{"type": "overQuota", "code": 7})
	if out["type"] != "overQuota" {
		t.Errorf("existing type overwritten: %v", out["type"])
	}
	// neither type nor code → unchanged, no panic.
	out = a.AdaptErrorEnvelope(map[string]any{"description": "x"})
	if _, ok := out["type"]; ok {
		t.Errorf("type should be absent: %v", out)
	}
}

// TestVersionDetectorServerInfoBody covers the serverInfo.version
// fallback branch (no top-level stalwartVersion field).
func TestVersionDetectorServerInfoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"serverInfo":{"version":"1.4.0"}}`))
	}))
	defer srv.Close()
	d := NewVersionDetector()
	v, err := d.Detect(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if v.Major != 1 || v.Minor != 4 {
		t.Errorf("got %+v", v)
	}
}

// TestVersionDetectorErrorCaching verifies a failed probe is cached
// for ErrorTTL: the second call returns the cached error without
// re-hitting the server.
func TestVersionDetectorErrorCaching(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		// No version anywhere → cacheError path.
		_, _ = w.Write([]byte(`{"capabilities":{}}`))
	}))
	defer srv.Close()
	d := NewVersionDetector()
	d.ErrorTTL = time.Minute
	if _, err := d.Detect(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error when no version present")
	}
	if _, err := d.Detect(context.Background(), srv.URL); err == nil {
		t.Fatal("expected cached error")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hit %d times, want 1 (error should be cached)", got)
	}
}

func TestVersionDetectorTTLDefaults(t *testing.T) {
	d := &VersionDetector{}
	if d.successTTL() != 5*time.Minute {
		t.Errorf("successTTL default=%v", d.successTTL())
	}
	if d.errorTTL() != 30*time.Second {
		t.Errorf("errorTTL default=%v", d.errorTTL())
	}
	d.TTL = time.Hour
	d.ErrorTTL = time.Second
	if d.successTTL() != time.Hour || d.errorTTL() != time.Second {
		t.Error("configured TTLs not honoured")
	}
}
