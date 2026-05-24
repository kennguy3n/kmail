// MLS key-material bridge.
//
// The SDK does NOT implement the MLS protocol. KChat owns the MLS
// surface (group membership, commits, welcome messages, leaf
// rotation) via the KChat MLS SDK; KMail consumes only the
// **exporter secrets** that the MLS SDK derives from the user's
// MLS group state. This is the single, load-bearing architectural
// rule from `docs/8c8a33156d774657b2fe4be485a103d0` / the do-not-
// do list:
//
//   "Do not build a parallel email-only key hierarchy. KMail
//    derives keys from KChat's MLS key tree."
//
// So this module declares only the **trait shape** that the
// platform shell must implement. Concrete impls live elsewhere:
//
//   - apps/ios/   — Swift shim that calls into the KChat MLS SDK
//                   on iOS (CoreFoundation / CryptoKit backed).
//   - apps/android/ — Kotlin shim using the Android port of the
//                     same KChat MLS SDK.
//   - apps/desktop/ — TypeScript shim in the Electron renderer
//                     bridging to the KChat MLS SDK's wasm build,
//                     exposed back to Rust via napi-rs.
//   - tests/      — `StaticMlsKeyProvider` below: stores raw bytes
//                   for a fixed-key-per-folder regression harness.
//
// All four impls satisfy the same contract: a 32-byte secret per
// `(scope, identifier)` pair, derived deterministically from the
// MLS state, returned via the trait method.
//
// Why a trait and not a function pointer / closure: the platform
// shell may need to access async state (e.g. iOS Keychain
// `SecItemCopyMatching`) when deriving the secret. The trait
// methods are synchronous because every supported platform's
// MLS-exporter API is itself synchronous at the point of use
// (it's a deterministic KDF over already-loaded state). If a
// shell needs to fetch the state from disk first, it does that
// in a one-time bootstrap before plugging the trait into the
// `KMailClient`, so the per-message exporter calls stay sync.

use crate::crypto::keystore::KeyMaterial;
use crate::error::Result;
use std::collections::HashMap;
use std::sync::Mutex;

/// The pure contract between KMail and the KChat MLS SDK.
///
/// Implementors MUST:
///   1. Return 32 bytes for every method (matches the SHA-256
///      hash length and the AES-256 key length the crypto module
///      consumes).
///   2. Return the **same** bytes for the **same** input over the
///      lifetime of one MLS epoch. The MLS exporter is
///      deterministic over the group state; the trait inherits
///      that determinism.
///   3. Return `Error::KeyStore(...)` if the underlying MLS state
///      is unavailable (user not yet enrolled, keychain locked).
///      The SDK propagates this back to the caller verbatim so
///      the UX can prompt the user to unlock / re-enrol.
///
/// Implementors MUST NOT:
///   - Return the user's raw MLS leaf private key here. The
///     method names are exporter-style for a reason: the
///     returned bytes are derived KDF outputs, not the long-term
///     MLS identity material.
///   - Cache the returned bytes outside the trait impl. The
///     `KeyMaterial` wrapper zeroes on drop (see `keystore.rs`);
///     keeping a `Vec<u8>` copy defeats that.
pub trait MlsKeyProvider: Send + Sync {
    /// Export the MLS leaf-derived secret used to wrap Confidential
    /// Send DEKs.
    ///
    /// The KChat MLS SDK derives this via
    /// `MLS.export_secret(label="kmail/v1/confidential-send",
    ///                    context=<recipient_user_id>, len=32)`.
    /// The `recipient_user_id` argument is the SDK-passed
    /// identifier that scopes the export to a specific recipient.
    ///
    /// Returning the SAME bytes for different recipients would be
    /// a critical correctness bug — a single wrapped DEK would be
    /// openable by anyone holding any user's leaf secret. The
    /// trait CANNOT verify uniqueness across implementors; this
    /// is a contract obligation on the shell.
    fn confidential_send_leaf_secret(&self, recipient_user_id: &str) -> Result<KeyMaterial>;

    /// Export the MLS credential-derived secret used as the
    /// per-folder master key for a Zero-Access Vault folder.
    ///
    /// The KChat MLS SDK derives this via
    /// `MLS.export_secret(label="kmail/v1/vault",
    ///                    context=<folder_id>, len=32)`. The
    /// folder ID is the SDK-passed identifier that scopes the
    /// derived key to a specific Vault folder; rotating the
    /// folder (e.g., user "rekey folder" action) MUST cause this
    /// to return a fresh value on the next call.
    fn vault_folder_master_secret(&self, folder_id: &str) -> Result<KeyMaterial>;
}

/// In-memory test implementation. NOT for production — only used
/// by the unit tests in this crate and by the `kmail-cli` debug
/// binary's `crypto roundtrip` subcommand.
///
/// Why we ship a test impl in `pub`: the FFI / napi binding
/// crates' integration tests need to exercise the full
/// seal-and-open path without pulling a real MLS implementation
/// into test scope (which would be a build-time dependency on
/// the KChat MLS SDK). A static map is the smallest possible
/// substitute that satisfies the trait contract.
///
/// Production code MUST NOT use this — the SDK doesn't expose a
/// helper that constructs it, and the FFI surface does NOT
/// provide a path to instantiate it from Swift / Kotlin /
/// TypeScript. The struct is `pub` for the SDK's own integration
/// tests in the `client.rs` module; downstream users must build
/// their own impl that delegates to the real MLS SDK.
///
/// Storage uses [`KeyMaterial`] (not raw `Vec<u8>`) so that the
/// secret bytes are zeroed on `Drop` / `HashMap::remove`. This is
/// deliberate even though the struct is test-only — it lets the
/// test impl demonstrate the same zeroize discipline that any
/// production [`MlsKeyProvider`] implementation MUST follow, so
/// a future maintainer skimming this file for reference doesn't
/// see a misleading example.
#[derive(Default)]
pub struct StaticMlsKeyProvider {
    confidential: Mutex<HashMap<String, KeyMaterial>>,
    vault: Mutex<HashMap<String, KeyMaterial>>,
}

impl StaticMlsKeyProvider {
    pub fn new() -> Self {
        Self::default()
    }

    /// Seed a Confidential Send leaf secret for `recipient_user_id`.
    /// Passing 32 bytes is enforced; anything else panics in test
    /// scope to surface the misuse early.
    pub fn with_confidential_secret(self, recipient_user_id: &str, secret: &[u8]) -> Self {
        assert_eq!(
            secret.len(),
            32,
            "MLS leaf secret must be 32 bytes (got {})",
            secret.len()
        );
        self.confidential.lock().unwrap().insert(
            recipient_user_id.to_string(),
            KeyMaterial::new(secret.to_vec()),
        );
        self
    }

    /// Seed a Vault folder master secret for `folder_id`. Same
    /// 32-byte constraint as the leaf secret.
    pub fn with_vault_secret(self, folder_id: &str, secret: &[u8]) -> Self {
        assert_eq!(
            secret.len(),
            32,
            "MLS folder master secret must be 32 bytes (got {})",
            secret.len()
        );
        self.vault
            .lock()
            .unwrap()
            .insert(folder_id.to_string(), KeyMaterial::new(secret.to_vec()));
        self
    }
}

impl MlsKeyProvider for StaticMlsKeyProvider {
    fn confidential_send_leaf_secret(&self, recipient_user_id: &str) -> Result<KeyMaterial> {
        let g = self.confidential.lock().map_err(|_| {
            crate::error::Error::KeyStore("StaticMlsKeyProvider mutex poisoned".into())
        })?;
        g.get(recipient_user_id).cloned().ok_or_else(|| {
            crate::error::Error::KeyStore(format!(
                "no Confidential Send secret seeded for recipient {recipient_user_id}"
            ))
        })
    }

    fn vault_folder_master_secret(&self, folder_id: &str) -> Result<KeyMaterial> {
        let g = self.vault.lock().map_err(|_| {
            crate::error::Error::KeyStore("StaticMlsKeyProvider mutex poisoned".into())
        })?;
        g.get(folder_id).cloned().ok_or_else(|| {
            crate::error::Error::KeyStore(format!(
                "no Vault folder secret seeded for folder {folder_id}"
            ))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::error::Error;

    #[test]
    fn static_provider_returns_seeded_confidential_secret() {
        let p =
            StaticMlsKeyProvider::new().with_confidential_secret("alice@kmail.test", &[0xAA; 32]);
        let secret = p.confidential_send_leaf_secret("alice@kmail.test").unwrap();
        assert_eq!(secret.as_slice(), &[0xAA; 32]);
    }

    #[test]
    fn static_provider_returns_seeded_vault_secret() {
        let p = StaticMlsKeyProvider::new().with_vault_secret("folder-vault-1", &[0xBB; 32]);
        let secret = p.vault_folder_master_secret("folder-vault-1").unwrap();
        assert_eq!(secret.as_slice(), &[0xBB; 32]);
    }

    /// Asking for an un-seeded scope must return a KeyStore
    /// error — distinct from a 32-byte-of-zeros default, which
    /// would silently produce decryption failures.
    #[test]
    fn static_provider_returns_keystore_error_for_unseeded() {
        let p = StaticMlsKeyProvider::new();
        let err = p
            .confidential_send_leaf_secret("nobody@kmail.test")
            .unwrap_err();
        assert!(matches!(err, Error::KeyStore(_)));
    }

    #[test]
    fn static_provider_distinguishes_recipients() {
        let p = StaticMlsKeyProvider::new()
            .with_confidential_secret("a@kmail.test", &[0xA1; 32])
            .with_confidential_secret("b@kmail.test", &[0xB1; 32]);
        assert_eq!(
            p.confidential_send_leaf_secret("a@kmail.test")
                .unwrap()
                .as_slice(),
            &[0xA1; 32]
        );
        assert_eq!(
            p.confidential_send_leaf_secret("b@kmail.test")
                .unwrap()
                .as_slice(),
            &[0xB1; 32]
        );
    }
}
