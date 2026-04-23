package dns

import (
	"context"
	"errors"
	"net"
	"testing"
)

// fakeResolver implements the Resolver interface with in-memory
// maps so tests exercise DNS logic without hitting real DNS.
type fakeResolver struct {
	mx  map[string][]*net.MX
	txt map[string][]string
	// If set, LookupMX and LookupTXT return this error when a name
	// is missing. Defaults to a *net.DNSError with IsNotFound=true
	// (the "no such record" case the Service treats as a benign
	// negative result).
	notFoundErr error
}

func (f *fakeResolver) LookupMX(_ context.Context, name string) ([]*net.MX, error) {
	if v, ok := f.mx[name]; ok {
		return v, nil
	}
	return nil, f.notFound(name)
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if v, ok := f.txt[name]; ok {
		return v, nil
	}
	return nil, f.notFound(name)
}

func (f *fakeResolver) notFound(name string) error {
	if f.notFoundErr != nil {
		return f.notFoundErr
	}
	return &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
}

func newService(fake *fakeResolver) *Service {
	return NewService(Config{
		Resolver:            fake,
		MailHost:            "mx.kmail.example",
		SPFInclude:          "_spf.kmail.example",
		DefaultDKIMSelector: "kmail",
		DKIMPublicKey:       "PUBKEY",
		DMARCPolicy:         "quarantine",
		ReportingMailbox:    "dmarc@kmail.example",
	})
}

// ---------------------------------------------------------------
// CheckMX
// ---------------------------------------------------------------

func TestCheckMX_EmptyDomain(t *testing.T) {
	_, err := newService(&fakeResolver{}).CheckMX(context.Background(), "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestCheckMX_PointsAtMailHost(t *testing.T) {
	svc := newService(&fakeResolver{
		mx: map[string][]*net.MX{
			"acme.example": {{Host: "mx.kmail.example.", Pref: 10}},
		},
	})
	ok, err := svc.CheckMX(context.Background(), "acme.example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected MX check to pass for direct match")
	}
}

func TestCheckMX_PointsAtSubdomainOfMailHost(t *testing.T) {
	svc := newService(&fakeResolver{
		mx: map[string][]*net.MX{
			"acme.example": {{Host: "us.mx.kmail.example.", Pref: 10}},
		},
	})
	ok, err := svc.CheckMX(context.Background(), "acme.example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected MX check to pass for subdomain match")
	}
}

func TestCheckMX_WrongTarget(t *testing.T) {
	svc := newService(&fakeResolver{
		mx: map[string][]*net.MX{
			"acme.example": {{Host: "mail.other.example.", Pref: 10}},
		},
	})
	ok, err := svc.CheckMX(context.Background(), "acme.example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected MX check to fail for wrong target")
	}
}

func TestCheckMX_NoRecords(t *testing.T) {
	svc := newService(&fakeResolver{})
	ok, err := svc.CheckMX(context.Background(), "acme.example")
	if err != nil {
		t.Fatalf("unexpected error for not-found: %v", err)
	}
	if ok {
		t.Error("expected MX check to fail when no MX records")
	}
}

// ---------------------------------------------------------------
// CheckSPF
// ---------------------------------------------------------------

func TestCheckSPF_HasInclude(t *testing.T) {
	svc := newService(&fakeResolver{
		txt: map[string][]string{
			"acme.example": {"v=spf1 include:_spf.kmail.example ~all"},
		},
	})
	ok, err := svc.CheckSPF(context.Background(), "acme.example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected SPF check to pass")
	}
}

func TestCheckSPF_MissingInclude(t *testing.T) {
	svc := newService(&fakeResolver{
		txt: map[string][]string{
			"acme.example": {"v=spf1 include:_spf.other.example ~all"},
		},
	})
	ok, err := svc.CheckSPF(context.Background(), "acme.example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected SPF check to fail without KMail include")
	}
}

func TestCheckSPF_NonSPFTXT(t *testing.T) {
	svc := newService(&fakeResolver{
		txt: map[string][]string{
			"acme.example": {"google-site-verification=abc"},
		},
	})
	ok, err := svc.CheckSPF(context.Background(), "acme.example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected SPF check to ignore non-SPF TXT")
	}
}

// ---------------------------------------------------------------
// CheckDKIM
// ---------------------------------------------------------------

func TestCheckDKIM_ValidKey(t *testing.T) {
	svc := newService(&fakeResolver{
		txt: map[string][]string{
			"kmail._domainkey.acme.example": {"v=DKIM1; k=rsa; p=MIGfMA0GCSqGSIb3..."},
		},
	})
	ok, err := svc.CheckDKIM(context.Background(), "acme.example", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected DKIM check to pass with valid key")
	}
}

func TestCheckDKIM_ExplicitSelector(t *testing.T) {
	svc := newService(&fakeResolver{
		txt: map[string][]string{
			"custom._domainkey.acme.example": {"v=DKIM1; k=rsa; p=ABC123"},
		},
	})
	ok, err := svc.CheckDKIM(context.Background(), "acme.example", "custom")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected DKIM check to pass with explicit selector")
	}
}

func TestCheckDKIM_EmptyPublicKey(t *testing.T) {
	svc := newService(&fakeResolver{
		txt: map[string][]string{
			"kmail._domainkey.acme.example": {"v=DKIM1; k=rsa; p="},
		},
	})
	ok, err := svc.CheckDKIM(context.Background(), "acme.example", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected DKIM check to fail with empty p= tag (revoked key)")
	}
}

func TestCheckDKIM_MissingRecord(t *testing.T) {
	svc := newService(&fakeResolver{})
	ok, err := svc.CheckDKIM(context.Background(), "acme.example", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected DKIM check to fail when record missing")
	}
}

// ---------------------------------------------------------------
// CheckDMARC
// ---------------------------------------------------------------

func TestCheckDMARC_ValidRecord(t *testing.T) {
	svc := newService(&fakeResolver{
		txt: map[string][]string{
			"_dmarc.acme.example": {"v=DMARC1; p=none; rua=mailto:dmarc@acme.example"},
		},
	})
	ok, err := svc.CheckDMARC(context.Background(), "acme.example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Error("expected DMARC check to pass")
	}
}

func TestCheckDMARC_Missing(t *testing.T) {
	svc := newService(&fakeResolver{})
	ok, err := svc.CheckDMARC(context.Background(), "acme.example")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected DMARC check to fail when record missing")
	}
}

// ---------------------------------------------------------------
// GenerateRecords
// ---------------------------------------------------------------

func TestGenerateRecords_EmptyDomain(t *testing.T) {
	svc := newService(&fakeResolver{})
	got := svc.GenerateRecords("")
	if got.Domain != "" || len(got.Records) != 0 {
		t.Errorf("expected empty records for empty domain, got %+v", got)
	}
}

func TestGenerateRecords_ContainsExpectedTypes(t *testing.T) {
	svc := newService(&fakeResolver{})
	got := svc.GenerateRecords("acme.example")
	seen := map[string]int{}
	for _, r := range got.Records {
		seen[r.Type]++
	}
	if got.Domain != "acme.example" {
		t.Errorf("domain not echoed back: %q", got.Domain)
	}
	if seen["MX"] == 0 {
		t.Error("expected MX record")
	}
	if seen["TXT"] < 4 {
		// SPF, DKIM, DMARC, MTA-STS, TLS-RPT → ≥4 TXT records.
		t.Errorf("expected at least 4 TXT records, got %d", seen["TXT"])
	}
	if seen["CNAME"] < 2 {
		// mta-sts, autoconfig, autodiscover → ≥2 CNAMEs.
		t.Errorf("expected at least 2 CNAME records, got %d", seen["CNAME"])
	}
}

func TestGenerateRecords_SPFIncludesConfiguredInclude(t *testing.T) {
	svc := newService(&fakeResolver{})
	got := svc.GenerateRecords("acme.example")
	found := false
	for _, r := range got.Records {
		if r.Type == "TXT" && r.Name == "acme.example" && containsStr(r.Value, "include:_spf.kmail.example") {
			found = true
		}
	}
	if !found {
		t.Error("expected SPF record to include _spf.kmail.example")
	}
}

func TestGenerateRecords_DKIMUsesConfiguredPublicKey(t *testing.T) {
	svc := newService(&fakeResolver{})
	got := svc.GenerateRecords("acme.example")
	found := false
	for _, r := range got.Records {
		if r.Type == "TXT" && r.Name == "kmail._domainkey.acme.example" && containsStr(r.Value, "p=PUBKEY") {
			found = true
		}
	}
	if !found {
		t.Error("expected DKIM record to embed configured public key")
	}
}

// ---------------------------------------------------------------
// VerifyDomain — validates input handling
// ---------------------------------------------------------------

func TestVerifyDomain_EmptyIDs(t *testing.T) {
	svc := newService(&fakeResolver{})
	_, err := svc.VerifyDomain(context.Background(), "", "did")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty tenantID, got %v", err)
	}
	_, err = svc.VerifyDomain(context.Background(), "tid", "")
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for empty domainID, got %v", err)
	}
}

func TestVerifyDomain_NilPool(t *testing.T) {
	svc := newService(&fakeResolver{})
	// svc.pool is nil; we expect an error, not a panic.
	_, err := svc.VerifyDomain(context.Background(), "tid", "did")
	if err == nil {
		t.Error("expected error from nil pool")
	}
	if errors.Is(err, ErrInvalidInput) {
		t.Error("nil-pool error should not be ErrInvalidInput")
	}
}

// ---------------------------------------------------------------
// helpers
// ---------------------------------------------------------------

func containsStr(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
