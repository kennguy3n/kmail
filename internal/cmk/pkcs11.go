// Package cmk — PKCS#11 envelope encryption shim.
//
// The full cgo-backed wire path (C_Initialize / C_OpenSession /
// C_Encrypt / C_Decrypt) lives behind the `pkcs11` build tag so
// the default `go build` does not require a system C compiler or
// the OASIS PKCS#11 headers. Operators who run KMail with HSM
// envelope encryption build the API binary with
// `go build -tags pkcs11 ./cmd/kmail-api`, which links in the
// implementation in `pkcs11_cgo.go`.
//
// The default no-cgo build keeps the connection-shape validation
// from Phase 6 (PKCS11Provider.Validate above) and uses this
// `pkcs11Encrypt` / `pkcs11Decrypt` shim that returns a clear
// error so an admin who forgot the build tag sees a useful
// failure mode.
//
// Wire-level details for operators reading the source:
//
//   1. C_Initialize(NULL_PTR) on first use.
//   2. C_OpenSession(slotID, CKF_SERIAL_SESSION | CKF_RW_SESSION).
//   3. C_Login(USER, PIN).
//   4. C_FindObjectsInit + C_FindObjects(label="kmail-cmk-<tenant>")
//      to resolve the AES-256 wrapping key handle.
//   5. C_EncryptInit(CKM_AES_GCM, mech params with IV) +
//      C_Encrypt over the DEK plaintext.
//   6. Decrypt is the symmetric path with C_DecryptInit /
//      C_Decrypt.
//
// All steps are documented end-to-end in `docs/SECURITY.md` §HSM.

//go:build !pkcs11

package cmk

import (
	"context"
	"errors"
)

// errPKCS11NotBuilt is returned by the no-cgo shim. A clear
// message points operators at the build tag so they understand
// why their env var is being ignored.
var errPKCS11NotBuilt = errors.New("cmk.pkcs11: KMail was built without the `pkcs11` build tag — rebuild the API binary with `go build -tags pkcs11 ./cmd/kmail-api` to enable C_Initialize / C_OpenSession / C_Encrypt / C_Decrypt wire calls")

// pkcs11Encrypt is the no-cgo build's stand-in for the real
// cgo-backed encrypt operation. It always returns
// errPKCS11NotBuilt so an admin sees a useful message rather
// than a silent fallback to the in-process AEAD.
//
// The `creds` parameter is the unwrapped PKCS#11 PIN bytes
// (loaded from `kmail-secrets`-envelope storage by the caller).
// The no-cgo shim does not use it, but the parameter is retained
// in the signature so the cgo build (`pkcs11_cgo.go`) can pass
// the PIN to `C_Login` without a future signature change rippling
// through every caller in `hsm.go`.
//
// Lifetime contract: Go's garbage collector and immutable strings
// make true credential zeroing impractical (the runtime may copy
// the underlying memory at any GC tick, and a `string(creds)`
// conversion produces an unreachable copy on the heap). What we
// CAN do is keep the plaintext lifetime as short as possible:
// `EncryptDEK` / `DecryptDEK` in `hsm.go` defer a best-effort
// `clear(creds)` immediately after the wire call so the PIN is
// overwritten on the stack-allocated slice header before the
// function returns. The cgo build should also avoid converting
// `creds` to `string` and instead hand the byte slice straight to
// `C.CString` + a `defer C.free` so the C-side copy is zeroed
// explicitly on the way out.
func pkcs11Encrypt(_ context.Context, _ HSMConfig, _ []byte, _ string, _ []byte) (ciphertext, iv []byte, err error) {
	return nil, nil, errPKCS11NotBuilt
}

// pkcs11Decrypt mirrors pkcs11Encrypt. The PIN credential
// argument is documented on pkcs11Encrypt.
func pkcs11Decrypt(_ context.Context, _ HSMConfig, _ []byte, _ string, _, _ []byte) ([]byte, error) {
	return nil, errPKCS11NotBuilt
}
