package jmap

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseStalwartVersion(t *testing.T) {
	cases := []struct {
		in   string
		want StalwartVersion
		err  bool
	}{
		{"v0.16.0", StalwartVersion{Major: 0, Minor: 16, Patch: 0, Raw: "v0.16.0"}, false},
		{"1.0.0", StalwartVersion{Major: 1, Minor: 0, Patch: 0, Raw: "1.0.0"}, false},
		{"v1.0.0-rc.1", StalwartVersion{Major: 1, Minor: 0, Patch: 0, Raw: "v1.0.0-rc.1"}, false},
		{"v0.17", StalwartVersion{Major: 0, Minor: 17, Raw: "v0.17"}, false},
		{"abc", StalwartVersion{}, true},
	}
	for _, c := range cases {
		got, err := ParseStalwartVersion(c.in)
		if c.err {
			if err == nil {
				t.Errorf("%s: expected error", c.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if got.Major != c.want.Major || got.Minor != c.want.Minor || got.Patch != c.want.Patch {
			t.Errorf("%s: got %+v want %+v", c.in, got, c.want)
		}
	}
}

func TestAdapter_AdaptCapabilityURN(t *testing.T) {
	v0 := Adapter{Version: StalwartVersion{Major: 0, Minor: 16}}
	v1 := Adapter{Version: StalwartVersion{Major: 1, Minor: 0}}
	if got := v0.AdaptCapabilityURN("urn:stalwart:mail"); got != "urn:stalwart:mail" {
		t.Errorf("v0: %q", got)
	}
	if got := v1.AdaptCapabilityURN("urn:stalwart:mail"); got != "urn:ietf:params:jmap:mail" {
		t.Errorf("v1: %q", got)
	}
	if got := v1.AdaptCapabilityURN("urn:ietf:params:jmap:core"); got != "urn:ietf:params:jmap:core" {
		t.Errorf("v1 passthrough: %q", got)
	}
}

func TestAdapter_AdaptAdminMethod(t *testing.T) {
	v0 := Adapter{Version: StalwartVersion{Major: 0, Minor: 16}}
	v1 := Adapter{Version: StalwartVersion{Major: 1, Minor: 0}}
	if got := v0.AdaptAdminMethod("x:Domain/set"); got != "x:Domain/set" {
		t.Errorf("v0: %q", got)
	}
	if got := v1.AdaptAdminMethod("x:Domain/set"); got != "urn:stalwart:admin:Domain/set" {
		t.Errorf("v1: %q", got)
	}
}

func TestVersionDetector_Detect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Server", "Stalwart/v0.16.0")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"capabilities":{}}`))
	}))
	defer srv.Close()
	d := NewVersionDetector()
	v, err := d.Detect(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if v.Major != 0 || v.Minor != 16 {
		t.Errorf("got %+v", v)
	}
	// Cached call should not hit the server again — change handler
	// to fail and expect cached value still returned.
	v2, err := d.Detect(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Detect cached: %v", err)
	}
	if v2.Raw != v.Raw {
		t.Errorf("cache miss: %v vs %v", v2, v)
	}
}

func TestVersionDetector_DetectV1Body(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"stalwartVersion":"v1.0.0"}`))
	}))
	defer srv.Close()
	d := NewVersionDetector()
	v, err := d.Detect(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !v.IsV1() {
		t.Errorf("expected v1, got %+v", v)
	}
}
