// Crypto primitives shared by Confidential Send and Zero-Access
// Vault flows.
//
// What lives here:
//
//   aead.rs         — AES-256-GCM encrypt / decrypt with explicit
//                     12-byte nonces (matches RFC 5116 §5.1 and the
//                     zk-object-fabric `StrictZK` envelope).
//   kdf.rs          — HKDF-SHA256 per RFC 5869 (extract / expand,
//                     expand-only, and a domain-tagged convenience
//                     wrapper used to derive Confidential-Send DEK
//                     wrap keys and Vault folder master keys).
//   keystore.rs     — `KeyStore` trait + `InMemoryKeyStore` reference
//                     implementation. Platform shells provide native
//                     backings (iOS Keychain Services, Android
//                     Keystore, OS keyring on desktop) via the FFI
//                     callback interface declared in
//                     `kmail-ffi/src/lib.rs`.
//   vault.rs        — Zero-Access Vault `seal` + `open`. One-layer
//                     construction: nonce drives both the HKDF salt
//                     and the AES-GCM nonce. See file header for
//                     the safety argument.
//   confidential.rs — Confidential Send `seal` + `open`. Two-layer
//                     construction: a random per-message DEK is
//                     wrapped under a KEK derived from the MLS
//                     leaf secret + a per-message salt. See file
//                     header for the rotation-via-envelope-drop
//                     rationale.
//   mls.rs          — `MlsKeyProvider` trait that the platform
//                     shell implements to bridge to the KChat
//                     MLS SDK. The SDK consumes MLS exporter
//                     secrets via this trait; it does NOT
//                     implement the MLS protocol itself.
//
// What does NOT live here:
//
//   - MLS protocol implementation. The KChat MLS SDK owns that
//     surface; this SDK only consumes raw secrets exported from
//     it via the `MlsKeyProvider` trait.
//   - Key wrapping format negotiation. The SDK only knows
//     `AES-256-GCM with a 12-byte nonce`. The BFF and the MLS
//     layer commit to that envelope shape — see
//     docs/ARCHITECTURE.md §5.

pub mod aead;
pub mod confidential;
pub mod kdf;
pub mod keystore;
pub mod mls;
pub mod vault;

pub use aead::{aes_gcm_decrypt, aes_gcm_encrypt, AeadEnvelope, NONCE_LEN, TAG_LEN};
pub use confidential::{ConfidentialEnvelope, DEK_LEN, KEK_SALT_LEN, MLS_LEAF_SECRET_LEN};
pub use kdf::{hkdf_derive, hkdf_expand_only, hkdf_extract, KdfLabel};
pub use keystore::{InMemoryKeyStore, KeyHandle, KeyMaterial, KeyStore};
pub use mls::{MlsKeyProvider, StaticMlsKeyProvider};
pub use vault::FOLDER_MASTER_KEY_LEN;
