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
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

// BearerMinter mints a short-lived OIDC bearer token authenticating
// the BFF to Stalwart as the given principal (the resolved CardDAV
// account email). Implemented by `*stalwartauth.Signer`; the bridge
// depends on this narrow interface rather than the concrete signer
// so the `contactbridge` package need not import `stalwartauth`.
// It is the same contract the JMAP proxy and `InternalClient` mint
// through (`docs/JMAP-CONTRACT.md` §3.2).
type BearerMinter interface {
	Mint(principal string) (string, error)
}

// Config wires NewService.
type Config struct {
	// StalwartURL is the base URL for Stalwart's CardDAV endpoint
	// (e.g. `http://stalwart:8080`). The bridge appends the
	// per-principal addressbook-home path on each request.
	StalwartURL string
	// AdminUser is the Basic-auth username the bridge presents to
	// Stalwart on behalf of the authenticated principal. Empty in
	// JMAP-first deployments that re-use the caller's bearer token;
	// populated (dev/CI) when the bridge is hoisted onto a dedicated
	// service account so it can resolve names and read mailboxes.
	AdminUser string
	// AdminPassword pairs with AdminUser. Never logged.
	AdminPassword string
	// Minter, when non-nil, mints a short-lived `stalwart`-audience
	// OIDC bearer for the resolved CardDAV principal on each request,
	// which the bridge forwards as `Authorization: Bearer …`. This
	// is the production BFF→Stalwart authentication path on the
	// official Stalwart image (which validates the JWT against the
	// OIDC directory whose `issuerUrl` points back at the BFF), the
	// same posture the JMAP proxy and `InternalClient` use. Mutually
	// exclusive with `AdminUser` (the dev/CI admin-Basic path takes
	// precedence when both are set, so dev never mints). When both
	// are empty the bridge sends no `Authorization` and defers to
	// the legacy mTLS client-certificate posture.
	Minter BearerMinter
	// HTTPClient overrides the HTTP client used for CardDAV requests.
	// Defaults to an http.Client with a 30s timeout.
	HTTPClient *http.Client
	// Logger, when set, records CardDAV account-email resolution
	// failures (see davAccount). A hard lookup failure means the
	// bridge falls back to the raw JMAP id, which Stalwart cannot
	// resolve to a DAV principal — so every CardDAV call for that
	// principal 404s. Logging makes a misconfigured admin credential or an
	// unreachable Stalwart visible instead of silently degrading.
	// nil disables this logging (e.g. the JMAP-first prod path, where
	// davAccount is a no-op because no admin credential is set).
	Logger *log.Logger
}

// Service speaks CardDAV to Stalwart.
type Service struct {
	cfg Config
	// emailCache memoises JMAP account id -> CardDAV account email
	// lookups (see davAccount). Size- and time-bounded: the LRU size
	// caps memory on a BFF instance fronting very many principals,
	// and the TTL bounds how long a stale mapping survives after an
	// admin changes a principal's email mid-process. The expirable
	// LRU is internally synchronised, so no extra mutex.
	emailCache *lru.LRU[string, string]
	// emailSF collapses concurrent lookups for the same account id
	// into a single in-flight x:Account/get call.
	emailSF singleflight.Group
}

const (
	// emailCacheMaxEntries caps the id->email cache as a memory guard
	// for a BFF instance fronting a very large number of distinct
	// principals. At ~64 B per entry, 50,000 entries stays well under
	// ~4 MiB.
	emailCacheMaxEntries = 50_000
	// emailCacheTTL bounds staleness after a principal email change
	// and mirrors the JMAP proxy / calendar-bridge identity caches so
	// all identity caches age out consistently. The id->email mapping
	// rarely changes, so re-resolving every 5 min is a negligible
	// extra x:Account/get per active principal.
	emailCacheTTL = 5 * time.Minute
)

// DevAdminConfig builds the contact-bridge Config. It layers in the
// dev/CI-only Stalwart superuser credentials when devEnv is true and
// KMAIL_STALWART_ADMIN_USER is set, and wires the production OIDC
// `minter` (which may be nil) for the BFF→Stalwart bearer path. The
// two are mutually exclusive at request time: when admin Basic is
// present (dev/CI) it takes precedence, so dev never mints; in
// production (devEnv false → no admin creds) the bridge mints a
// `stalwart`-audience bearer per CardDAV request, exactly as the
// JMAP proxy and `InternalClient` do. Mirrors
// calendarbridge.DevAdminConfig so the two bridges gate auth
// identically.
func DevAdminConfig(stalwartURL string, devEnv bool, logger *log.Logger, minter BearerMinter) Config {
	cfg := Config{StalwartURL: stalwartURL, Minter: minter}
	if devEnv {
		if adminUser := os.Getenv("KMAIL_STALWART_ADMIN_USER"); adminUser != "" {
			cfg.AdminUser = adminUser
			cfg.AdminPassword = os.Getenv("KMAIL_STALWART_ADMIN_PASS")
			cfg.Logger = logger
		}
	}
	return cfg
}

// NewService returns a Service.
func NewService(cfg Config) *Service {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Service{
		cfg:        cfg,
		emailCache: lru.NewLRU[string, string](emailCacheMaxEntries, nil, emailCacheTTL),
	}
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
	UID      string   `json:"uid,omitempty"`
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
	home := s.addressBookHome(ctx, accountID)
	body := strings.NewReader(addressBookHomePropfindBody)
	resp, err := s.do(ctx, "PROPFIND", home, body, map[string]string{
		"Depth":        "1",
		"Content-Type": "application/xml; charset=utf-8",
	}, accountID)
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
	path := s.addressBookPath(ctx, accountID, addressBookID)
	resp, err := s.do(ctx, "REPORT", path, strings.NewReader(addressbookQueryBody), map[string]string{
		"Depth":        "1",
		"Content-Type": "application/xml; charset=utf-8",
	}, accountID)
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
	path := s.contactPath(ctx, accountID, addressBookID, uid)
	resp, err := s.do(ctx, "GET", path, nil, map[string]string{
		"Accept": "text/vcard",
	}, accountID)
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
	path := s.contactPath(ctx, accountID, addressBookID, uid)
	resp, err := s.do(ctx, "PUT", path, strings.NewReader(body), map[string]string{
		"Content-Type":  "text/vcard; charset=utf-8",
		"If-None-Match": "*",
	}, accountID)
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
	path := s.contactPath(ctx, accountID, addressBookID, uid)
	resp, err := s.do(ctx, "PUT", path, strings.NewReader(body), map[string]string{
		"Content-Type": "text/vcard; charset=utf-8",
	}, accountID)
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
	path := s.contactPath(ctx, accountID, addressBookID, uid)
	resp, err := s.do(ctx, "DELETE", path, nil, nil, accountID)
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

func (s *Service) addressBookHome(ctx context.Context, accountID string) string {
	return strings.TrimRight(s.cfg.StalwartURL, "/") + "/dav/card/" + url.PathEscape(s.davAccount(ctx, accountID)) + "/"
}

func (s *Service) addressBookPath(ctx context.Context, accountID, abID string) string {
	return s.addressBookHome(ctx, accountID) + url.PathEscape(abID) + "/"
}

func (s *Service) contactPath(ctx context.Context, accountID, abID, uid string) string {
	return s.addressBookPath(ctx, accountID, abID) + url.PathEscape(uid) + ".vcf"
}

func (s *Service) do(ctx context.Context, method, urlStr string, body io.Reader, headers map[string]string, accountID string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlStr, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if err := s.setAuth(ctx, req, accountID); err != nil {
		return nil, err
	}
	return s.cfg.HTTPClient.Do(req)
}

// setAuth stamps the BFF→Stalwart `Authorization` header on a
// CardDAV request, mirroring the JMAP proxy / InternalClient
// three-armed switch (`docs/JMAP-CONTRACT.md` §3.2):
//
//  1. Dev/CI (`AdminUser` set): authenticate with the recovery-admin
//     Basic credential (gated by `middleware.IsDevEnv` in the
//     entrypoints). Takes precedence so dev never mints.
//  2. Production (`Minter` set): mint a short-lived,
//     `stalwart`-audience OIDC bearer for the resolved CardDAV
//     principal and forward it as `Authorization: Bearer …`. The
//     principal is the same `/dav/card/{principal}/` value the path
//     is keyed by (`davAccount`); without admin creds that resolves
//     to the account id unchanged, which in production is already
//     the principal email. A mint failure FAILS CLOSED — return an
//     error rather than issue an unauthenticated CardDAV call.
//  3. Legacy mTLS-only (neither set): leave `Authorization` unset
//     and defer to the mTLS client certificate / trusted-network
//     posture.
func (s *Service) setAuth(ctx context.Context, req *http.Request, accountID string) error {
	switch {
	case s.cfg.AdminUser != "":
		req.SetBasicAuth(s.cfg.AdminUser, s.cfg.AdminPassword)
	case s.cfg.Minter != nil:
		principal := s.davAccount(ctx, accountID)
		if strings.TrimSpace(principal) == "" {
			// Never mint an unscoped token — fail closed.
			return fmt.Errorf("contactbridge: cannot mint stalwart bearer with empty account id")
		}
		token, err := s.cfg.Minter.Mint(principal)
		if err != nil {
			return fmt.Errorf("contactbridge: mint stalwart bearer for account=%s: %w", principal, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return nil
}

// davAccount maps a JMAP account id to the account *email* Stalwart
// uses to key CardDAV collections (`/dav/card/{email}/`). Stalwart
// resolves a DAV path's principal segment via the account's email
// address (crates/dav/src/common/uri.rs -> account_id_from_email),
// so a bare login name 404s; the BFF resolves callers to their JMAP
// account id, which is a third, distinct identifier. When the bridge
// holds admin credentials it asks Stalwart for the email via the
// `x:Account/get` management method and memoises the result; without
// credentials — or on any lookup failure — it returns the id
// unchanged (in production the id is already the email).
func (s *Service) davAccount(ctx context.Context, accountID string) string {
	if accountID == "" || s.cfg.AdminUser == "" {
		return accountID
	}
	if cached, ok := s.emailCache.Get(accountID); ok {
		return cached
	}
	// Dedupe concurrent lookups and only memoise a *resolved* email.
	// On any failure lookupAccountEmail returns ""; we then fall back to
	// the raw id for this call without caching it, so the next request
	// retries once Stalwart recovers. Failures never touch the cache,
	// so a failing goroutine cannot overwrite another's success. A hard
	// lookup error (transport / non-2xx / malformed or JMAP-level
	// error response, as opposed to a clean not-found) is logged once
	// per attempt so a persistent misconfiguration is observable
	// rather than silently 404-ing every CardDAV call.
	v, _, _ := s.emailSF.Do(accountID, func() (interface{}, error) {
		email, lookupErr := s.lookupAccountEmail(ctx, accountID)
		if email != "" {
			s.emailCache.Add(accountID, email)
		} else if lookupErr != nil && s.cfg.Logger != nil {
			s.cfg.Logger.Printf("contactbridge: resolve CardDAV email for account %q failed: %v", accountID, lookupErr)
		}
		return email, nil
	})
	if email, _ := v.(string); email != "" {
		return email
	}
	return accountID
}

// lookupAccountEmail issues a single `x:Account/get` JMAP call as the
// admin principal and returns the resolved account email address —
// the identifier Stalwart keys DAV collections by. It returns a
// non-nil error for *hard* failures (transport, non-2xx, malformed or
// JMAP-level error response) so the caller can surface a
// misconfiguration; a successful call that simply doesn't contain the
// requested id (e.g. an unprovisioned principal) returns ("", nil)
// since that is an expected, non-alarming outcome. Stalwart ignores
// the `accountId` envelope field for this management method, so an
// empty value is fine.
func (s *Service) lookupAccountEmail(ctx context.Context, accountID string) (string, error) {
	ids, err := json.Marshal([]string{accountID})
	if err != nil {
		return "", err
	}
	body := `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["x:Account/get",{"accountId":"","ids":` + string(ids) + `},"0"]]}`
	endpoint := strings.TrimRight(s.cfg.StalwartURL, "/") + "/jmap"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth(s.cfg.AdminUser, s.cfg.AdminPassword)
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("x:Account/get: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var envelope struct {
		MethodResponses [][]json.RawMessage `json:"methodResponses"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", err
	}
	if len(envelope.MethodResponses) == 0 || len(envelope.MethodResponses[0]) < 2 {
		return "", fmt.Errorf("x:Account/get: malformed response envelope")
	}
	// A JMAP method-level error arrives as an HTTP 200 whose first
	// tuple element is the literal "error" (e.g.
	// `["error",{"type":"unknownMethod"},"0"]`) rather than the echoed
	// method name. Without this check the error object would unmarshal
	// cleanly into result with an empty List and look like a benign
	// not-found, hiding a real misconfiguration from the caller's
	// logging.
	var methodName string
	if err := json.Unmarshal(envelope.MethodResponses[0][0], &methodName); err != nil {
		return "", fmt.Errorf("x:Account/get: malformed method name: %w", err)
	}
	if methodName == "error" {
		var jmapErr struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(envelope.MethodResponses[0][1], &jmapErr)
		return "", fmt.Errorf("x:Account/get: JMAP error %q", jmapErr.Type)
	}
	var result struct {
		List []struct {
			ID    string `json:"id"`
			Email string `json:"emailAddress"`
		} `json:"list"`
	}
	if err := json.Unmarshal(envelope.MethodResponses[0][1], &result); err != nil {
		return "", err
	}
	// Match strictly on the requested id. The request passes
	// `ids:[accountID]`, so Stalwart returns at most that one account;
	// we never fall back to an arbitrary list entry, which could
	// otherwise cache a *different* principal's email and route every
	// subsequent CardDAV call for this account to the wrong
	// addressbook home.
	for _, a := range result.List {
		if a.ID == accountID && a.Email != "" {
			return a.Email, nil
		}
	}
	return "", nil
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
			c.FN = unescapeVCardValue(value)
		case "EMAIL":
			if v := unescapeVCardValue(value); v != "" {
				c.Emails = append(c.Emails, v)
			}
		case "TEL":
			if v := unescapeVCardValue(value); v != "" {
				c.Phones = append(c.Phones, v)
			}
		case "ORG":
			c.Org = unescapeVCardValue(value)
		case "NOTE":
			c.Note = unescapeVCardValue(value)
		case "PHOTO":
			c.PhotoURL = unescapeVCardValue(value)
		case "CATEGORIES":
			for _, g := range splitVCardList(value) {
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
		escaped := make([]string, len(d.Groups))
		for i, g := range d.Groups {
			escaped[i] = escapeVCardValue(g)
		}
		fmt.Fprintf(&b, "CATEGORIES:%s\r\n", strings.Join(escaped, ","))
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

// unescapeVCardValue is the inverse of escapeVCardValue per RFC
// 6350 §3.4: `\\`, `\n` / `\N`, `\,`, `\;` decode to their literal
// counterparts.
func unescapeVCardValue(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '\\':
				b.WriteByte('\\')
			case 'n', 'N':
				b.WriteByte('\n')
			case ',':
				b.WriteByte(',')
			case ';':
				b.WriteByte(';')
			default:
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// splitVCardList splits a comma-separated vCard list value (e.g.
// CATEGORIES) into its constituent items, respecting backslash-
// escaped commas. Each returned item is unescaped.
func splitVCardList(s string) []string {
	var out []string
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			b.WriteByte(s[i])
			b.WriteByte(s[i+1])
			i++
			continue
		}
		if s[i] == ',' {
			out = append(out, unescapeVCardValue(b.String()))
			b.Reset()
			continue
		}
		b.WriteByte(s[i])
	}
	out = append(out, unescapeVCardValue(b.String()))
	return out
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
	Href     string        `xml:"href"`
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
