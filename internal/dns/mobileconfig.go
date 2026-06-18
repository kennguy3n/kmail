// Package dns — Apple configuration profile (`.mobileconfig`).
//
// Apple devices (iOS / iPadOS / macOS) don't read the Mozilla or
// Outlook autoconfig formats, and — crucially — neither of those
// formats has a slot for CalDAV / CardDAV. Apple's configuration
// profile is the one discovery format that carries mail, calendar,
// and contacts together, so a user taps a single link and lands
// with Mail + Calendar + Contacts all wired to KMail. Every major
// hosted-email provider ships one of these; this is KMail's.
//
// The profile is an XML property list (plist). We build it through
// encoding/xml so every dynamic value (email, domain, hostnames)
// is XML-escaped by the encoder — these are public, unauthenticated
// endpoints fed by the request's `emailaddress`, so injection-safe
// rendering is a hard requirement.
package dns

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// AppleConfigContentType is the MIME type Apple devices expect for
// a configuration profile; serving anything else makes Safari /
// Mail download it as plain text instead of offering to install.
const AppleConfigContentType = "application/x-apple-aspen-config"

// mobileconfigNamespace seeds deterministic per-payload UUIDs.
// Deriving the UUIDs from (domain, email, payload) — rather than
// random — means re-downloading the profile yields the *same*
// PayloadUUIDs, so the device updates the existing profile in
// place instead of accumulating duplicates.
var mobileconfigNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://kmail/mobileconfig"))

func payloadUUID(parts ...string) string {
	return strings.ToUpper(uuid.NewSHA1(mobileconfigNamespace, []byte(strings.Join(parts, "\x00"))).String())
}

// payloadIdentifier builds a stable reverse-DNS-ish identifier.
// Apple keys profile de-duplication on the top-level identifier,
// so it must be stable per (brand, domain) and distinct per
// payload via the suffix.
func payloadIdentifier(brand, domain, suffix string) string {
	base := "email." + sanitizeIdent(brand) + "." + sanitizeIdent(domain)
	if suffix == "" {
		return base
	}
	return base + "." + suffix
}

// sanitizeIdent reduces an arbitrary label to the dot-separated
// alphanumeric form an Apple PayloadIdentifier expects.
func sanitizeIdent(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "kmail"
	}
	return out
}

// AppleMobileConfig renders an unsigned `.mobileconfig` profile for
// the given email and tenant settings. It always emits a Mail
// (IMAP + SMTP) payload, and additionally a CalDAV / CardDAV
// payload whenever the corresponding URL in `s` parses to a host.
// Unsigned is the norm for self-service profiles — the device shows
// an "Unsigned" banner but installs fine; signing would require a
// per-deployment cert and is a separate concern.
func AppleMobileConfig(email string, s AutoconfigSettings, brand string) ([]byte, error) {
	if brand == "" {
		brand = "KMail"
	}
	display := s.DisplayName
	if display == "" {
		display = brand + " — " + s.Domain
	}

	mail := plistDict{}
	mail.set("PayloadType", "com.apple.mail.managed")
	mail.set("PayloadVersion", 1)
	mail.set("PayloadIdentifier", payloadIdentifier(brand, s.Domain, "mail"))
	mail.set("PayloadUUID", payloadUUID("mail", s.Domain, email))
	mail.set("PayloadDisplayName", brand+" — Mail")
	mail.set("EmailAccountDescription", display)
	mail.set("EmailAccountName", email)
	mail.set("EmailAccountType", "EmailTypeIMAP")
	mail.set("EmailAddress", email)
	mail.set("IncomingMailServerAuthentication", "EmailAuthPassword")
	mail.set("IncomingMailServerHostName", s.IMAPHost)
	mail.set("IncomingMailServerPortNumber", s.IMAPPort)
	mail.set("IncomingMailServerUseSSL", true)
	mail.set("IncomingMailServerUsername", email)
	mail.set("OutgoingMailServerAuthentication", "EmailAuthPassword")
	mail.set("OutgoingMailServerHostName", s.SMTPHost)
	mail.set("OutgoingMailServerPortNumber", s.SMTPPort)
	mail.set("OutgoingMailServerUseSSL", true)
	mail.set("OutgoingMailServerUsername", email)
	mail.set("OutgoingPasswordSameAsIncomingPassword", true)
	mail.set("PreventMove", false)
	mail.set("SMIMEEnabled", false)

	payloads := []any{mail}

	if host, port, useSSL, ok := davEndpoint(s.CalDAVURL); ok {
		cal := plistDict{}
		cal.set("PayloadType", "com.apple.caldav.account")
		cal.set("PayloadVersion", 1)
		cal.set("PayloadIdentifier", payloadIdentifier(brand, s.Domain, "caldav"))
		cal.set("PayloadUUID", payloadUUID("caldav", s.Domain, email))
		cal.set("PayloadDisplayName", brand+" — Calendar")
		cal.set("CalDAVAccountDescription", display)
		cal.set("CalDAVHostName", host)
		cal.set("CalDAVPort", port)
		cal.set("CalDAVUseSSL", useSSL)
		cal.set("CalDAVUsername", email)
		payloads = append(payloads, cal)
	}

	if host, port, useSSL, ok := davEndpoint(s.CardDAVURL); ok {
		card := plistDict{}
		card.set("PayloadType", "com.apple.carddav.account")
		card.set("PayloadVersion", 1)
		card.set("PayloadIdentifier", payloadIdentifier(brand, s.Domain, "carddav"))
		card.set("PayloadUUID", payloadUUID("carddav", s.Domain, email))
		card.set("PayloadDisplayName", brand+" — Contacts")
		card.set("CardDAVAccountDescription", display)
		card.set("CardDAVHostName", host)
		card.set("CardDAVPort", port)
		card.set("CardDAVUseSSL", useSSL)
		card.set("CardDAVUsername", email)
		payloads = append(payloads, card)
	}

	root := plistDict{}
	root.set("PayloadContent", payloads)
	root.set("PayloadDisplayName", display)
	root.set("PayloadIdentifier", payloadIdentifier(brand, s.Domain, ""))
	root.set("PayloadOrganization", brand)
	root.set("PayloadRemovalDisallowed", false)
	root.set("PayloadType", "Configuration")
	root.set("PayloadUUID", payloadUUID("config", s.Domain, email))
	root.set("PayloadVersion", 1)

	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	buf.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "\t")
	plistStart := xml.StartElement{
		Name: xml.Name{Local: "plist"},
		Attr: []xml.Attr{{Name: xml.Name{Local: "version"}, Value: "1.0"}},
	}
	if err := enc.EncodeToken(plistStart); err != nil {
		return nil, err
	}
	if err := encodePlistValue(enc, root); err != nil {
		return nil, err
	}
	if err := enc.EncodeToken(plistStart.End()); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// davEndpoint parses a CalDAV / CardDAV service URL into the
// host / port / TLS triple an Apple DAV payload needs. The
// PrincipalURL is deliberately omitted from the profile: the
// device discovers it via RFC 6764 well-known lookup against the
// host, which is more robust than hard-coding a collection root.
// Returns ok=false for an empty or unparseable URL so the caller
// simply skips that payload.
func davEndpoint(raw string) (host string, port int, useSSL bool, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", 0, false, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", 0, false, false
	}
	useSSL = u.Scheme != "http"
	port = 443
	if !useSSL {
		port = 80
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return "", 0, false, false
		}
		port = n
	}
	return u.Hostname(), port, useSSL, true
}

// plistDict is an ordered key/value map. Order matters for plist
// readability (and keeps test golden output stable), so we keep an
// explicit slice rather than a Go map.
type plistDict struct {
	keys []string
	vals []any
}

func (d *plistDict) set(key string, val any) {
	d.keys = append(d.keys, key)
	d.vals = append(d.vals, val)
}

// MarshalXML emits `<dict><key>..</key><value/>...</dict>`. The
// xml.Encoder escapes all string content, so user-controlled
// values cannot break out of the document.
func (d plistDict) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "dict"}
	start.Attr = nil
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for i, key := range d.keys {
		if err := e.EncodeElement(key, xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
			return err
		}
		if err := encodePlistValue(e, d.vals[i]); err != nil {
			return err
		}
	}
	return e.EncodeToken(start.End())
}

// encodePlistValue writes a single plist value element for the
// supported Go types (string / int / bool / nested dict / array).
func encodePlistValue(e *xml.Encoder, v any) error {
	switch t := v.(type) {
	case string:
		return e.EncodeElement(t, xml.StartElement{Name: xml.Name{Local: "string"}})
	case int:
		return e.EncodeElement(strconv.Itoa(t), xml.StartElement{Name: xml.Name{Local: "integer"}})
	case bool:
		name := "false"
		if t {
			name = "true"
		}
		el := xml.StartElement{Name: xml.Name{Local: name}}
		if err := e.EncodeToken(el); err != nil {
			return err
		}
		return e.EncodeToken(el.End())
	case plistDict:
		return e.EncodeElement(t, xml.StartElement{Name: xml.Name{Local: "dict"}})
	case []any:
		arr := xml.StartElement{Name: xml.Name{Local: "array"}}
		if err := e.EncodeToken(arr); err != nil {
			return err
		}
		for _, item := range t {
			if err := encodePlistValue(e, item); err != nil {
				return err
			}
		}
		return e.EncodeToken(arr.End())
	default:
		return fmt.Errorf("mobileconfig: unsupported plist value type %T", v)
	}
}
