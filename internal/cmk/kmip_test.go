package cmk

import (
	"bytes"
	"testing"
)

func TestEncodeDecodeIntegerRoundTrip(t *testing.T) {
	buf := encodeInteger(tagBatchCount, 1)
	tag, typ, val, err := decodeTTLV(buf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tag != tagBatchCount || typ != ttlvInteger {
		t.Fatalf("tag=%x typ=%d", tag, typ)
	}
	if len(val) != 4 || val[3] != 1 {
		t.Fatalf("val=%v", val)
	}
}

func TestEncodeStructureNests(t *testing.T) {
	body := concat(
		encodeInteger(tagBatchCount, 2),
		encodeText(tagAttributeName, "Name"),
	)
	frame := encodeStructure(tagRequestHeader, body)
	tag, typ, val, err := decodeTTLV(frame)
	if err != nil {
		t.Fatalf("decode outer: %v", err)
	}
	if tag != tagRequestHeader || typ != ttlvStructure {
		t.Fatalf("outer tag=%x typ=%d", tag, typ)
	}
	if !bytes.Equal(val, body) {
		t.Fatalf("structure body mismatch: got %x want %x", val, body)
	}
}

func TestRequestMessageFraming(t *testing.T) {
	frame := encodeRequestMessage(opLocate, encodeLocateRequest("kmail-cmk-tenant-1"))
	tag, typ, _, err := decodeTTLV(frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if tag != tagRequestMessage || typ != ttlvStructure {
		t.Fatalf("expected RequestMessage Structure, got tag=%x typ=%d", tag, typ)
	}
	if len(frame) < 16 {
		t.Fatalf("frame too short: %d", len(frame))
	}
}

func TestParseEncryptResponseExtractsData(t *testing.T) {
	payload := concat(
		encodeBytes(tagData, []byte("ciphertext")),
		encodeBytes(tagIVCounterNonce, []byte("nonce123")),
	)
	ct, iv, err := parseEncryptResponse(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(ct) != "ciphertext" || string(iv) != "nonce123" {
		t.Fatalf("ct=%q iv=%q", ct, iv)
	}
}

func TestPaddedLenAlignsTo8(t *testing.T) {
	cases := map[int]int{0: 0, 1: 8, 7: 8, 8: 8, 9: 16, 16: 16}
	for in, want := range cases {
		if got := paddedLen(in); got != want {
			t.Errorf("paddedLen(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestKMIPProviderValidateStillEnforcesShape(t *testing.T) {
	p := KMIPProvider{}
	cfg := HSMConfig{Provider: HSMKMIP, Endpoint: "kmips://hsm.example.com:5696"}
	if err := p.Validate(t.Context(), cfg, "user:pass"); err != nil {
		t.Fatalf("expected accept, got %v", err)
	}
}
