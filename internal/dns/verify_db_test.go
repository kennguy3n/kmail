package dns

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/kennguy3n/kmail/internal/testsupport"
)

// TestVerifyDomainFullDB drives VerifyDomain end-to-end against a
// live DB with a fake resolver that publishes all four records, so
// the verification passes and the persisted flags flip to true.
func TestVerifyDomainFullDB(t *testing.T) {
	pool := testsupport.Pool(t)
	tenant := testsupport.SeedTenant(t, pool, "pro", "active")
	dkimSvc := NewDKIMRotationService(pool, nil)
	domainID := seedDomain(t, dkimSvc, tenant)

	// Recover the seeded domain name so the resolver can answer for it.
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
		Pool: pool, Resolver: fake,
		MailHost: "mx.kmail.example", SPFInclude: "_spf.kmail.example",
		DefaultDKIMSelector: "kmail", DKIMPublicKey: "PUBKEY",
		DMARCPolicy: "quarantine", ReportingMailbox: "dmarc@kmail.example",
	})

	res, err := svc.VerifyDomain(context.Background(), tenant, domainID)
	if err != nil {
		t.Fatalf("VerifyDomain: %v", err)
	}
	if !res.Verified || !res.MXVerified || !res.SPFVerified || !res.DKIMVerified || !res.DMARCVerified {
		t.Fatalf("expected all checks verified, got %+v", res)
	}

	// Cross-tenant / missing domain → ErrNotFound.
	if _, err := svc.VerifyDomain(context.Background(), tenant, "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("VerifyDomain missing domain=%v want ErrNotFound", err)
	}
}

func TestDNSStatusMappers(t *testing.T) {
	cases := []struct {
		err  error
		dns  int
		dkim int
	}{
		{ErrInvalidInput, http.StatusBadRequest, http.StatusBadRequest},
		{ErrNotFound, http.StatusNotFound, http.StatusNotFound},
		{errors.New("boom"), http.StatusInternalServerError, http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := statusForDNSError(c.err); got != c.dns {
			t.Errorf("statusForDNSError(%v)=%d want %d", c.err, got, c.dns)
		}
		if got := statusForDKIM(c.err); got != c.dkim {
			t.Errorf("statusForDKIM(%v)=%d want %d", c.err, got, c.dkim)
		}
	}
}

func TestDKIMOptionSetters(t *testing.T) {
	svc := NewDKIMRotationService(nil, nil)
	if svc.WithPusher(nil) != svc {
		t.Error("WithPusher should return the receiver for chaining")
	}
	if svc.WithEnvelope(nil) != svc {
		t.Error("WithEnvelope should return the receiver for chaining")
	}
}
