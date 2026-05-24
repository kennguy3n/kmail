// Platform-keychain abstraction.
//
// Native shells provide a `KeyStore` implementation backed by:
//   - iOS: Keychain Services (`SecItem*` family, Data Protection
//          class `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`).
//   - Android: Android Keystore via `AndroidKeyStore` provider
//             with hardware-backed extraction when the device
//             supports a TEE / SE.
//   - Desktop (Electron): the OS keyring (`keytar` / `node-keytar`
//                         / Secret Service on Linux).
//
// The trait surface is intentionally small — store / fetch /
// delete — because keychain APIs have wildly different ergonomics
// and trying to model attributes (access groups, biometric gating,
// kex sharing) in a portable trait yields a leaky lowest-common-
// denominator. Higher-level policy lives in the platform shells.
//
// `InMemoryKeyStore` is the reference implementation used by
// integration tests and the `kmail-cli` debug binary. It has NO
// persistence and exists purely so the upper layers can be tested
// without a real keychain.

use crate::error::{Error, Result};
use std::collections::HashMap;
use std::sync::Mutex;
use zeroize::{Zeroize, ZeroizeOnDrop};

/// Opaque key handle. The string layout is implementation-defined;
/// for the in-memory store it's just a UUID, but a real iOS
/// implementation might return a SecItem ref-string.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct KeyHandle(pub String);

/// Key material wrapper that zeroes itself on drop.
///
/// Implementations of `KeyStore` MUST return material via this
/// wrapper to keep secret bytes out of long-lived stack frames.
#[derive(Clone, Zeroize, ZeroizeOnDrop)]
pub struct KeyMaterial(pub Vec<u8>);

impl KeyMaterial {
    pub fn new(bytes: Vec<u8>) -> Self {
        Self(bytes)
    }

    pub fn as_slice(&self) -> &[u8] {
        &self.0
    }

    pub fn len(&self) -> usize {
        self.0.len()
    }

    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }
}

impl std::fmt::Debug for KeyMaterial {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("KeyMaterial")
            .field("len", &self.0.len())
            .field("bytes", &"[redacted]")
            .finish()
    }
}

/// Trait implemented by each platform's keychain bridge.
///
/// All methods are synchronous because every supported platform
/// exposes a synchronous keychain API at the C level. The UniFFI
/// callback interface mirrors this trait verbatim.
pub trait KeyStore: Send + Sync {
    /// Store key material under a stable label. Returns a handle
    /// the caller stores in their domain table.
    fn put(&self, label: &str, material: &[u8]) -> Result<KeyHandle>;

    /// Retrieve material previously stored under `handle`. Returns
    /// `None` if the handle was deleted (e.g. user wiped data) —
    /// callers must treat this as a re-auth / re-enrolment trigger.
    fn get(&self, handle: &KeyHandle) -> Result<Option<KeyMaterial>>;

    /// Delete the material referenced by `handle`. Idempotent.
    fn delete(&self, handle: &KeyHandle) -> Result<()>;

    /// Optional metadata helper used by the desktop "Manage keys"
    /// pane. Implementations may return an empty list.
    fn list(&self) -> Result<Vec<KeyHandle>>;
}

/// Reference implementation. Backed by a `Mutex<HashMap>`.
///
/// Concurrent access is supported (the desktop client opens
/// multiple `KMailClient`s for multi-account use), but bytes are
/// never written to disk. Suitable for tests and for the
/// `kmail-cli` debug binary.
#[derive(Default)]
pub struct InMemoryKeyStore {
    inner: Mutex<HashMap<KeyHandle, Vec<u8>>>,
}

impl InMemoryKeyStore {
    pub fn new() -> Self {
        Self::default()
    }
}

impl KeyStore for InMemoryKeyStore {
    fn put(&self, label: &str, material: &[u8]) -> Result<KeyHandle> {
        if label.is_empty() {
            return Err(Error::InvalidArgument("label is empty".into()));
        }
        if material.is_empty() {
            return Err(Error::InvalidArgument("key material is empty".into()));
        }
        let handle = KeyHandle(format!("mem:{}:{}", label, uuid::Uuid::new_v4()));
        let mut g = self
            .inner
            .lock()
            .map_err(|_| Error::KeyStore("in-memory keystore poisoned".into()))?;
        g.insert(handle.clone(), material.to_vec());
        Ok(handle)
    }

    fn get(&self, handle: &KeyHandle) -> Result<Option<KeyMaterial>> {
        let g = self
            .inner
            .lock()
            .map_err(|_| Error::KeyStore("in-memory keystore poisoned".into()))?;
        Ok(g.get(handle).cloned().map(KeyMaterial::new))
    }

    fn delete(&self, handle: &KeyHandle) -> Result<()> {
        let mut g = self
            .inner
            .lock()
            .map_err(|_| Error::KeyStore("in-memory keystore poisoned".into()))?;
        if let Some(mut v) = g.remove(handle) {
            v.zeroize();
        }
        Ok(())
    }

    fn list(&self) -> Result<Vec<KeyHandle>> {
        let g = self
            .inner
            .lock()
            .map_err(|_| Error::KeyStore("in-memory keystore poisoned".into()))?;
        Ok(g.keys().cloned().collect())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn in_memory_roundtrip() {
        let ks = InMemoryKeyStore::new();
        let h = ks.put("mls/leaf/v1", &[0xAA; 32]).unwrap();
        let got = ks.get(&h).unwrap().unwrap();
        assert_eq!(got.as_slice(), &[0xAA; 32]);

        ks.delete(&h).unwrap();
        assert!(ks.get(&h).unwrap().is_none());
        // Idempotent.
        ks.delete(&h).unwrap();
    }

    #[test]
    fn empty_label_or_material_is_invalid() {
        let ks = InMemoryKeyStore::new();
        assert!(matches!(
            ks.put("", &[1, 2, 3]),
            Err(Error::InvalidArgument(_))
        ));
        assert!(matches!(ks.put("k", &[]), Err(Error::InvalidArgument(_))));
    }

    /// Verify `KeyMaterial`'s Debug formatter never spills secret
    /// bytes — important for log capture in CI.
    #[test]
    fn key_material_debug_redacts() {
        let m = KeyMaterial::new(vec![0xDE, 0xAD, 0xBE, 0xEF]);
        let s = format!("{m:?}");
        assert!(s.contains("len: 4"));
        assert!(s.contains("redacted"));
        assert!(!s.contains("DE") && !s.contains("de"));
    }
}
