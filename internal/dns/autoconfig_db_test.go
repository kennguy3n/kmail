package dns

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestVerifyDomainHandlerSuccessDB covers the verifyDomain handler
// success path (200) using a pool-backed service with a fake resolver
// that publishes all four records.
func TestVerifyDomainHandlerSuccessDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	dkimSvc := NewDKIMRotationService(pool, nil)
	domainID := seedDomain(t, dkimSvc, tenant)
	name, err := NewService(Config{Pool: pool}).LookupDomainName(context.Background(), tenant, domainID)
	if err != nil {
		t.Fatalf("LookupDomainName: %v", err)
	}
	fake := &fakeResolver{
		mx: map[string][]*net.MX{name: {{Host: "mx.kmail.example.", Pref: 10}}},
		txt: map[string][]string{
			name:                       {"v=spf1 include:_spf.kmail.example ~all"},
			"kmail._domainkey." + name: {"v=DKIM1; k=rsa; p=MIGfMA0GCSqGSIb3"},
			"_dmarc." + name:           {"v=DMARC1; p=quarantine; rua=mailto:dmarc@kmail.example"},
		},
	}
	svc := NewService(Config{
		Pool: pool, Resolver: fake, MailHost: "mx.kmail.example",
		SPFInclude: "_spf.kmail.example", DefaultDKIMSelector: "kmail",
		DKIMPublicKey: "PUBKEY", DMARCPolicy: "quarantine", ReportingMailbox: "dmarc@kmail.example",
	})
	h := NewHandlers(svc, nil)

	rec := httptest.NewRecorder()
	h.verifyDomain(rec, dnsReq(tenant, http.MethodPost, "", map[string]string{"id": tenant, "domainId": domainID}))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"verified":true`) {
		t.Fatalf("verifyDomain success=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestAutoconfigDBLookups covers SettingsForEmail's pool branch:
// a registered (but unverified) domain answers with a provisioning
// note, while an unknown domain surfaces ErrUnknownDomain → 404.
func TestAutoconfigDBLookups(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	dkimSvc := NewDKIMRotationService(pool, nil)
	domainID := seedDomain(t, dkimSvc, tenant)
	name, err := NewService(Config{Pool: pool}).LookupDomainName(context.Background(), tenant, domainID)
	if err != nil {
		t.Fatalf("LookupDomainName: %v", err)
	}

	svc := NewAutoconfigService(AutoconfigConfig{
		Pool:     pool,
		IMAPHost: "imap.kmail.test", IMAPPort: 993,
		SMTPHost: "smtp.kmail.test", SMTPPort: 587,
	})
	h := NewAutoconfigHandlers(svc, nil)
	mux := http.NewServeMux()
	h.Register(mux)

	// Registered but unverified domain → 200 (provisioning).
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mail/config-v1.1.xml?emailaddress=u@"+name, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("registered domain autoconfig=%d body=%s", rec.Code, rec.Body.String())
	}

	// Unknown domain → 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mail/config-v1.1.xml?emailaddress=u@nope-"+tenant[:8]+".invalid", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown domain autoconfig=%d want 404", rec.Code)
	}

	// Outlook path, unknown domain → 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/autodiscover/autodiscover.xml",
		strings.NewReader(`<Autodiscover><Request><EMailAddress>u@nope-`+tenant[:8]+`.invalid</EMailAddress></Request></Autodiscover>`)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown domain outlook=%d want 404", rec.Code)
	}
}
