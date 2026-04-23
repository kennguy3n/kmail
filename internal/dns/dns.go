// Package dns hosts the DNS Onboarding Service business logic:
// the DNS wizard that generates, validates, and monitors MX, SPF,
// DKIM, DMARC, MTA-STS, TLS-RPT, and autoconfig records for
// tenant-owned sending domains.
//
// See docs/ARCHITECTURE.md §7 and docs/PROPOSAL.md §9.3.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kmail/internal/middleware"
)

// Resolver abstracts the subset of `net.Resolver` the service calls
// so tests can inject deterministic responses without hitting real
// DNS.
type Resolver interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// Service exposes DNS onboarding operations: record generation for
// the DNS wizard and live verification of a tenant domain against
// its authoritative DNS.
type Service struct {
	pool     *pgxpool.Pool
	resolver Resolver
	// MailHost is the hostname tenant MX records must point at for
	// KMail to accept mail (e.g. `mx.kmail.example`). Verification
	// passes if any MX target is this host or a subdomain of it.
	MailHost string
	// SPFInclude is the SPF `include:` mechanism tenants must add
	// for KMail to be an authorized sender (e.g.
	// `_spf.kmail.example`).
	SPFInclude string
	// DefaultDKIMSelector is the selector used when no explicit
	// selector is provided to CheckDKIM. KMail defaults to
	// `kmail` per docs/PROPOSAL.md §9.3.
	DefaultDKIMSelector string
	// DKIMPublicKey is the base64-encoded DKIM public key KMail
	// publishes for the tenant (plumbed by the key-rotation
	// pipeline). Surfaced through GenerateRecords so the DNS
	// wizard can show the tenant the expected record.
	DKIMPublicKey string
	// DMARCPolicy is the policy tag (`none` / `quarantine` /
	// `reject`) surfaced through GenerateRecords.
	DMARCPolicy string
	// ReportingMailbox receives aggregate DMARC and TLS-RPT
	// reports (e.g. `dmarc@kmail.example`).
	ReportingMailbox string
}

// Config wires NewService.
type Config struct {
	Pool                *pgxpool.Pool
	Resolver            Resolver
	MailHost            string
	SPFInclude          string
	DefaultDKIMSelector string
	DKIMPublicKey       string
	DMARCPolicy         string
	ReportingMailbox    string
}

// NewService returns a Service with the provided configuration. A
// nil Resolver falls back to `net.DefaultResolver` so production
// callers can omit it; tests supply a fake resolver directly.
func NewService(cfg Config) *Service {
	resolver := cfg.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	selector := cfg.DefaultDKIMSelector
	if selector == "" {
		selector = "kmail"
	}
	policy := cfg.DMARCPolicy
	if policy == "" {
		policy = "none"
	}
	return &Service{
		pool:                cfg.Pool,
		resolver:            resolver,
		MailHost:            cfg.MailHost,
		SPFInclude:          cfg.SPFInclude,
		DefaultDKIMSelector: selector,
		DKIMPublicKey:       cfg.DKIMPublicKey,
		DMARCPolicy:         policy,
		ReportingMailbox:    cfg.ReportingMailbox,
	}
}

// ErrInvalidInput is returned when a caller-supplied argument is
// missing or malformed. Handlers map this to HTTP 400.
var ErrInvalidInput = errors.New("invalid input")

// ErrNotFound is returned by VerifyDomain when the domain row does
// not exist or RLS hides it from the caller.
var ErrNotFound = errors.New("not found")

// VerificationResult is the response from VerifyDomain. It carries
// the per-check booleans plus an aggregate flag that is `true` only
// when every individual check passed.
type VerificationResult struct {
	DomainID      string `json:"domain_id"`
	Domain        string `json:"domain"`
	MXVerified    bool   `json:"mx_verified"`
	SPFVerified   bool   `json:"spf_verified"`
	DKIMVerified  bool   `json:"dkim_verified"`
	DMARCVerified bool   `json:"dmarc_verified"`
	Verified      bool   `json:"verified"`
}

// DomainRecord is one DNS record row the DNS wizard instructs a
// tenant to publish.
type DomainRecord struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Value    string `json:"value"`
	TTL      int    `json:"ttl,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Notes    string `json:"notes,omitempty"`
}

// DomainRecords is the full set of records KMail asks the tenant to
// publish for a custom sending domain.
type DomainRecords struct {
	Domain  string         `json:"domain"`
	Records []DomainRecord `json:"records"`
}

// CheckMX verifies that the authoritative MX records for `domain`
// include a target under `s.MailHost`. Returns (false, nil) for a
// domain that resolves cleanly but does not point at KMail; returns
// (false, err) only on lookup failures.
func (s *Service) CheckMX(ctx context.Context, domain string) (bool, error) {
	if domain == "" {
		return false, fmt.Errorf("%w: domain is required", ErrInvalidInput)
	}
	records, err := s.resolver.LookupMX(ctx, domain)
	if err != nil {
		if isNoDataError(err) {
			return false, nil
		}
		return false, fmt.Errorf("lookup MX: %w", err)
	}
	if s.MailHost == "" {
		// Without a configured mail host we cannot assert the
		// record points at KMail; treat the check as unverifiable
		// rather than claiming success.
		return false, nil
	}
	target := normalizeHost(s.MailHost)
	for _, mx := range records {
		host := normalizeHost(mx.Host)
		if host == target || strings.HasSuffix(host, "."+target) {
			return true, nil
		}
	}
	return false, nil
}

// CheckSPF verifies that the SPF record for `domain` includes
// `s.SPFInclude`. Lookups that return no TXT records are treated as
// a negative result, not an error.
func (s *Service) CheckSPF(ctx context.Context, domain string) (bool, error) {
	if domain == "" {
		return false, fmt.Errorf("%w: domain is required", ErrInvalidInput)
	}
	if s.SPFInclude == "" {
		return false, nil
	}
	txts, err := s.resolver.LookupTXT(ctx, domain)
	if err != nil {
		if isNoDataError(err) {
			return false, nil
		}
		return false, fmt.Errorf("lookup SPF TXT: %w", err)
	}
	needle := "include:" + strings.ToLower(s.SPFInclude)
	for _, txt := range txts {
		lower := strings.ToLower(txt)
		if strings.HasPrefix(lower, "v=spf1") && strings.Contains(lower, needle) {
			return true, nil
		}
	}
	return false, nil
}

// CheckDKIM verifies that `{selector}._domainkey.{domain}` publishes
// a DKIM v=DKIM1 record with a non-empty `p=` public key tag. An
// empty `selector` falls back to `s.DefaultDKIMSelector`.
func (s *Service) CheckDKIM(ctx context.Context, domain, selector string) (bool, error) {
	if domain == "" {
		return false, fmt.Errorf("%w: domain is required", ErrInvalidInput)
	}
	if selector == "" {
		selector = s.DefaultDKIMSelector
	}
	if selector == "" {
		return false, nil
	}
	name := selector + "._domainkey." + domain
	txts, err := s.resolver.LookupTXT(ctx, name)
	if err != nil {
		if isNoDataError(err) {
			return false, nil
		}
		return false, fmt.Errorf("lookup DKIM TXT: %w", err)
	}
	for _, txt := range txts {
		if hasDKIMPublicKey(txt) {
			return true, nil
		}
	}
	return false, nil
}

// CheckDMARC verifies that `_dmarc.{domain}` publishes a DMARC v=1
// record. It does not enforce any specific policy strength — the
// DNS wizard reports back the observed policy via GenerateRecords.
func (s *Service) CheckDMARC(ctx context.Context, domain string) (bool, error) {
	if domain == "" {
		return false, fmt.Errorf("%w: domain is required", ErrInvalidInput)
	}
	name := "_dmarc." + domain
	txts, err := s.resolver.LookupTXT(ctx, name)
	if err != nil {
		if isNoDataError(err) {
			return false, nil
		}
		return false, fmt.Errorf("lookup DMARC TXT: %w", err)
	}
	for _, txt := range txts {
		lower := strings.ToLower(strings.TrimSpace(txt))
		if strings.HasPrefix(lower, "v=dmarc1") {
			return true, nil
		}
	}
	return false, nil
}

// VerifyDomain runs MX / SPF / DKIM / DMARC checks against the
// authoritative DNS for a tenant-owned domain, updates the
// `domains` verification flags, and returns the aggregated result.
//
// Structured as three phases so the pgx pool connection is never
// held across DNS lookups (which can block for 5–30 s each on
// resolver timeouts):
//
//  1. Short-lived RLS-scoped tx → SELECT the domain name.
//  2. DNS lookups (MX / SPF / DKIM / DMARC) with no DB connection held.
//  3. Short-lived RLS-scoped tx → UPDATE the verification flags.
//
// If the row is deleted between phase 1 and phase 3 the UPDATE
// affects 0 rows, which is surfaced as ErrNotFound.
func (s *Service) VerifyDomain(ctx context.Context, tenantID, domainID string) (*VerificationResult, error) {
	if tenantID == "" || domainID == "" {
		return nil, fmt.Errorf("%w: tenant id and domain id are required", ErrInvalidInput)
	}
	if s.pool == nil {
		return nil, errors.New("dns: service has no database pool")
	}

	// Phase 1 — SELECT the domain name under an RLS-scoped tx.
	var domain string
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT domain
			FROM domains
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`, domainID, tenantID).Scan(&domain)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("verify domain: %w", err)
	}

	// Phase 2 — DNS lookups outside any transaction. Each check can
	// block for the resolver's timeout; holding a pooled DB
	// connection across these would starve the pool under
	// concurrent verification load.
	result := VerificationResult{}
	if result.MXVerified, err = s.CheckMX(ctx, domain); err != nil {
		return nil, fmt.Errorf("verify domain: %w", err)
	}
	if result.SPFVerified, err = s.CheckSPF(ctx, domain); err != nil {
		return nil, fmt.Errorf("verify domain: %w", err)
	}
	if result.DKIMVerified, err = s.CheckDKIM(ctx, domain, ""); err != nil {
		return nil, fmt.Errorf("verify domain: %w", err)
	}
	if result.DMARCVerified, err = s.CheckDMARC(ctx, domain); err != nil {
		return nil, fmt.Errorf("verify domain: %w", err)
	}
	result.Verified = result.MXVerified && result.SPFVerified && result.DKIMVerified && result.DMARCVerified

	// Phase 3 — UPDATE the flags under a second RLS-scoped tx.
	// RowsAffected == 0 means the row was deleted between phase 1
	// and phase 3 or RLS now hides it; return ErrNotFound.
	var rows int64
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := middleware.SetTenantGUC(ctx, tx, tenantID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE domains
			SET mx_verified    = $3,
			    spf_verified   = $4,
			    dkim_verified  = $5,
			    dmarc_verified = $6,
			    verified       = $7
			WHERE id = $1::uuid AND tenant_id = $2::uuid
		`,
			domainID, tenantID,
			result.MXVerified, result.SPFVerified,
			result.DKIMVerified, result.DMARCVerified,
			result.Verified,
		)
		if err != nil {
			return err
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("verify domain: %w", err)
	}
	if rows == 0 {
		return nil, ErrNotFound
	}

	result.DomainID = domainID
	result.Domain = domain
	return &result, nil
}

// GenerateRecords returns the DNS records the tenant must publish to
// route mail through KMail: MX, SPF, DKIM, DMARC, MTA-STS, TLS-RPT,
// and autoconfig (`autodiscover` / `autoconfig` CNAMEs). The
// wizard surfaces this directly to the tenant admin UI.
func (s *Service) GenerateRecords(domain string) DomainRecords {
	out := DomainRecords{Domain: domain}
	if domain == "" {
		return out
	}

	if s.MailHost != "" {
		out.Records = append(out.Records, DomainRecord{
			Type:     "MX",
			Name:     domain,
			Value:    ensureTrailingDot(s.MailHost),
			Priority: 10,
			TTL:      3600,
			Notes:    "Route inbound mail to KMail.",
		})
	}

	if s.SPFInclude != "" {
		out.Records = append(out.Records, DomainRecord{
			Type:  "TXT",
			Name:  domain,
			Value: fmt.Sprintf("v=spf1 include:%s ~all", s.SPFInclude),
			TTL:   3600,
			Notes: "Authorize KMail to send on behalf of this domain.",
		})
	}

	dkimValue := "v=DKIM1; k=rsa; p=<PUBLIC_KEY>"
	if s.DKIMPublicKey != "" {
		dkimValue = fmt.Sprintf("v=DKIM1; k=rsa; p=%s", s.DKIMPublicKey)
	}
	out.Records = append(out.Records, DomainRecord{
		Type:  "TXT",
		Name:  fmt.Sprintf("%s._domainkey.%s", s.DefaultDKIMSelector, domain),
		Value: dkimValue,
		TTL:   3600,
		Notes: "DKIM signing key. KMail rotates this key periodically.",
	})

	dmarcValue := fmt.Sprintf("v=DMARC1; p=%s; adkim=s; aspf=s; fo=1", s.DMARCPolicy)
	if s.ReportingMailbox != "" {
		dmarcValue = fmt.Sprintf(
			"v=DMARC1; p=%s; rua=mailto:%s; ruf=mailto:%s; adkim=s; aspf=s; fo=1",
			s.DMARCPolicy, s.ReportingMailbox, s.ReportingMailbox,
		)
	}
	out.Records = append(out.Records, DomainRecord{
		Type:  "TXT",
		Name:  "_dmarc." + domain,
		Value: dmarcValue,
		TTL:   3600,
		Notes: "DMARC policy. KMail ingests the rua/ruf aggregate and forensic feeds.",
	})

	if s.MailHost != "" {
		out.Records = append(out.Records, DomainRecord{
			Type:  "TXT",
			Name:  "_mta-sts." + domain,
			Value: "v=STSv1; id=" + domain,
			TTL:   3600,
			Notes: "MTA-STS policy id. Publish the matching policy at https://mta-sts." + domain + "/.well-known/mta-sts.txt.",
		})
		out.Records = append(out.Records, DomainRecord{
			Type:  "CNAME",
			Name:  "mta-sts." + domain,
			Value: ensureTrailingDot("mta-sts." + s.MailHost),
			TTL:   3600,
			Notes: "Point the MTA-STS policy host at KMail.",
		})
	}

	if s.ReportingMailbox != "" {
		out.Records = append(out.Records, DomainRecord{
			Type:  "TXT",
			Name:  "_smtp._tls." + domain,
			Value: "v=TLSRPTv1; rua=mailto:" + s.ReportingMailbox,
			TTL:   3600,
			Notes: "TLS-RPT reporting endpoint.",
		})
	}

	if s.MailHost != "" {
		out.Records = append(out.Records, DomainRecord{
			Type:  "CNAME",
			Name:  "autoconfig." + domain,
			Value: ensureTrailingDot("autoconfig." + s.MailHost),
			TTL:   3600,
			Notes: "Thunderbird autoconfig.",
		})
		out.Records = append(out.Records, DomainRecord{
			Type:  "CNAME",
			Name:  "autodiscover." + domain,
			Value: ensureTrailingDot("autodiscover." + s.MailHost),
			TTL:   3600,
			Notes: "Apple Mail / Outlook autodiscover.",
		})
	}

	return out
}

// normalizeHost lowercases and strips the trailing dot that
// `net.LookupMX` returns on MX targets.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	return strings.TrimSuffix(h, ".")
}

// ensureTrailingDot appends a `.` to a hostname if missing so the
// generated records are unambiguous as fully qualified targets.
func ensureTrailingDot(h string) string {
	if strings.HasSuffix(h, ".") {
		return h
	}
	return h + "."
}

// hasDKIMPublicKey returns true when a TXT record looks like a
// DKIM v=DKIM1 record with a non-empty `p=` public key tag.
func hasDKIMPublicKey(txt string) bool {
	lower := strings.ToLower(strings.TrimSpace(txt))
	if !strings.Contains(lower, "v=dkim1") {
		return false
	}
	for _, part := range strings.Split(txt, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(kv[0]), "p") && strings.TrimSpace(kv[1]) != "" {
			return true
		}
	}
	return false
}

// isNoDataError reports whether a DNS lookup error is the benign
// "no such record" case we want to surface as "not verified" rather
// than an infrastructure error.
func isNoDataError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return dnsErr.IsNotFound || dnsErr.Err == "no such host"
	}
	return false
}
