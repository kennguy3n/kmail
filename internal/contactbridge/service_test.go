package contactbridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// davServer is a CardDAV stub that resolves the JMAP id -> email (so
// the bridge builds the email-keyed /dav/card/{email}/ path) and
// records the path of the last request per method, letting tests
// assert the principal segment is the resolved email rather than the
// raw JMAP id.
type davServer struct {
	*httptest.Server
	lastPath map[string]string
}

func newDavServer(t *testing.T, abMultistatus, contactMultistatus, vcardBody string) *davServer {
	t.Helper()
	d := &davServer{lastPath: map[string]string{}}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.lastPath[r.Method] = r.URL.Path
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/jmap":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(accountGetBody))
		case r.Method == "PROPFIND":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(abMultistatus))
		case r.Method == "REPORT":
			w.WriteHeader(http.StatusMultiStatus)
			_, _ = w.Write([]byte(contactMultistatus))
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/vcard")
			_, _ = w.Write([]byte(vcardBody))
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(d.Server.Close)
	return d
}

// TestCardDAVRoundTripUsesEmailPrincipal drives the full CRUD surface
// through the resolver and asserts every CardDAV verb targets the
// email-keyed principal path (/dav/card/{email}/...), which is the
// whole point of the resolve-by-email fix: Stalwart 404s a bare login
// name or JMAP id.
func TestCardDAVRoundTripUsesEmailPrincipal(t *testing.T) {
	const abMS = `<multistatus xmlns="DAV:" xmlns:cs="urn:ietf:params:xml:ns:carddav">
  <response>
    <href>/dav/card/kmail-dev@kmail.dev/</href>
    <propstat><status>HTTP/1.1 200 OK</status><prop><resourcetype/></prop></propstat>
  </response>
  <response>
    <href>/dav/card/kmail-dev@kmail.dev/default/</href>
    <propstat><status>HTTP/1.1 200 OK</status><prop><displayname>Contacts</displayname><resourcetype><addressbook/></resourcetype></prop></propstat>
  </response>
</multistatus>`
	const contactMS = `<multistatus xmlns="DAV:" xmlns:c="urn:ietf:params:xml:ns:carddav">
  <response>
    <href>/dav/card/kmail-dev@kmail.dev/default/u1.vcf</href>
    <propstat><status>HTTP/1.1 200 OK</status><prop><address-data>BEGIN:VCARD
VERSION:4.0
UID:u1
FN:Ada Lovelace
EMAIL:ada@kmail.dev
END:VCARD</address-data></prop></propstat>
  </response>
</multistatus>`
	const vcard = "BEGIN:VCARD\r\nVERSION:4.0\r\nUID:u1\r\nFN:Ada Lovelace\r\nEMAIL:ada@kmail.dev\r\nEND:VCARD\r\n"

	srv := newDavServer(t, abMS, contactMS, vcard)
	svc := NewService(Config{StalwartURL: srv.URL, AdminUser: "admin", AdminPassword: "pw"})
	ctx := context.Background()

	books, err := svc.ListAddressBooks(ctx, "b")
	if err != nil {
		t.Fatalf("ListAddressBooks: %v", err)
	}
	if len(books) != 1 || books[0].ID != "default" || !books[0].IsDefault {
		t.Fatalf("ListAddressBooks=%+v want one default book id=default", books)
	}
	if p := srv.lastPath["PROPFIND"]; p != "/dav/card/kmail-dev@kmail.dev/" {
		t.Fatalf("PROPFIND path=%q want email-keyed home", p)
	}

	contacts, err := svc.GetContacts(ctx, "b", "default")
	if err != nil {
		t.Fatalf("GetContacts: %v", err)
	}
	if len(contacts) != 1 || contacts[0].FN != "Ada Lovelace" || contacts[0].UID != "u1" {
		t.Fatalf("GetContacts=%+v want one parsed contact", contacts)
	}
	if p := srv.lastPath["REPORT"]; p != "/dav/card/kmail-dev@kmail.dev/default/" {
		t.Fatalf("REPORT path=%q want email-keyed addressbook", p)
	}

	got, err := svc.GetContact(ctx, "b", "default", "u1")
	if err != nil {
		t.Fatalf("GetContact: %v", err)
	}
	if got.FN != "Ada Lovelace" || len(got.Emails) != 1 || got.Emails[0] != "ada@kmail.dev" {
		t.Fatalf("GetContact=%+v want Ada Lovelace", got)
	}
	if p := srv.lastPath["GET"]; p != "/dav/card/kmail-dev@kmail.dev/default/u1.vcf" {
		t.Fatalf("GET path=%q want email-keyed contact resource", p)
	}

	uid, err := svc.CreateContact(ctx, "b", "default", ContactDraft{UID: "u1", FN: "Ada Lovelace"})
	if err != nil {
		t.Fatalf("CreateContact: %v", err)
	}
	if uid != "u1" {
		t.Fatalf("CreateContact uid=%q want u1", uid)
	}
	if p := srv.lastPath["PUT"]; p != "/dav/card/kmail-dev@kmail.dev/default/u1.vcf" {
		t.Fatalf("PUT path=%q want email-keyed contact resource", p)
	}

	if err := svc.UpdateContact(ctx, "b", "default", "u1", ContactDraft{FN: "Ada B. Lovelace"}); err != nil {
		t.Fatalf("UpdateContact: %v", err)
	}
	if err := svc.DeleteContact(ctx, "b", "default", "u1"); err != nil {
		t.Fatalf("DeleteContact: %v", err)
	}
	if p := srv.lastPath["DELETE"]; p != "/dav/card/kmail-dev@kmail.dev/default/u1.vcf" {
		t.Fatalf("DELETE path=%q want email-keyed contact resource", p)
	}
}

// TestServiceInputValidation covers the cheap guard branches that
// reject malformed calls before any network I/O.
func TestServiceInputValidation(t *testing.T) {
	svc := NewService(Config{StalwartURL: "http://stalwart:8080"})
	ctx := context.Background()

	cases := []struct {
		name string
		err  error
	}{
		{"ListAddressBooks no account", firstErr(func() error { _, e := svc.ListAddressBooks(ctx, ""); return e })},
		{"GetContacts no book", firstErr(func() error { _, e := svc.GetContacts(ctx, "b", ""); return e })},
		{"GetContact no uid", firstErr(func() error { _, e := svc.GetContact(ctx, "b", "default", ""); return e })},
		{"CreateContact no fn", firstErr(func() error { _, e := svc.CreateContact(ctx, "b", "default", ContactDraft{}); return e })},
		{"UpdateContact no uid", svc.UpdateContact(ctx, "b", "default", "", ContactDraft{FN: "x"})},
		{"DeleteContact no uid", svc.DeleteContact(ctx, "b", "default", "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, ErrInvalidInput) {
				t.Fatalf("err=%v want ErrInvalidInput", tc.err)
			}
		})
	}
}

func firstErr(f func() error) error { return f() }

// TestVCardBuildParseRoundTrip asserts BuildVCard -> ParseVCard
// preserves every modelled field through RFC 6350 escaping, including
// values that contain the structural characters ( , ; \ ) and the
// multi-value EMAIL / TEL / CATEGORIES lists.
func TestVCardBuildParseRoundTrip(t *testing.T) {
	in := ContactDraft{
		UID:      "u-42",
		FN:       `Doe, John; "Ada\Tina"`,
		Emails:   []string{"a@kmail.dev", "b@kmail.dev"},
		Phones:   []string{"+1-555-0100", "+1-555-0199"},
		Org:      "Acme, Inc.; R&D",
		Note:     "first line\nsecond; line",
		PhotoURL: "https://cdn.kmail.dev/p/u-42.png",
		Groups:   []string{"work", "vip, gold"},
	}
	got := ParseVCard(BuildVCard(in))

	if got.UID != in.UID || got.FN != in.FN || got.Org != in.Org || got.Note != in.Note || got.PhotoURL != in.PhotoURL {
		t.Fatalf("scalar round-trip mismatch: got %+v want %+v", got, in)
	}
	if strings.Join(got.Emails, "|") != strings.Join(in.Emails, "|") {
		t.Fatalf("emails=%v want %v", got.Emails, in.Emails)
	}
	if strings.Join(got.Phones, "|") != strings.Join(in.Phones, "|") {
		t.Fatalf("phones=%v want %v", got.Phones, in.Phones)
	}
	if strings.Join(got.Groups, "|") != strings.Join(in.Groups, "|") {
		t.Fatalf("groups=%v want %v", got.Groups, in.Groups)
	}
}

func TestEscapeUnescapeVCardValue(t *testing.T) {
	for _, raw := range []string{
		"plain",
		"a,b,c",
		"a;b;c",
		`back\slash`,
		"multi\nline",
		`all , ; \ end`,
		"",
	} {
		if got := unescapeVCardValue(escapeVCardValue(raw)); got != raw {
			t.Fatalf("round-trip %q -> %q", raw, got)
		}
	}
}

func TestSplitVCardList(t *testing.T) {
	// "vip\, gold" is a single escaped item; the unescaped commas
	// separate the list, escaped commas stay inside an item.
	got := splitVCardList(`work,vip\, gold,family`)
	want := []string{"work", "vip, gold", "family"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("splitVCardList=%v want %v", got, want)
	}
}
