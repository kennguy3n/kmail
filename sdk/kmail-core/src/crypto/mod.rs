// Crypto primitives shared by Confidential Send and Zero-Access
// Vault decryption.
//
// What lives here:
//
//   aead.rs     — AES-256-GCM encrypt / decrypt with explicit
//                 12-byte nonces (matches RFC 5116 §5.1 and the
//                 zk-object-fabric `StrictZK` envelope).
//   kdf.rs      — HKDF-SHA256 per RFC 5869 (extract / expand,
//                 expand-only, and a domain-tagged convenience
//                 wrapper used to derive the Confidential-Send DEK
//                 wrapping key from an MLS leaf secret).
//   keystore.rs — `KeyStore` trait + `InMemoryKeyStore` reference
//                 implementation. Platform shells provide native
//                 backings (iOS Keychain Services, Android
//                 Keystore, OS keyring on desktop) via the FFI
//                 callback interface declared in
//                 `kmail-ffi/kmail.udl`.
//
// What does NOT live here:
//
//   - MLS protocol implementation. The KChat MLS SDK owns that
//     surface; the SDK only consumes raw secrets exported from it.
//   - Key wrapping format negotiation. The SDK only knows
//     `AES-256-GCM with a 12-byte nonce`. The BFF and the MLS
//     layer commit to that envelope shape — see
//     docs/ARCHITECTURE.md §5.

pub mod aead;
pub mod kdf;
pub mod keystore;

pub use aead::{aes_gcm_decrypt, aes_gcm_encrypt, AeadEnvelope, NONCE_LEN, TAG_LEN};
pub use kdf::{hkdf_derive, hkdf_expand_only, hkdf_extract, KdfLabel};
pub use keystore::{InMemoryKeyStore, KeyHandle, KeyMaterial, KeyStore};
