// deliverability-check exercises everything in KMail's email
// authentication stack that can be validated WITHOUT real IP pools,
// PTR records, or provider mailboxes — i.e. the parts that do not
// require sending to Gmail/Microsoft/Yahoo and reading the verdict.
//
// It drives the *production* code paths (internal/dns):
//
//  1. generates a real 2048-bit DKIM key pair via the same
//     DKIMRotationService the BFF uses;
//  2. generates the published SPF / DKIM / DMARC / MTA-STS / MX zone
//     records via dns.Service.GenerateRecords;
//  3. validates each record's syntax against the relevant RFC shape;
//  4. proves *key consistency*: signs a digest with the generated
//     private key and verifies it against the public key parsed back
//     out of the published DKIM TXT record — the single most common
//     cause of a DKIM "fail" at a provider is a record that does not
//     match the signing key, and this catches exactly that.
//
// What it does NOT do (requires real infra — documented as the
// follow-up in docs/BENCHMARKS.md): send through a warmed IP pool
// with valid PTR and measure inbox placement / provider pass rates.
//
// Usage:
//
//	go run ./scripts/loadtest/deliverability-check.go \
//	  --domain acme.example --mail-host mx.kmail.app \
//	  --spf-include _spf.kmail.app --dmarc-policy reject \
//	  --reporting dmarc@kmail.app --md-out deliverability.md
//
// Exits non-zero if any local check fails, so it can gate CI.
//
//go:build ignore

package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/kennguy3n/kmail/internal/dns"
)

type check struct {
	Name   string
	Pass   bool
	Detail string
}

func main() {
	var (
		domain    string
		mailHost  string
		spfInc    string
		selector  string
		dmarcPol  string
		reporting string
		mdOut     string
	)
	flag.StringVar(&domain, "domain", "acme.example", "domain to generate + validate records for")
	flag.StringVar(&mailHost, "mail-host", "mx.kmail.app", "KMail mail host (MX / autoconfig target)")
	flag.StringVar(&spfInc, "spf-include", "_spf.kmail.app", "SPF include host")
	flag.StringVar(&selector, "dkim-selector", "kmail", "DKIM selector")
	flag.StringVar(&dmarcPol, "dmarc-policy", "reject", "DMARC policy (none|quarantine|reject)")
	flag.StringVar(&reporting, "reporting", "dmarc-reports@kmail.app", "DMARC rua/ruf reporting mailbox")
	flag.StringVar(&mdOut, "md-out", "", "write a Markdown report here (default stdout)")
	flag.Parse()

	logger := log.New(os.Stderr, "", 0)

	// 1. Real DKIM key pair from the production rotation service.
	rot := dns.NewDKIMRotationService(nil, logger)
	pair, err := rot.GenerateKeyPair()
	if err != nil {
		fmt.Fprintf(os.Stderr, "deliverability-check: generate dkim key: %v\n", err)
		os.Exit(1)
	}

	// 2. Published zone records from the production generator, with
	// the freshly generated public key spliced in.
	svc := dns.NewService(dns.Config{
		MailHost:            mailHost,
		SPFInclude:          spfInc,
		DefaultDKIMSelector: selector,
		DKIMPublicKey:       pair.PublicKey,
		DMARCPolicy:         dmarcPol,
		ReportingMailbox:    reporting,
	})
	records := svc.GenerateRecords(domain)

	var spf, dkim, dmarc dns.DomainRecord
	var haveMX bool
	for _, r := range records.Records {
		switch {
		case r.Type == "MX":
			haveMX = true
		case r.Type == "TXT" && r.Name == domain && strings.HasPrefix(r.Value, "v=spf1"):
			spf = r
		case r.Type == "TXT" && strings.Contains(r.Name, "._domainkey."):
			dkim = r
		case r.Type == "TXT" && r.Name == "_dmarc."+domain:
			dmarc = r
		}
	}

	// 3. + 4. Validate.
	var checks []check
	checks = append(checks, check{"MX present", haveMX, mxDetail(haveMX, mailHost)})
	checks = append(checks, validateSPF(spf))
	checks = append(checks, validateDMARC(dmarc, dmarcPol))
	dkimPub, dkimCheck := validateDKIMRecord(dkim)
	checks = append(checks, dkimCheck)
	checks = append(checks, validateKeyConsistency(pair.PrivateKey, dkimPub))

	report := render(domain, selector, records, checks)
	if mdOut != "" {
		if err := os.WriteFile(mdOut, []byte(report), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "deliverability-check: write md: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("deliverability-check: wrote %s\n", mdOut)
	} else {
		fmt.Print(report)
	}

	failed := 0
	for _, c := range checks {
		if !c.Pass {
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "deliverability-check: %d/%d local checks FAILED\n", failed, len(checks))
		os.Exit(1)
	}
}

func mxDetail(ok bool, host string) string {
	if ok {
		return "routes inbound to " + host
	}
	return "no MX generated (set --mail-host)"
}

func validateSPF(r dns.DomainRecord) check {
	if r.Value == "" {
		return check{"SPF syntax", false, "no SPF record generated"}
	}
	v := r.Value
	ok := strings.HasPrefix(v, "v=spf1 ") &&
		strings.Contains(v, "include:") &&
		(strings.HasSuffix(v, "~all") || strings.HasSuffix(v, "-all"))
	return check{"SPF syntax", ok, v}
}

func validateDMARC(r dns.DomainRecord, wantPolicy string) check {
	if r.Value == "" {
		return check{"DMARC syntax", false, "no DMARC record generated"}
	}
	v := r.Value
	ok := strings.HasPrefix(v, "v=DMARC1;") &&
		strings.Contains(v, "p="+wantPolicy) &&
		strings.Contains(v, "adkim=") &&
		strings.Contains(v, "aspf=")
	return check{"DMARC syntax", ok, v}
}

// validateDKIMRecord parses the `p=` base64-DER public key out of the
// published DKIM TXT value and returns it for the consistency check.
func validateDKIMRecord(r dns.DomainRecord) (*rsa.PublicKey, check) {
	if r.Value == "" {
		return nil, check{"DKIM record syntax", false, "no DKIM record generated"}
	}
	if !strings.HasPrefix(r.Value, "v=DKIM1;") || !strings.Contains(r.Value, "k=rsa;") {
		return nil, check{"DKIM record syntax", false, "missing v=DKIM1/k=rsa tags: " + r.Value}
	}
	p := extractTag(r.Value, "p=")
	if p == "" || strings.Contains(p, "<PUBLIC_KEY>") {
		return nil, check{"DKIM record syntax", false, "missing/placeholder p= public key"}
	}
	der, err := base64.StdEncoding.DecodeString(p)
	if err != nil {
		return nil, check{"DKIM record syntax", false, "p= is not valid base64: " + err.Error()}
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, check{"DKIM record syntax", false, "p= is not a valid PKIX key: " + err.Error()}
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, check{"DKIM record syntax", false, "p= is not an RSA key"}
	}
	return rsaPub, check{"DKIM record syntax", true, fmt.Sprintf("RSA-%d public key parses from published p=", rsaPub.N.BitLen())}
}

// validateKeyConsistency signs a digest with the generated private
// key and verifies it against the public key parsed from the DNS
// record. A mismatch is the #1 cause of provider DKIM failures.
func validateKeyConsistency(privPEM string, pub *rsa.PublicKey) check {
	const name = "DKIM key consistency"
	if pub == nil {
		return check{name, false, "no public key to verify against (DKIM record invalid)"}
	}
	block, _ := pem.Decode([]byte(privPEM))
	if block == nil {
		return check{name, false, "private key is not valid PEM"}
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return check{name, false, "private key is not valid PKCS#8: " + err.Error()}
	}
	priv, ok := key.(*rsa.PrivateKey)
	if !ok {
		return check{name, false, "private key is not RSA"}
	}
	digest := sha256.Sum256([]byte("kmail deliverability key-consistency probe"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		return check{name, false, "sign failed: " + err.Error()}
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig); err != nil {
		return check{name, false, "published public key does NOT match signing key: " + err.Error()}
	}
	return check{name, true, "signature made with private key verifies against published public key"}
}

func extractTag(record, tag string) string {
	for _, part := range strings.Split(record, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, tag) {
			return strings.TrimSpace(strings.TrimPrefix(part, tag))
		}
	}
	return ""
}

func render(domain, selector string, records dns.DomainRecords, checks []check) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Deliverability local validation — %s\n\n", domain)
	fmt.Fprintf(&b, "> Local-only: validates DKIM signing/keys and SPF/DMARC record generation. ")
	fmt.Fprintf(&b, "Inbox-placement at Gmail/Microsoft/Yahoo requires warmed IP pools + PTR + provider mailboxes (see prerequisites in docs/BENCHMARKS.md).\n\n")

	allPass := true
	fmt.Fprintf(&b, "## Checks\n\n| Check | Result | Detail |\n| --- | --- | --- |\n")
	for _, c := range checks {
		mark := "PASS"
		if !c.Pass {
			mark = "FAIL"
			allPass = false
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c.Name, mark, truncate(c.Detail, 90))
	}
	verdict := "ALL LOCAL CHECKS PASS"
	if !allPass {
		verdict = "ONE OR MORE LOCAL CHECKS FAILED"
	}
	fmt.Fprintf(&b, "\n**Verdict: %s** (DKIM selector `%s`)\n\n", verdict, selector)

	fmt.Fprintf(&b, "## Generated zone records\n\n```dns\n")
	for _, r := range records.Records {
		if r.Type == "MX" {
			fmt.Fprintf(&b, "%-44s %-5s %d %s\n", r.Name+".", r.Type, r.Priority, r.Value)
			continue
		}
		fmt.Fprintf(&b, "%-44s %-5s %q\n", r.Name+".", r.Type, r.Value)
	}
	fmt.Fprintf(&b, "```\n")
	return b.String()
}

// truncate shortens s to at most n runes, appending an ellipsis when it
// trims. It counts runes (not bytes) so multi-byte values are not cut
// mid-character, and guards n<=0 so it never panics on an empty budget.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
