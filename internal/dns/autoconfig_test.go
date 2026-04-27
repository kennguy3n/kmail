package dns

import (
	"context"
	"strings"
	"testing"
)

func TestSettingsForEmailDefaults(t *testing.T) {
	svc := NewAutoconfigService(AutoconfigConfig{})
	got, err := svc.SettingsForEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("SettingsForEmail: %v", err)
	}
	if got.Domain != "example.com" {
		t.Fatalf("Domain = %q, want example.com", got.Domain)
	}
	if got.IMAPHost == "" || got.SMTPHost == "" {
		t.Fatalf("expected default IMAP/SMTP hosts, got %+v", got)
	}
}

func TestMozillaXMLContainsServers(t *testing.T) {
	svc := NewAutoconfigService(AutoconfigConfig{
		IMAPHost: "imap.kmail.test", IMAPPort: 993,
		SMTPHost: "smtp.kmail.test", SMTPPort: 587,
	})
	settings, _ := svc.SettingsForEmail(context.Background(), "bob@example.com")
	body, err := MozillaXML("bob@example.com", *settings)
	if err != nil {
		t.Fatalf("MozillaXML: %v", err)
	}
	got := string(body)
	for _, want := range []string{"<incomingServer", "imap.kmail.test", "smtp.kmail.test", "bob@example.com"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Mozilla XML missing %q in:\n%s", want, got)
		}
	}
}

func TestOutlookXMLContainsServers(t *testing.T) {
	svc := NewAutoconfigService(AutoconfigConfig{
		IMAPHost: "imap.kmail.test", IMAPPort: 993,
		SMTPHost: "smtp.kmail.test", SMTPPort: 587,
	})
	settings, _ := svc.SettingsForEmail(context.Background(), "bob@example.com")
	body, err := OutlookXML("bob@example.com", *settings)
	if err != nil {
		t.Fatalf("OutlookXML: %v", err)
	}
	got := string(body)
	for _, want := range []string{"<Type>IMAP</Type>", "<Type>SMTP</Type>", "imap.kmail.test", "smtp.kmail.test"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Outlook XML missing %q in:\n%s", want, got)
		}
	}
}

func TestDomainFromEmail(t *testing.T) {
	cases := map[string]string{
		"a@b.com":            "b.com",
		"  c@D.example.com ": "d.example.com",
	}
	for in, want := range cases {
		got, err := domainFromEmail(strings.TrimSpace(in))
		if err != nil {
			t.Fatalf("domainFromEmail(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("domainFromEmail(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := domainFromEmail("invalid"); err == nil {
		t.Fatal("expected error for invalid email")
	}
}
