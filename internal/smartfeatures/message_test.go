package smartfeatures

import "testing"

func TestAddressDomainAndNormalized(t *testing.T) {
	a := Address{Email: "Alice@Example.COM"}
	if a.Domain() != "example.com" {
		t.Fatalf("Domain = %q", a.Domain())
	}
	if a.Normalized() != "alice@example.com" {
		t.Fatalf("Normalized = %q", a.Normalized())
	}
	if (Address{Email: "bogus"}).Domain() != "" {
		t.Fatalf("expected empty domain for address with no @")
	}
}

func TestMessageHeaderCaseInsensitive(t *testing.T) {
	m := Message{Headers: map[string]string{"List-Unsubscribe": "<x>"}}
	if m.Header("list-unsubscribe") != "<x>" {
		t.Fatalf("case-insensitive header lookup failed")
	}
	if !m.HasHeader("LIST-UNSUBSCRIBE") {
		t.Fatalf("HasHeader case-insensitive failed")
	}
	if m.HasHeader("Missing") {
		t.Fatalf("HasHeader returned true for missing header")
	}
}

func TestParseAddressList(t *testing.T) {
	got := ParseAddressList(`"Alice" <alice@example.com>, bob@example.com`)
	if len(got) != 2 {
		t.Fatalf("expected 2 addresses, got %d (%#v)", len(got), got)
	}
	if got[0].Email != "alice@example.com" || got[0].Name != "Alice" {
		t.Fatalf("first address wrong: %#v", got[0])
	}
	if got[1].Email != "bob@example.com" {
		t.Fatalf("second address wrong: %#v", got[1])
	}
	if ParseAddressList("   ") != nil {
		t.Fatalf("expected nil for blank input")
	}
}
