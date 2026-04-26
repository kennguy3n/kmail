// Package contactbridge — Phase 5 CardDAV proxy + minimal vCard 4.0
// parser.
//
// Mirrors the calendarbridge package: HTTP service that forwards
// PROPFIND / REPORT / GET / PUT / DELETE to Stalwart's CardDAV
// endpoint, parses vCard payloads into a slim DTO the BFF
// surfaces to the React contacts UI.
//
// The vCard parser is intentionally minimal — only FN, N, EMAIL,
// TEL, ORG, NOTE — because the BFF round-trips the raw payload
// for any property it does not understand.
package contactbridge

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config wires NewService.
type Config struct {
	StalwartURL string
	HTTPClient  *http.Client
}

// Service speaks CardDAV to Stalwart.
type Service struct {
	cfg Config
}

// NewService returns a Service.
func NewService(cfg Config) *Service {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Service{cfg: cfg}
}

// AddressBook represents one CardDAV addressbook collection.
type AddressBook struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	IsDefault   bool   `json:"isDefault"`
}

// Contact is the slim DTO the BFF surfaces.
type Contact struct {
	UID      string   `json:"uid"`
	FN       string   `json:"fn"`
	Emails   []string `json:"emails,omitempty"`
	Phones   []string `json:"phones,omitempty"`
	Org      string   `json:"org,omitempty"`
	Note     string   `json:"note,omitempty"`
	PhotoURL string   `json:"photoUrl,omitempty"`
	Groups   []string `json:"groups,omitempty"`
	VCardRaw string   `json:"vcardRaw,omitempty"`
}

// ContactDraft is the input shape for create / update. The
// service builds a vCard 4.0 payload from these fields.
type ContactDraft struct {
	UID    string   `json:"uid,omitempty"`
	FN       string   `json:"fn"`
	Emails   []string `json:"emails,omitempty"`
	Phones   []string `json:"phones,omitempty"`
	Org      string   `json:"org,omitempty"`
	Note     string   `json:"note,omitempty"`
	PhotoURL string   `json:"photoUrl,omitempty"`
	Groups   []string `json:"groups,omitempty"`
}

// ErrInvalidInput / ErrNotFound mirror the calendarbridge package.
var ErrInvalidInput = errors.New("invalid input")
var ErrNotFound = errors.New("not found")

// ListAddressBooks returns the principal's addressbook home.
func (s *Service) ListAddressBooks(ctx context.Context, accountID string) ([]AddressBook, error) {
	if accountID == "" {
		return nil, fmt.Errorf("%w: accountID required", ErrInvalidInput)
	}
	home := s.addressBookHome(accountID)
	body := strings.NewReader(addressBookHomePropfindBody)
	resp, err := s.do(ctx, "PROPFIND", home, body, map[string]string{
		"Depth":        "1",
		"Content-Type": "application/xml; charset=utf-8",
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("carddav PROPFIND: HTTP %d", resp.StatusCode)
	}
	return parseAddressBookMultistatus(resp.Body, home)
}

// GetContacts enumerates the contacts in an addressbook.
func (s *Service) GetContacts(ctx context.Context, accountID, addressBookID string) ([]Contact, error) {
	if accountID == "" || addressBookID == "" {
		return nil, fmt.Errorf("%w: accountID and addressBookID required", ErrInvalidInput)
	}
	path := s.addressBookPath(accountID, addressBookID)
	resp, err := s.do(ctx, "REPORT", path, strings.NewReader(addressbookQueryBody), map[string]string{
		"Depth":        "1",
		"Content-Type": "application/xml; charset=utf-8",
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusMultiStatus && resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("carddav REPORT: HTTP %d", resp.StatusCode)
	}
	return parseContactMultistatus(resp.Body)
}

// GetContact fetches one contact by UID.
func (s *Service) GetContact(ctx context.Context, accountID, addressBookID, uid string) (*Contact, error) {
	if uid == "" {
		return nil, fmt.Errorf("%w: uid required", ErrInvalidInput)
	}
	path := s.contactPath(accountID, addressBookID, uid)
	resp, err := s.do(ctx, "GET", path, nil, map[string]string{
		"Accept": "text/vcard",
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("carddav GET: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	c := ParseVCard(string(raw))
	if c.UID == "" {
		c.UID = uid
	}
	return c, nil
}

// CreateContact PUTs a vCard built from the draft.
func (s *Service) CreateContact(ctx context.Context, accountID, addressBookID string, d ContactDraft) (string, error) {
	if d.FN == "" {
		return "", fmt.Errorf("%w: fn required", ErrInvalidInput)
	}
	uid := d.UID
	if uid == "" {
		uid = fmt.Sprintf("kmail-%d", time.Now().UnixNano())
	}
	d.UID = uid
	body := BuildVCard(d)
	path := s.contactPath(accountID, addressBookID, uid)
	resp, err := s.do(ctx, "PUT", path, strings.NewReader(body), map[string]string{
		"Content-Type": "text/vcard; charset=utf-8",
		"If-None-Match": "*",
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return "", fmt.Errorf("carddav PUT: HTTP %d", resp.StatusCode)
	}
	return uid, nil
}

// UpdateContact PUTs an existing vCard, overwriting the prior body.
func (s *Service) UpdateContact(ctx context.Context, accountID, addressBookID, uid string, d ContactDraft) error {
	if uid == "" {
		return fmt.Errorf("%w: uid required", ErrInvalidInput)
	}
	d.UID = uid
	body := BuildVCard(d)
	path := s.contactPath(accountID, addressBookID, uid)
	resp, err := s.do(ctx, "PUT", path, strings.NewReader(body), map[string]string{
		"Content-Type": "text/vcard; charset=utf-8",
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("carddav PUT: HTTP %d", resp.StatusCode)
	}
	return nil
}

// DeleteContact removes the resource.
func (s *Service) DeleteContact(ctx context.Context, accountID, addressBookID, uid string) error {
	if uid == "" {
		return fmt.Errorf("%w: uid required", ErrInvalidInput)
	}
	path := s.contactPath(accountID, addressBookID, uid)
	resp, err := s.do(ctx, "DELETE", path, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("carddav DELETE: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *Service) addressBookHome(accountID string) string {
	return strings.TrimRight(s.cfg.StalwartURL, "/") + "/dav/" + url.PathEscape(accountID) + "/addressbooks/"
}

func (s *Service) addressBookPath(accountID, abID string) string {
	return s.addressBookHome(accountID) + url.PathEscape(abID) + "/"
}

func (s *Service) contactPath(accountID, abID, uid string) string {
	return s.addressBookPath(accountID, abID) + url.PathEscape(uid) + ".vcf"
}

func (s *Service) do(ctx context.Context, method, urlStr string, body io.Reader, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return s.cfg.HTTPClient.Do(req)
}

// ---------------------------------------------------------------
// vCard parser
// ---------------------------------------------------------------

// ParseVCard extracts the slim Contact view from a vCard 4.0
// payload. Unknown properties are preserved in VCardRaw so the BFF
// can round-trip them on update.
func ParseVCard(raw string) *Contact {
	c := &Contact{VCardRaw: raw}
	for _, line := range splitLines(raw) {
		line = strings.TrimRight(line, "\r")
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		name := strings.ToUpper(strings.SplitN(line[:colon], ";", 2)[0])
		value := line[colon+1:]
		switch name {
		case "UID":
			c.UID = value
		case "FN":
			c.FN = value
		case "EMAIL":
			if value != "" {
				c.Emails = append(c.Emails, value)
			}
		case "TEL":
			if value != "" {
				c.Phones = append(c.Phones, value)
			}
		case "ORG":
			c.Org = value
		case "NOTE":
			c.Note = value
		case "PHOTO":
			c.PhotoURL = value
		case "CATEGORIES":
			for _, g := range strings.Split(value, ",") {
				if g = strings.TrimSpace(g); g != "" {
					c.Groups = append(c.Groups, g)
				}
			}
		}
	}
	return c
}

// BuildVCard renders a vCard 4.0 payload from the draft.
func BuildVCard(d ContactDraft) string {
	var b strings.Builder
	b.WriteString("BEGIN:VCARD\r\nVERSION:4.0\r\n")
	if d.UID != "" {
		fmt.Fprintf(&b, "UID:%s\r\n", d.UID)
	}
	if d.FN != "" {
		fmt.Fprintf(&b, "FN:%s\r\n", escapeVCardValue(d.FN))
	}
	for _, e := range d.Emails {
		fmt.Fprintf(&b, "EMAIL:%s\r\n", escapeVCardValue(e))
	}
	for _, p := range d.Phones {
		fmt.Fprintf(&b, "TEL:%s\r\n", escapeVCardValue(p))
	}
	if d.Org != "" {
		fmt.Fprintf(&b, "ORG:%s\r\n", escapeVCardValue(d.Org))
	}
	if d.Note != "" {
		fmt.Fprintf(&b, "NOTE:%s\r\n", escapeVCardValue(d.Note))
	}
	if d.PhotoURL != "" {
		fmt.Fprintf(&b, "PHOTO:%s\r\n", escapeVCardValue(d.PhotoURL))
	}
	if len(d.Groups) > 0 {
		fmt.Fprintf(&b, "CATEGORIES:%s\r\n", escapeVCardValue(strings.Join(d.Groups, ",")))
	}
	b.WriteString("END:VCARD\r\n")
	return b.String()
}

func escapeVCardValue(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, ",", "\\,")
	s = strings.ReplaceAll(s, ";", "\\;")
	return s
}

func splitLines(s string) []string {
	return strings.Split(s, "\n")
}

// ---------------------------------------------------------------
// CardDAV multistatus parsers
// ---------------------------------------------------------------

const addressBookHomePropfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:" xmlns:cs="urn:ietf:params:xml:ns:carddav">
  <d:prop>
    <d:displayname/>
    <d:resourcetype/>
    <cs:addressbook-description/>
  </d:prop>
</d:propfind>`

const addressbookQueryBody = `<?xml version="1.0" encoding="utf-8"?>
<C:addressbook-query xmlns:D="DAV:" xmlns:C="urn:ietf:params:xml:ns:carddav">
  <D:prop>
    <D:getetag/>
    <C:address-data/>
  </D:prop>
</C:addressbook-query>`

type davMultistatus struct {
	Responses []davResponse `xml:"response"`
}

type davResponse struct {
	Href     string       `xml:"href"`
	Propstat []davPropstat `xml:"propstat"`
}

type davPropstat struct {
	Prop   davProp `xml:"prop"`
	Status string  `xml:"status"`
}

type davProp struct {
	DisplayName  string `xml:"displayname"`
	ResourceType struct {
		AddressBook *struct{} `xml:"addressbook"`
	} `xml:"resourcetype"`
	AddressBookDescription string `xml:"addressbook-description"`
	AddressData            string `xml:"address-data"`
}

func parseAddressBookMultistatus(r io.Reader, home string) ([]AddressBook, error) {
	var ms davMultistatus
	if err := xml.NewDecoder(r).Decode(&ms); err != nil {
		return nil, fmt.Errorf("carddav decode: %w", err)
	}
	var out []AddressBook
	first := true
	for _, resp := range ms.Responses {
		if strings.TrimSuffix(resp.Href, "/") == strings.TrimSuffix(homePath(home), "/") {
			continue
		}
		var prop davProp
		for _, ps := range resp.Propstat {
			if strings.Contains(ps.Status, "200") {
				prop = ps.Prop
			}
		}
		if prop.ResourceType.AddressBook == nil {
			continue
		}
		ab := AddressBook{
			ID:          collectionIDFromHref(resp.Href),
			Name:        prop.DisplayName,
			Description: prop.AddressBookDescription,
			IsDefault:   first,
		}
		out = append(out, ab)
		first = false
	}
	return out, nil
}

func parseContactMultistatus(r io.Reader) ([]Contact, error) {
	var ms davMultistatus
	if err := xml.NewDecoder(r).Decode(&ms); err != nil {
		return nil, fmt.Errorf("carddav decode: %w", err)
	}
	var out []Contact
	for _, resp := range ms.Responses {
		var prop davProp
		for _, ps := range resp.Propstat {
			if strings.Contains(ps.Status, "200") {
				prop = ps.Prop
			}
		}
		if prop.AddressData == "" {
			continue
		}
		c := ParseVCard(prop.AddressData)
		out = append(out, *c)
	}
	return out, nil
}

func homePath(home string) string {
	u, err := url.Parse(home)
	if err != nil {
		return home
	}
	return u.Path
}

func collectionIDFromHref(href string) string {
	href = strings.TrimSuffix(href, "/")
	idx := strings.LastIndex(href, "/")
	if idx < 0 {
		return href
	}
	return href[idx+1:]
}
