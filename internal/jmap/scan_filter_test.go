package jmap

import (
	"net/http/httptest"
	"testing"
)

func TestRequestCarriesMessageContent(t *testing.T) {
	cases := map[string]bool{
		"/jmap":                 true,
		"/jmap/":                true,
		"/jmap/upload":          true,
		"/jmap/upload/abc-123":  true,
		"/api/v1/jmap/upload":   true,
		"/api/v1/jmap":          true,
		"/api/v1/jmap/":         true,
		"/api/v1/health":        false,
		"/.well-known/jmap":     false,
		"/api/v1/tenants":       false,
	}
	for path, want := range cases {
		r := httptest.NewRequest("POST", path, nil)
		got := requestCarriesMessageContent(r)
		if got != want {
			t.Errorf("requestCarriesMessageContent(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestShouldScanBodyOnlyOnSubmitMethods(t *testing.T) {
	upload := httptest.NewRequest("POST", "/jmap/upload/blob-1", nil)
	if !shouldScanBody(upload, []byte(`{"any":"body"}`)) {
		t.Errorf("upload path must always scan")
	}

	jmap := httptest.NewRequest("POST", "/jmap", nil)
	cases := map[string]bool{
		`{"using":["urn:..."],"methodCalls":[["Email/get",{},"#0"]]}`:           false,
		`{"methodCalls":[["Mailbox/get",{},"#0"]]}`:                              false,
		`{"methodCalls":[["Email/query",{"filter":{}},"#0"]]}`:                   false,
		`{"methodCalls":[["Thread/get",{},"#0"]]}`:                               false,
		`{"methodCalls":[["Email/set",{"create":{"k":{}}},"#0"]]}`:               true,
		`{"methodCalls":[["EmailSubmission/set",{"create":{"k":{}}},"#0"]]}`:     true,
		`{"methodCalls":[["EmailSubmission/create",{"emailId":"a"},"#0"]]}`:      true,
	}
	for body, want := range cases {
		got := shouldScanBody(jmap, []byte(body))
		if got != want {
			t.Errorf("shouldScanBody(/jmap, %q) = %v, want %v", body, got, want)
		}
	}
}
