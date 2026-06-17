package dns

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func sampleSettings() AutoconfigSettings {
	return AutoconfigSettings{
		Domain:      "example.com",
		DisplayName: "KMail — example.com",
		IMAPHost:    "imap.kmail.test",
		IMAPPort:    993,
		SMTPHost:    "smtp.kmail.test",
		SMTPPort:    587,
		// Calendar on the default 443; contacts on a non-standard
		// port to prove the port is parsed back out of the URL.
		CalDAVURL:  "https://dav.kmail.test/dav/cal/",
		CardDAVURL: "https://dav.kmail.test:8443/dav/card/",
	}
}

// payloadByType finds the payload dict with the given PayloadType.
func payloadByType(t *testing.T, root map[string]any, typ string) map[string]any {
	t.Helper()
	content, ok := root["PayloadContent"].([]any)
	if !ok {
		t.Fatalf("PayloadContent is %T, want []any", root["PayloadContent"])
	}
	for _, item := range content {
		d, ok := item.(map[string]any)
		if ok && d["PayloadType"] == typ {
			return d
		}
	}
	return nil
}

func TestAppleMobileConfigStructure(t *testing.T) {
	email := "alice@example.com"
	body, err := AppleMobileConfig(email, sampleSettings(), "KMail")
	if err != nil {
		t.Fatalf("AppleMobileConfig: %v", err)
	}
	root := parsePlist(t, body)

	if root["PayloadType"] != "Configuration" {
		t.Fatalf("root PayloadType = %v, want Configuration", root["PayloadType"])
	}
	if root["PayloadOrganization"] != "KMail" {
		t.Fatalf("PayloadOrganization = %v, want KMail", root["PayloadOrganization"])
	}

	mail := payloadByType(t, root, "com.apple.mail.managed")
	if mail == nil {
		t.Fatal("missing mail payload")
	}
	checks := map[string]any{
		"EmailAddress":                 email,
		"EmailAccountName":             email,
		"IncomingMailServerHostName":   "imap.kmail.test",
		"IncomingMailServerPortNumber": 993,
		"IncomingMailServerUseSSL":     true,
		"IncomingMailServerUsername":   email,
		"OutgoingMailServerHostName":   "smtp.kmail.test",
		"OutgoingMailServerPortNumber": 587,
		"OutgoingMailServerUseSSL":     true,
	}
	for k, want := range checks {
		if mail[k] != want {
			t.Errorf("mail[%q] = %v (%T), want %v", k, mail[k], mail[k], want)
		}
	}

	cal := payloadByType(t, root, "com.apple.caldav.account")
	if cal == nil {
		t.Fatal("missing caldav payload")
	}
	if cal["CalDAVHostName"] != "dav.kmail.test" || cal["CalDAVPort"] != 443 || cal["CalDAVUseSSL"] != true {
		t.Errorf("caldav host/port/ssl = %v/%v/%v, want dav.kmail.test/443/true",
			cal["CalDAVHostName"], cal["CalDAVPort"], cal["CalDAVUseSSL"])
	}
	if cal["CalDAVUsername"] != email {
		t.Errorf("caldav username = %v, want %s", cal["CalDAVUsername"], email)
	}

	card := payloadByType(t, root, "com.apple.carddav.account")
	if card == nil {
		t.Fatal("missing carddav payload")
	}
	// 8443 must be parsed out of the URL — the load-bearing proof
	// that CalDAVURL / CardDAVURL are genuinely consumed.
	if card["CardDAVHostName"] != "dav.kmail.test" || card["CardDAVPort"] != 8443 || card["CardDAVUseSSL"] != true {
		t.Errorf("carddav host/port/ssl = %v/%v/%v, want dav.kmail.test/8443/true",
			card["CardDAVHostName"], card["CardDAVPort"], card["CardDAVUseSSL"])
	}
}

func TestAppleMobileConfigSkipsDAVWhenURLEmpty(t *testing.T) {
	s := sampleSettings()
	s.CalDAVURL = ""
	s.CardDAVURL = ""
	body, err := AppleMobileConfig("bob@example.com", s, "")
	if err != nil {
		t.Fatalf("AppleMobileConfig: %v", err)
	}
	root := parsePlist(t, body)
	if payloadByType(t, root, "com.apple.caldav.account") != nil {
		t.Error("caldav payload should be absent when CalDAVURL is empty")
	}
	if payloadByType(t, root, "com.apple.carddav.account") != nil {
		t.Error("carddav payload should be absent when CardDAVURL is empty")
	}
	if payloadByType(t, root, "com.apple.mail.managed") == nil {
		t.Error("mail payload must always be present")
	}
}

// TestAppleMobileConfigEscaping feeds attacker-controlled markup
// through the email + display name. The profile is rendered from
// unauthenticated public input, so a breakout would be a real
// injection bug. We assert the document still parses (no breakout)
// and the values round-trip exactly.
func TestAppleMobileConfigEscaping(t *testing.T) {
	s := sampleSettings()
	s.DisplayName = `Ev&il "<Corp>"`
	email := `a"<x>&@example.com`
	body, err := AppleMobileConfig(email, s, "KMail")
	if err != nil {
		t.Fatalf("AppleMobileConfig: %v", err)
	}
	if bytes.Contains(body, []byte("<x>")) {
		t.Fatalf("unescaped injected markup present in:\n%s", body)
	}
	root := parsePlist(t, body) // parse failure here = malformed/broken-out XML
	if root["PayloadDisplayName"] != s.DisplayName {
		t.Errorf("PayloadDisplayName = %q, want %q", root["PayloadDisplayName"], s.DisplayName)
	}
	mail := payloadByType(t, root, "com.apple.mail.managed")
	if mail["EmailAddress"] != email {
		t.Errorf("EmailAddress = %q, want %q", mail["EmailAddress"], email)
	}
}

func TestAppleMobileConfigDeterministicUUIDs(t *testing.T) {
	s := sampleSettings()
	a, err := AppleMobileConfig("alice@example.com", s, "KMail")
	if err != nil {
		t.Fatalf("AppleMobileConfig: %v", err)
	}
	b, err := AppleMobileConfig("alice@example.com", s, "KMail")
	if err != nil {
		t.Fatalf("AppleMobileConfig: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("same inputs produced different profiles; UUIDs are not deterministic")
	}
	c, err := AppleMobileConfig("carol@example.com", s, "KMail")
	if err != nil {
		t.Fatalf("AppleMobileConfig: %v", err)
	}
	ua := parsePlist(t, a)["PayloadUUID"]
	uc := parsePlist(t, c)["PayloadUUID"]
	if ua == uc {
		t.Fatalf("different emails produced identical PayloadUUID %v", ua)
	}
}

func TestDavEndpoint(t *testing.T) {
	cases := []struct {
		raw    string
		host   string
		port   int
		useSSL bool
		ok     bool
	}{
		{"", "", 0, false, false},
		{"https://dav.kmail.test/dav/cal/", "dav.kmail.test", 443, true, true},
		{"https://dav.kmail.test:8443/dav/card/", "dav.kmail.test", 8443, true, true},
		{"http://dav.kmail.test/dav/cal/", "dav.kmail.test", 80, false, true},
		{"http://dav.kmail.test:8080/", "dav.kmail.test", 8080, false, true},
		{"mailto:nobody@example.com", "", 0, false, false}, // no host
		{"https://%zz/bad", "", 0, false, false},           // unparseable escape
	}
	for _, c := range cases {
		host, port, useSSL, ok := davEndpoint(c.raw)
		if host != c.host || port != c.port || useSSL != c.useSSL || ok != c.ok {
			t.Errorf("davEndpoint(%q) = (%q,%d,%v,%v), want (%q,%d,%v,%v)",
				c.raw, host, port, useSSL, ok, c.host, c.port, c.useSSL, c.ok)
		}
	}
}

func TestAppleProfileHandler(t *testing.T) {
	svc := NewAutoconfigService(AutoconfigConfig{
		IMAPHost: "imap.kmail.test", IMAPPort: 993,
		SMTPHost: "smtp.kmail.test", SMTPPort: 587,
		CalDAVHost: "dav.kmail.test", CalDAVPort: 443,
		BrandName: "KMail",
	})
	mux := http.NewServeMux()
	NewAutoconfigHandlers(svc, nil).Register(mux)

	// Missing emailaddress → 400.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/email.mobileconfig", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing emailaddress = %d, want 400", rec.Code)
	}

	// Happy path: no pool → defaults for any domain.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/email.mobileconfig?emailaddress=u@example.com", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("mobileconfig = %d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != AppleConfigContentType {
		t.Errorf("Content-Type = %q, want %q", ct, AppleConfigContentType)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	root := parsePlist(t, rec.Body.Bytes())
	if payloadByType(t, root, "com.apple.mail.managed") == nil {
		t.Error("served profile missing mail payload")
	}
	if payloadByType(t, root, "com.apple.caldav.account") == nil {
		t.Error("served profile missing caldav payload (CalDAVHost configured)")
	}
}

// --- minimal XML-plist parser (test-only) -------------------------
// Doubles as a well-formedness check: a malformed or broken-out
// document makes the decoder error, failing the test.

func parsePlist(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			t.Fatal("parsePlist: no <dict> found")
		}
		if err != nil {
			t.Fatalf("parsePlist: %v", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			return parsePlistDict(t, dec)
		}
	}
}

func parsePlistDict(t *testing.T, dec *xml.Decoder) map[string]any {
	t.Helper()
	m := map[string]any{}
	var key string
	haveKey := false
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("parsePlistDict: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if el.Name.Local == "key" {
				key = readPlistText(t, dec, "key")
				haveKey = true
				continue
			}
			if !haveKey {
				t.Fatalf("parsePlistDict: value <%s> without key", el.Name.Local)
			}
			m[key] = parsePlistValue(t, dec, el)
			haveKey = false
		case xml.EndElement:
			if el.Name.Local == "dict" {
				return m
			}
		}
	}
}

func parsePlistValue(t *testing.T, dec *xml.Decoder, start xml.StartElement) any {
	t.Helper()
	switch start.Name.Local {
	case "string":
		return readPlistText(t, dec, "string")
	case "integer":
		n, err := strconv.Atoi(readPlistText(t, dec, "integer"))
		if err != nil {
			t.Fatalf("parsePlistValue: integer: %v", err)
		}
		return n
	case "true", "false":
		consumePlistEnd(t, dec, start.Name.Local)
		return start.Name.Local == "true"
	case "dict":
		return parsePlistDict(t, dec)
	case "array":
		var out []any
		for {
			tok, err := dec.Token()
			if err != nil {
				t.Fatalf("parsePlistValue: array: %v", err)
			}
			switch el := tok.(type) {
			case xml.StartElement:
				out = append(out, parsePlistValue(t, dec, el))
			case xml.EndElement:
				if el.Name.Local == "array" {
					return out
				}
			}
		}
	default:
		t.Fatalf("parsePlistValue: unknown type %s", start.Name.Local)
		return nil
	}
}

func readPlistText(t *testing.T, dec *xml.Decoder, name string) string {
	t.Helper()
	var sb strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("readPlistText(%s): %v", name, err)
		}
		switch el := tok.(type) {
		case xml.CharData:
			sb.Write(el)
		case xml.EndElement:
			if el.Name.Local == name {
				return sb.String()
			}
		}
	}
}

func consumePlistEnd(t *testing.T, dec *xml.Decoder, name string) {
	t.Helper()
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("consumePlistEnd(%s): %v", name, err)
		}
		if el, ok := tok.(xml.EndElement); ok && el.Name.Local == name {
			return
		}
	}
}
