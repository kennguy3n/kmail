package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/kmail/internal/cmk"
)

func TestEnvProvider_PrefixThenBare(t *testing.T) {
	ctx := context.Background()
	t.Setenv("KMAIL_FOO", "from-prefixed")
	t.Setenv("FOO", "from-bare")
	v, err := EnvProvider{}.Resolve(ctx, "FOO")
	if err != nil || v != "from-prefixed" {
		t.Fatalf("expected prefixed to win, got %q err=%v", v, err)
	}

	t.Setenv("KMAIL_ONLYBARE", "")
	t.Setenv("ONLYBARE", "bare-only")
	if v, err := (EnvProvider{}).Resolve(ctx, "ONLYBARE"); err != nil || v != "bare-only" {
		t.Fatalf("expected bare fallback, got %q err=%v", v, err)
	}
}

func TestEnvProvider_NotFound(t *testing.T) {
	_, err := EnvProvider{}.Resolve(context.Background(), "DEFINITELY_UNSET_VAR_XYZ")
	if !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got %v", err)
	}
}

func TestNew_UnknownBackend(t *testing.T) {
	if _, err := New(context.Background(), "does-not-exist", ""); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestNew_DefaultsToEnv(t *testing.T) {
	p, err := New(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Backend() != "env" {
		t.Fatalf("expected env backend, got %q", p.Backend())
	}
}

func TestRegisteredBackends_IncludesEnvAndVault(t *testing.T) {
	got := RegisteredBackends()
	want := map[string]bool{"env": false, "vault": false}
	for _, b := range got {
		if _, ok := want[b]; ok {
			want[b] = true
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("backend %q not registered (have %v)", name, got)
		}
	}
}

func TestRegisterBackend_DuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration")
		}
	}()
	RegisterBackend("env", func(context.Context, string) (Provider, error) { return EnvProvider{}, nil })
}

// --- Vault ---

func TestVaultProvider_ResolveKVv2(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "tok-123" {
			t.Errorf("missing/incorrect vault token: %q", r.Header.Get("X-Vault-Token"))
		}
		// KV v2 path: /v1/secret/data/kmail/prod
		if r.URL.Path != "/v1/secret/data/kmail/prod" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"data": map[string]any{
				"stripe_webhook": "whsec_live",
				"value":          "default-field",
			}},
		})
	}))
	defer srv.Close()

	v := &VaultProvider{Addr: srv.URL, Token: "tok-123", HTTP: srv.Client(), cache: map[string]cachedSecret{}}

	got, err := v.Resolve(context.Background(), "secret/kmail/prod#stripe_webhook")
	if err != nil || got != "whsec_live" {
		t.Fatalf("field resolve = %q err=%v", got, err)
	}
	// Default field is "value".
	got, err = v.Resolve(context.Background(), "secret/kmail/prod")
	if err != nil || got != "default-field" {
		t.Fatalf("default field resolve = %q err=%v", got, err)
	}
}

func TestVaultProvider_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "{}", http.StatusNotFound)
	}))
	defer srv.Close()
	v := &VaultProvider{Addr: srv.URL, Token: "t", HTTP: srv.Client(), cache: map[string]cachedSecret{}}
	if _, err := v.Resolve(context.Background(), "secret/missing#x"); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected ErrSecretNotFound, got %v", err)
	}
}

func TestVaultProvider_MalformedRef(t *testing.T) {
	v := &VaultProvider{Addr: "http://x", Token: "t", HTTP: http.DefaultClient, cache: map[string]cachedSecret{}}
	if _, err := v.Resolve(context.Background(), "no-slash"); err == nil {
		t.Fatal("expected error for reference without mount/path")
	}
}

func TestVaultProvider_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "sealed", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	v := &VaultProvider{Addr: srv.URL, Token: "t", HTTP: srv.Client(), cache: map[string]cachedSecret{}}
	_, err := v.Resolve(context.Background(), "secret/x#y")
	if err == nil || errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("expected transport error (not NotFound), got %v", err)
	}
}

func TestSplitRef(t *testing.T) {
	cases := []struct{ in, path, field string }{
		{"a/b#c", "a/b", "c"},
		{"a/b", "a/b", "value"},
		{"a/b#", "a/b", "value"},
	}
	for _, c := range cases {
		p, f := splitRef(c.in)
		if p != c.path || f != c.field {
			t.Errorf("splitRef(%q) = (%q,%q), want (%q,%q)", c.in, p, f, c.path, c.field)
		}
	}
}

// --- Rotation ---

func key(b byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = b
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestRotatingEnvelope_PrimaryRoundTrip(t *testing.T) {
	re, err := NewRotatingEnvelope(key(0x11))
	if err != nil {
		t.Fatal(err)
	}
	blob, err := re.Wrap([]byte("super-secret"))
	if err != nil {
		t.Fatal(err)
	}
	pt, wasEnc, err := re.Unwrap(blob)
	if err != nil || !wasEnc || string(pt) != "super-secret" {
		t.Fatalf("roundtrip failed: pt=%q enc=%v err=%v", pt, wasEnc, err)
	}
}

func TestRotatingEnvelope_DecryptsOldKeyAfterRotation(t *testing.T) {
	// Seal a blob under the OLD key only.
	old, err := cmk.NewAESGCMEnvelopeFromKeyMaterial(key(0xAA))
	if err != nil {
		t.Fatal(err)
	}
	legacyBlob, err := old.Wrap([]byte("written-before-rotation"))
	if err != nil {
		t.Fatal(err)
	}

	// Rotate: new primary, old key retired but still on the ring.
	re, err := NewRotatingEnvelope(key(0xBB), key(0xAA))
	if err != nil {
		t.Fatal(err)
	}
	if re.RetiredKeyCount() != 1 {
		t.Fatalf("expected 1 retired key, got %d", re.RetiredKeyCount())
	}

	// The old blob must still decrypt (zero-downtime read).
	pt, _, err := re.Unwrap(legacyBlob)
	if err != nil || string(pt) != "written-before-rotation" {
		t.Fatalf("old-key blob failed to decrypt after rotation: pt=%q err=%v", pt, err)
	}

	// New writes use the new key, and a ring WITHOUT the old key
	// can still read them (proves Wrap used primary, not retired).
	newBlob, err := re.Wrap([]byte("written-after-rotation"))
	if err != nil {
		t.Fatal(err)
	}
	reNewOnly, _ := NewRotatingEnvelope(key(0xBB))
	pt, _, err = reNewOnly.Unwrap(newBlob)
	if err != nil || string(pt) != "written-after-rotation" {
		t.Fatalf("new blob not sealed under primary: pt=%q err=%v", pt, err)
	}
}

func TestRotatingEnvelope_CorruptWhenKeyNotOnRing(t *testing.T) {
	orphan, _ := cmk.NewAESGCMEnvelopeFromKeyMaterial(key(0xCC))
	blob, _ := orphan.Wrap([]byte("unreadable"))

	re, _ := NewRotatingEnvelope(key(0x11), key(0x22)) // neither is 0xCC
	if _, _, err := re.Unwrap(blob); !errors.Is(err, cmk.ErrEnvelopeCorrupted) {
		t.Fatalf("expected ErrEnvelopeCorrupted when no key matches, got %v", err)
	}
}

func TestRotatingEnvelope_RejectsBadPrimary(t *testing.T) {
	if _, err := NewRotatingEnvelope("not-a-valid-key"); err == nil {
		t.Fatal("expected error for malformed primary key")
	}
}

func TestLoadEnvelope_FromEnv(t *testing.T) {
	t.Setenv("KMAIL_SECRETS_KEY", key(0x42))
	env, err := LoadEnvelope(context.Background(), EnvProvider{})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := env.Wrap([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if pt, _, err := env.Unwrap(blob); err != nil || string(pt) != "x" {
		t.Fatalf("roundtrip via LoadEnvelope failed: %v", err)
	}
}

func TestLoadEnvelope_UnsetPrimary(t *testing.T) {
	t.Setenv("KMAIL_SECRETS_KEY", "")
	t.Setenv("SECRETS_KEY", "")
	if _, err := LoadEnvelope(context.Background(), EnvProvider{}); err == nil {
		t.Fatal("expected error when KMAIL_SECRETS_KEY unset")
	}
}

func TestLoadEnvelope_WithRetiredList(t *testing.T) {
	t.Setenv("KMAIL_SECRETS_KEY", key(0x01))
	t.Setenv("KMAIL_SECRETS_KEY_RETIRED", key(0x02)+","+key(0x03))
	env, err := LoadEnvelope(context.Background(), EnvProvider{})
	if err != nil {
		t.Fatal(err)
	}
	re, ok := env.(*RotatingEnvelope)
	if !ok {
		t.Fatalf("expected *RotatingEnvelope, got %T", env)
	}
	if re.RetiredKeyCount() != 2 {
		t.Fatalf("expected 2 retired keys, got %d", re.RetiredKeyCount())
	}
}

// stubProvider returns a fixed value/error per ref so LoadEnvelope's
// error handling around the retired-key lookup can be exercised.
type stubProvider struct {
	primary      string
	retiredErr   error
	retiredValue string
	retiredIsSet bool
}

func (p stubProvider) Backend() string { return "stub" }

func (p stubProvider) Resolve(_ context.Context, ref string) (string, error) {
	switch ref {
	case "SECRETS_KEY":
		return p.primary, nil
	case "SECRETS_KEY_RETIRED":
		if p.retiredErr != nil {
			return "", p.retiredErr
		}
		if !p.retiredIsSet {
			return "", fmt.Errorf("%w: %q", ErrSecretNotFound, ref)
		}
		return p.retiredValue, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrSecretNotFound, ref)
	}
}

func TestLoadEnvelope_RetiredNotFoundIsTolerated(t *testing.T) {
	// A NotFound on the retired key is the normal "not mid-rotation"
	// case: LoadEnvelope must succeed with a single-key ring.
	p := stubProvider{primary: key(0x01)}
	env, err := LoadEnvelope(context.Background(), p)
	if err != nil {
		t.Fatalf("NotFound on retired key must be tolerated, got %v", err)
	}
	re, ok := env.(*RotatingEnvelope)
	if !ok {
		t.Fatalf("expected *RotatingEnvelope, got %T", env)
	}
	if re.RetiredKeyCount() != 0 {
		t.Fatalf("expected 0 retired keys, got %d", re.RetiredKeyCount())
	}
}

func TestLoadEnvelope_RetiredTransportErrorFailsHard(t *testing.T) {
	// A transport error (NOT NotFound) resolving the retired key must
	// propagate: booting with a partial keyring would silently fail
	// to decrypt rows still sealed under a retired key.
	boom := errors.New("vault: connection refused")
	p := stubProvider{primary: key(0x01), retiredErr: boom}
	_, err := LoadEnvelope(context.Background(), p)
	if err == nil {
		t.Fatal("expected a transport error resolving retired keys to fail LoadEnvelope")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("expected wrapped transport error, got %v", err)
	}
	if errors.Is(err, ErrSecretNotFound) {
		t.Fatal("transport error must not be conflated with ErrSecretNotFound")
	}
}
