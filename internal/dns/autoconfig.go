// Package dns — autoconfig / autodiscover XML generation.
//
// Phase 8 closes the loop with the DNS wizard: the wizard already
// instructs operators to publish the autoconfig DNS records
// (`autoconfig.<domain>` CNAME, `_autodiscover._tcp.<domain>` SRV,
// `.well-known/autoconfig`) but the actual HTTP endpoints those
// records point at were missing. This file owns the response
// shape:
//
//   - Mozilla Thunderbird autoconfig (`mail/config-v1.1.xml`,
//     plus the legacy `.well-known/autoconfig/mail/config-v1.1.xml`
//     path).
//   - Microsoft Outlook autodiscover (`/autodiscover/autodiscover.xml`).
//
// Both responses are tenant-aware: the email address in the
// inbound request is parsed, the bare domain looked up against
// the `domains` table (without RLS — these are unauthenticated
// public discovery endpoints), and the reply describes the
// tenant's IMAP / SMTP / CalDAV / CardDAV endpoints.
package dns

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AutoconfigSettings carries the per-tenant server settings the
// XML responses describe. Built from `AutoconfigConfig` plus a
// row in `domains`.
type AutoconfigSettings struct {
	Domain         string
	DisplayName    string
	IMAPHost       string
	IMAPPort       int
	SMTPHost       string
	SMTPPort       int
	CalDAVURL      string
	CardDAVURL     string
	SocketType     string // "SSL" / "STARTTLS"
	Authentication string // "password-cleartext" (over TLS)
}

// AutoconfigConfig wires the autoconfig service. The IMAP / SMTP /
// CalDAV defaults flow through from `KMAIL_AUTOCONFIG_*` env vars
// in the BFF; the per-domain row in `domains` provides the bare
// domain that becomes the displayed user-facing name.
type AutoconfigConfig struct {
	Pool       *pgxpool.Pool
	IMAPHost   string
	IMAPPort   int
	SMTPHost   string
	SMTPPort   int
	CalDAVHost string
	CalDAVPort int
	BaseURL    string // e.g. "https://api.kmail.example"
	BrandName  string // shown in Thunderbird's UI; defaults to "KMail"
}

// AutoconfigService resolves an inbound email to its tenant's
// server settings. A nil pool short-circuits to the configured
// defaults (handy for dev / single-tenant deployments).
type AutoconfigService struct {
	cfg AutoconfigConfig
}

// NewAutoconfigService applies sensible defaults to cfg and
// returns the service.
func NewAutoconfigService(cfg AutoconfigConfig) *AutoconfigService {
	if cfg.IMAPHost == "" {
		cfg.IMAPHost = "imap.kmail.local"
	}
	if cfg.IMAPPort == 0 {
		cfg.IMAPPort = 993
	}
	if cfg.SMTPHost == "" {
		cfg.SMTPHost = "smtp.kmail.local"
	}
	if cfg.SMTPPort == 0 {
		cfg.SMTPPort = 587
	}
	if cfg.CalDAVHost == "" {
		cfg.CalDAVHost = cfg.IMAPHost
	}
	if cfg.CalDAVPort == 0 {
		cfg.CalDAVPort = 443
	}
	if cfg.BrandName == "" {
		cfg.BrandName = "KMail"
	}
	return &AutoconfigService{cfg: cfg}
}

// ErrUnknownDomain is returned when the requested email's domain
// is not registered to any tenant. The handler maps this to 404.
var ErrUnknownDomain = errors.New("autoconfig: domain not registered")

// SettingsForEmail looks up the per-tenant settings for the email
// address. The email is parsed, the bare domain is matched
// against `domains.domain` (case-insensitive). Unknown domains
// return ErrUnknownDomain so the handler can answer 404 — RFC
// 6186 / Mozilla autoconfig clients handle that gracefully.
func (s *AutoconfigService) SettingsForEmail(ctx context.Context, email string) (*AutoconfigSettings, error) {
	domain, err := domainFromEmail(email)
	if err != nil {
		return nil, err
	}
	settings := AutoconfigSettings{
		Domain:         domain,
		DisplayName:    s.cfg.BrandName + " — " + domain,
		IMAPHost:       s.cfg.IMAPHost,
		IMAPPort:       s.cfg.IMAPPort,
		SMTPHost:       s.cfg.SMTPHost,
		SMTPPort:       s.cfg.SMTPPort,
		CalDAVURL:      fmt.Sprintf("https://%s/dav/calendars/", s.cfg.CalDAVHost),
		CardDAVURL:     fmt.Sprintf("https://%s/dav/contacts/", s.cfg.CalDAVHost),
		SocketType:     "SSL",
		Authentication: "password-cleartext",
	}
	if s.cfg.SMTPPort == 587 {
		settings.SocketType = "STARTTLS"
	}
	if s.cfg.Pool == nil {
		return &settings, nil
	}
	// Public endpoint: skip RLS GUC and just confirm the domain
	// is registered to *some* tenant. The XML payload is the same
	// across all tenants on the same KMail cluster — what differs
	// is whether we answer at all.
	var verified bool
	row := s.cfg.Pool.QueryRow(ctx, `
		SELECT verified
		FROM domains
		WHERE lower(domain) = lower($1)
		LIMIT 1
	`, domain)
	if err := row.Scan(&verified); err != nil {
		return nil, ErrUnknownDomain
	}
	if !verified {
		// The domain is registered but DNS is not yet verified.
		// Mozilla / Outlook clients will retry once DNS lands; we
		// answer the same XML so a partially-verified domain is
		// never the reason an autoconfig client fails.
		settings.DisplayName += " (provisioning)"
	}
	return &settings, nil
}

// domainFromEmail extracts the bare domain from `local@domain`.
func domainFromEmail(email string) (string, error) {
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return "", fmt.Errorf("autoconfig: invalid email %q", email)
	}
	return strings.TrimSpace(strings.ToLower(email[at+1:])), nil
}

// MozillaConfig is the XML shape Thunderbird expects at
// `mail/config-v1.1.xml` (Mozilla autoconfig spec). Only the
// fields KMail uses are surfaced.
type MozillaConfig struct {
	XMLName        xml.Name           `xml:"clientConfig"`
	Version        string             `xml:"version,attr"`
	EmailProvider  *MozillaProvider   `xml:"emailProvider"`
	ClientConfigID string             `xml:"clientConfigUpdate>url,omitempty"`
}

// MozillaProvider is the `<emailProvider>` block.
type MozillaProvider struct {
	ID              string                  `xml:"id,attr"`
	Domains         []string                `xml:"domain"`
	DisplayName     string                  `xml:"displayName"`
	DisplayShortName string                 `xml:"displayShortName"`
	IncomingServers []MozillaIncomingServer `xml:"incomingServer"`
	OutgoingServers []MozillaOutgoingServer `xml:"outgoingServer"`
}

// MozillaIncomingServer describes IMAP / POP3 settings.
type MozillaIncomingServer struct {
	Type           string `xml:"type,attr"`
	Hostname       string `xml:"hostname"`
	Port           int    `xml:"port"`
	SocketType     string `xml:"socketType"`
	Authentication string `xml:"authentication"`
	Username       string `xml:"username"`
}

// MozillaOutgoingServer describes SMTP settings.
type MozillaOutgoingServer struct {
	Type           string `xml:"type,attr"`
	Hostname       string `xml:"hostname"`
	Port           int    `xml:"port"`
	SocketType     string `xml:"socketType"`
	Authentication string `xml:"authentication"`
	Username       string `xml:"username"`
}

// MozillaXML renders the Thunderbird autoconfig XML for the
// given email address + settings.
func MozillaXML(email string, s AutoconfigSettings) ([]byte, error) {
	cfg := MozillaConfig{
		Version: "1.1",
		EmailProvider: &MozillaProvider{
			ID:               s.Domain,
			Domains:          []string{s.Domain},
			DisplayName:      s.DisplayName,
			DisplayShortName: s.Domain,
			IncomingServers: []MozillaIncomingServer{{
				Type:           "imap",
				Hostname:       s.IMAPHost,
				Port:           s.IMAPPort,
				SocketType:     "SSL",
				Authentication: s.Authentication,
				Username:       email,
			}},
			OutgoingServers: []MozillaOutgoingServer{{
				Type:           "smtp",
				Hostname:       s.SMTPHost,
				Port:           s.SMTPPort,
				SocketType:     s.SocketType,
				Authentication: s.Authentication,
				Username:       email,
			}},
		},
	}
	body, err := xml.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}

// OutlookAutodiscoverRequest is the (subset of the) XML shape
// Outlook posts to autodiscover.xml.
type OutlookAutodiscoverRequest struct {
	XMLName xml.Name `xml:"Autodiscover"`
	Request struct {
		EMailAddress string `xml:"EMailAddress"`
	} `xml:"Request"`
}

// OutlookAutodiscoverResponse describes the response shape
// Outlook expects. KMail only fills the IMAP / SMTP protocols;
// MAPI / Exchange / ActiveSync are explicitly out of scope per
// the do-not-do list.
type OutlookAutodiscoverResponse struct {
	XMLName  xml.Name        `xml:"Autodiscover"`
	XMLNS    string          `xml:"xmlns,attr"`
	Response OutlookResponse `xml:"Response"`
}

// OutlookResponse is the `<Response>` block.
type OutlookResponse struct {
	XMLNS   string         `xml:"xmlns,attr"`
	User    OutlookUser    `xml:"User"`
	Account OutlookAccount `xml:"Account"`
}

// OutlookUser is the `<User>` block — display name only.
type OutlookUser struct {
	DisplayName string `xml:"DisplayName"`
}

// OutlookAccount is the `<Account>` block.
type OutlookAccount struct {
	AccountType string            `xml:"AccountType"`
	Action      string            `xml:"Action"`
	Protocols   []OutlookProtocol `xml:"Protocol"`
}

// OutlookProtocol is one server protocol entry.
type OutlookProtocol struct {
	Type           string `xml:"Type"`
	Server         string `xml:"Server"`
	Port           int    `xml:"Port"`
	DomainRequired string `xml:"DomainRequired"`
	LoginName      string `xml:"LoginName"`
	SPA            string `xml:"SPA"`
	SSL            string `xml:"SSL"`
	AuthRequired   string `xml:"AuthRequired"`
}

// OutlookXML renders the autodiscover.xml response for the given
// email + settings.
func OutlookXML(email string, s AutoconfigSettings) ([]byte, error) {
	resp := OutlookAutodiscoverResponse{
		XMLNS: "http://schemas.microsoft.com/exchange/autodiscover/responseschema/2006",
		Response: OutlookResponse{
			XMLNS: "http://schemas.microsoft.com/exchange/autodiscover/outlook/responseschema/2006a",
			User: OutlookUser{
				DisplayName: s.DisplayName,
			},
			Account: OutlookAccount{
				AccountType: "email",
				Action:      "settings",
				Protocols: []OutlookProtocol{
					{
						Type: "IMAP", Server: s.IMAPHost, Port: s.IMAPPort,
						DomainRequired: "off", LoginName: email,
						SPA: "off", SSL: "on", AuthRequired: "on",
					},
					{
						Type: "SMTP", Server: s.SMTPHost, Port: s.SMTPPort,
						DomainRequired: "off", LoginName: email,
						SPA: "off", SSL: "on", AuthRequired: "on",
					},
				},
			},
		},
	}
	body, err := xml.MarshalIndent(resp, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), body...), nil
}

// CacheControlMaxAge is what the autoconfig handlers use as
// the response Cache-Control max-age. Mozilla / Outlook clients
// respect this; 1 hour is the conservative spec recommendation.
const CacheControlMaxAge = 1 * time.Hour
