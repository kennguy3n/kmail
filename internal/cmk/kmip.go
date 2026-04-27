// Package cmk — KMIP TTLV wire protocol (subset).
//
// KMail's HSM provider talks KMIP 1.4 over TLS to envelope-
// encrypt and decrypt the per-tenant DEKs that wrap message
// bodies in the privacy plan. Phase 6 shipped a stub that only
// validated the endpoint URL; Phase 8 closes the loop with real
// wire traffic over the standard 5696/tcp port.
//
// We implement only the subset of KMIP this product needs: the
// `Locate`, `Encrypt`, and `Decrypt` operations, plus the request
// / response framing required to round-trip them. Any KMIP
// feature outside that subset (asymmetric ops, register, get,
// rekey, etc.) is documented in `docs/SECURITY.md` as
// "out-of-scope" — operators should drive those flows from the
// HSM appliance's own console, not through KMail.
//
// Encoding is TTLV per the OASIS KMIP Specification §9.1:
//
//   3-byte tag │ 1-byte type │ 4-byte BE length │ value (padded to 8)
//
// Numeric types use big-endian; length excludes padding bytes.
// We pad to 8-byte alignment for the value as KMIP requires.

package cmk

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// KMIP 1.4 TTLV item types (subset).
const (
	ttlvStructure   byte = 0x01
	ttlvInteger     byte = 0x02
	ttlvLongInteger byte = 0x03
	ttlvEnumeration byte = 0x05
	ttlvByteString  byte = 0x08
	ttlvTextString  byte = 0x07
)

// KMIP 1.4 tags (subset relevant to Locate / Encrypt / Decrypt).
const (
	tagRequestMessage           uint32 = 0x420078
	tagRequestHeader            uint32 = 0x420077
	tagProtocolVersion          uint32 = 0x420069
	tagProtocolVersionMajor     uint32 = 0x42006A
	tagProtocolVersionMinor     uint32 = 0x42006B
	tagBatchCount               uint32 = 0x42000D
	tagBatchItem                uint32 = 0x42000F
	tagOperation                uint32 = 0x42005C
	tagRequestPayload           uint32 = 0x420079
	tagResponseMessage          uint32 = 0x42007B
	tagResponseHeader           uint32 = 0x42007A
	tagResponsePayload          uint32 = 0x42007C
	tagResultStatus             uint32 = 0x42007F
	tagResultReason             uint32 = 0x42007E
	tagResultMessage            uint32 = 0x42007D
	tagUniqueBatchItemID        uint32 = 0x420093
	tagUniqueIdentifier         uint32 = 0x420094
	tagAttribute                uint32 = 0x420008
	tagAttributeName            uint32 = 0x42000A
	tagAttributeValue           uint32 = 0x42000B
	tagCryptographicParameters  uint32 = 0x42002B
	tagBlockCipherMode          uint32 = 0x420011
	tagCryptographicAlgorithm   uint32 = 0x420028
	tagData                     uint32 = 0x4200C2
	tagIVCounterNonce           uint32 = 0x42003D
)

// KMIP operations.
const (
	opLocate  int32 = 0x00000008
	opEncrypt int32 = 0x0000001F
	opDecrypt int32 = 0x00000020
)

// KMIP cryptographic algorithm codes (subset).
const (
	algAES int32 = 0x00000003
)

// KMIP block cipher modes (subset).
const (
	bcmGCM int32 = 0x00000005
)

// KMIPClient encodes the wire transport plus credentials for a
// single appliance. Construction is cheap; the underlying
// connection is opened per call so connection pooling is the
// caller's responsibility.
type KMIPClient struct {
	Address  string
	TLS      *tls.Config
	Timeout  time.Duration
	Username string
	Password string
}

// NewKMIPClient builds a client. `addr` is host:port. `tls.Config`
// must be supplied — KMIP requires TLS in production. A nil
// config falls back to a permissive default that still verifies
// server identity (no InsecureSkipVerify).
func NewKMIPClient(addr string, tlsCfg *tls.Config) *KMIPClient {
	if tlsCfg == nil {
		tlsCfg = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &KMIPClient{Address: addr, TLS: tlsCfg, Timeout: 15 * time.Second}
}

// Encrypt asks the HSM to encrypt `plaintext` under the symmetric
// key identified by `keyID`. Returns ciphertext + IV.
func (c *KMIPClient) Encrypt(keyID string, plaintext []byte) (ciphertext, iv []byte, err error) {
	resp, err := c.exchange(opEncrypt, encodeEncryptRequest(keyID, plaintext))
	if err != nil {
		return nil, nil, err
	}
	return parseEncryptResponse(resp)
}

// Decrypt asks the HSM to decrypt `ciphertext` (with `iv`) under
// the key identified by `keyID`. Returns plaintext.
func (c *KMIPClient) Decrypt(keyID string, ciphertext, iv []byte) ([]byte, error) {
	resp, err := c.exchange(opDecrypt, encodeDecryptRequest(keyID, ciphertext, iv))
	if err != nil {
		return nil, err
	}
	return parseDecryptResponse(resp)
}

// Locate asks the HSM for keys matching the supplied attribute
// name (the first match wins). Used to convert human-readable
// labels to KMIP unique identifiers.
func (c *KMIPClient) Locate(attributeName string) (string, error) {
	resp, err := c.exchange(opLocate, encodeLocateRequest(attributeName))
	if err != nil {
		return "", err
	}
	return parseLocateResponse(resp)
}

// exchange dials the appliance, frames a single batch request,
// reads the response, and returns the response payload bytes for
// the first batch item.
func (c *KMIPClient) exchange(operation int32, payload []byte) ([]byte, error) {
	if c.Address == "" {
		return nil, errors.New("kmip: address required")
	}
	dialer := &net.Dialer{Timeout: c.Timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", c.Address, c.TLS)
	if err != nil {
		return nil, fmt.Errorf("kmip: tls dial: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(c.Timeout)
	_ = conn.SetDeadline(deadline)
	req := encodeRequestMessage(operation, payload)
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("kmip: write: %w", err)
	}
	respMsg, err := readTTLV(conn)
	if err != nil {
		return nil, fmt.Errorf("kmip: read: %w", err)
	}
	return extractResponsePayload(respMsg)
}

// encodeRequestMessage wraps the operation-specific payload in a
// `RequestMessage` frame (header + single batch item).
func encodeRequestMessage(operation int32, payload []byte) []byte {
	header := encodeStructure(tagRequestHeader,
		concat(
			encodeStructure(tagProtocolVersion,
				concat(
					encodeInteger(tagProtocolVersionMajor, 1),
					encodeInteger(tagProtocolVersionMinor, 4),
				),
			),
			encodeInteger(tagBatchCount, 1),
		),
	)
	batchItem := encodeStructure(tagBatchItem,
		concat(
			encodeEnumeration(tagOperation, operation),
			encodeStructure(tagRequestPayload, payload),
		),
	)
	return encodeStructure(tagRequestMessage, concat(header, batchItem))
}

// encodeEncryptRequest builds the operation-specific Encrypt
// request payload.
func encodeEncryptRequest(keyID string, plaintext []byte) []byte {
	return concat(
		encodeText(tagUniqueIdentifier, keyID),
		encodeStructure(tagCryptographicParameters,
			concat(
				encodeEnumeration(tagBlockCipherMode, bcmGCM),
				encodeEnumeration(tagCryptographicAlgorithm, algAES),
			),
		),
		encodeBytes(tagData, plaintext),
	)
}

// encodeDecryptRequest builds the Decrypt request payload.
func encodeDecryptRequest(keyID string, ciphertext, iv []byte) []byte {
	return concat(
		encodeText(tagUniqueIdentifier, keyID),
		encodeStructure(tagCryptographicParameters,
			concat(
				encodeEnumeration(tagBlockCipherMode, bcmGCM),
				encodeEnumeration(tagCryptographicAlgorithm, algAES),
			),
		),
		encodeBytes(tagData, ciphertext),
		encodeBytes(tagIVCounterNonce, iv),
	)
}

// encodeLocateRequest builds a Locate-by-Name request.
func encodeLocateRequest(name string) []byte {
	return encodeStructure(tagAttribute,
		concat(
			encodeText(tagAttributeName, "Name"),
			encodeText(tagAttributeValue, name),
		),
	)
}

// extractResponsePayload locates the first batch item's
// ResponsePayload bytes inside a parsed ResponseMessage.
func extractResponsePayload(msg []byte) ([]byte, error) {
	tag, typ, val, err := decodeTTLV(msg)
	if err != nil {
		return nil, err
	}
	if tag != tagResponseMessage || typ != ttlvStructure {
		return nil, fmt.Errorf("kmip: expected ResponseMessage, got tag=0x%06x type=%d", tag, typ)
	}
	rest := val
	for len(rest) > 0 {
		t, ty, v, err := decodeTTLV(rest)
		if err != nil {
			return nil, err
		}
		rest = rest[paddedLen(len(v)+8):]
		if t == tagBatchItem && ty == ttlvStructure {
			inner := v
			for len(inner) > 0 {
				it, ity, iv, err := decodeTTLV(inner)
				if err != nil {
					return nil, err
				}
				inner = inner[paddedLen(len(iv)+8):]
				if it == tagResultStatus && ity == ttlvEnumeration {
					if len(iv) < 4 {
						return nil, errors.New("kmip: short ResultStatus")
					}
					if int32(binary.BigEndian.Uint32(iv[:4])) != 0 {
						return nil, errors.New("kmip: server returned non-success ResultStatus")
					}
				}
				if it == tagResponsePayload && ity == ttlvStructure {
					return iv, nil
				}
			}
		}
	}
	return nil, errors.New("kmip: no ResponsePayload in ResponseMessage")
}

// parseEncryptResponse pulls the ciphertext + IV out of an
// Encrypt response payload.
func parseEncryptResponse(payload []byte) (ciphertext, iv []byte, err error) {
	rest := payload
	for len(rest) > 0 {
		t, ty, v, e := decodeTTLV(rest)
		if e != nil {
			return nil, nil, e
		}
		rest = rest[paddedLen(len(v)+8):]
		if t == tagData && ty == ttlvByteString {
			ciphertext = append(ciphertext[:0], v...)
		}
		if t == tagIVCounterNonce && ty == ttlvByteString {
			iv = append(iv[:0], v...)
		}
	}
	if ciphertext == nil {
		return nil, nil, errors.New("kmip: Encrypt response missing Data")
	}
	return ciphertext, iv, nil
}

// parseDecryptResponse pulls the plaintext out of a Decrypt
// response payload.
func parseDecryptResponse(payload []byte) ([]byte, error) {
	rest := payload
	for len(rest) > 0 {
		t, ty, v, e := decodeTTLV(rest)
		if e != nil {
			return nil, e
		}
		rest = rest[paddedLen(len(v)+8):]
		if t == tagData && ty == ttlvByteString {
			return append([]byte(nil), v...), nil
		}
	}
	return nil, errors.New("kmip: Decrypt response missing Data")
}

// parseLocateResponse pulls the first UniqueIdentifier out of a
// Locate response payload.
func parseLocateResponse(payload []byte) (string, error) {
	rest := payload
	for len(rest) > 0 {
		t, ty, v, e := decodeTTLV(rest)
		if e != nil {
			return "", e
		}
		rest = rest[paddedLen(len(v)+8):]
		if t == tagUniqueIdentifier && ty == ttlvTextString {
			return string(v), nil
		}
	}
	return "", errors.New("kmip: Locate response missing UniqueIdentifier")
}

// readTTLV reads a single TTLV-framed structure from r,
// returning the entire on-wire bytes (header + body, padded).
func readTTLV(r io.Reader) ([]byte, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(hdr[4:8])
	padded := paddedLen(int(length))
	body := make([]byte, padded)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return append(hdr[:], body[:length]...), nil
}

// decodeTTLV peels the next TTLV item off the front of `buf`.
func decodeTTLV(buf []byte) (tag uint32, typ byte, val []byte, err error) {
	if len(buf) < 8 {
		return 0, 0, nil, fmt.Errorf("kmip: short TTLV header (%d bytes)", len(buf))
	}
	tag = uint32(buf[0])<<16 | uint32(buf[1])<<8 | uint32(buf[2])
	typ = buf[3]
	length := binary.BigEndian.Uint32(buf[4:8])
	if int(length) > len(buf)-8 {
		return 0, 0, nil, fmt.Errorf("kmip: TTLV length %d exceeds remaining %d", length, len(buf)-8)
	}
	return tag, typ, buf[8 : 8+length], nil
}

// encodeStructure wraps `body` in a TTLV item with type
// `Structure`. Length is the unpadded body length; the returned
// buffer is padded to 8-byte alignment as KMIP requires.
func encodeStructure(tag uint32, body []byte) []byte {
	return encodeTTLV(tag, ttlvStructure, body)
}

// encodeInteger encodes a 32-bit integer (KMIP "Integer" type).
func encodeInteger(tag uint32, v int32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(v))
	return encodeTTLV(tag, ttlvInteger, buf[:])
}

// encodeEnumeration encodes a 32-bit enum.
func encodeEnumeration(tag uint32, v int32) []byte {
	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(v))
	return encodeTTLV(tag, ttlvEnumeration, buf[:])
}

// encodeText encodes a UTF-8 text string.
func encodeText(tag uint32, s string) []byte {
	return encodeTTLV(tag, ttlvTextString, []byte(s))
}

// encodeBytes encodes a binary blob (KMIP "ByteString" type).
func encodeBytes(tag uint32, b []byte) []byte {
	return encodeTTLV(tag, ttlvByteString, b)
}

// encodeTTLV is the shared TTLV emitter. Pads the value to
// 8-byte alignment as the KMIP spec requires for every primitive
// item except `Structure` (which is already aligned because its
// body is composed of items that are themselves padded).
func encodeTTLV(tag uint32, typ byte, val []byte) []byte {
	length := len(val)
	out := make([]byte, 8+paddedLen(length))
	out[0] = byte((tag >> 16) & 0xff)
	out[1] = byte((tag >> 8) & 0xff)
	out[2] = byte(tag & 0xff)
	out[3] = typ
	binary.BigEndian.PutUint32(out[4:8], uint32(length))
	copy(out[8:], val)
	return out
}

// paddedLen rounds n up to the next 8-byte multiple.
func paddedLen(n int) int {
	if n%8 == 0 {
		return n
	}
	return n + (8 - n%8)
}

// concat is `bytes.Join(parts, nil)` without the join allocation
// for small slice counts (TTLV requests are 3-6 items).
func concat(parts ...[]byte) []byte {
	total := 0
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
