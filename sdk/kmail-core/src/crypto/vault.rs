// Zero-Access Vault envelope: seal + open.
//
// Construction (one-layer, salt-as-nonce):
//
//   nonce      = OsRng.fill(12)
//   DEK        = HKDF-SHA256(salt=nonce, ikm=folder_master_key,
//                            info=VaultFolderMaster label, len=32)
//   ciphertext = AES-256-GCM(key=DEK, nonce=nonce, plaintext, aad)
//
// The same nonce is consumed twice — once as the HKDF salt that
// derives the DEK, and once as the AES-GCM nonce that
// authenticates the ciphertext. That is NOT nonce reuse: HKDF is
// a one-way function, so a fresh `nonce` produces a fresh `DEK`,
// which means the (key, nonce) pair fed into AES-GCM is unique
// per message even though `nonce` appears in both roles. The
// construction is equivalent to a "nonce-derived key" scheme in
// the NIST SP 800-108 KBKDF family — see RFC 9180 §5.1 for the
// HPKE rationale (the only difference here is we don't have a
// public-key KEM step because the keying material was already
// exported from MLS by the platform shell).
//
// Two safety conditions make this sound:
//
//   1. `nonce` is sampled from `OsRng` at seal time. A 96-bit
//      uniform-random nonce has birthday collision probability
//      ~2^-32 at 2^48 messages, which is well past any realistic
//      vault-folder write count.
//   2. `open()` does NOT re-derive a random nonce — it consumes
//      the nonce from the envelope. So a corrupted envelope where
//      a third party flips the nonce will produce a different DEK
//      and AES-GCM authentication will fail, surfacing as
//      `Error::Decryption("authentication failed")`.
//
// The architecture is fixed by docs/ARCHITECTURE.md §5 ("Zero-
// Access Vault — server cannot read plaintext"). This module is
// the only place in the SDK that gets to choose the nonce; every
// other layer (`KMailClient`, FFI / napi bindings) calls into
// here. That centralisation is what lets us assert the nonce-
// uniqueness invariant in tests.

use crate::crypto::{aead, aes_gcm_decrypt, aes_gcm_encrypt, hkdf_derive, AeadEnvelope, KdfLabel};
use crate::error::{Error, Result};
use rand::RngCore;

/// Length of the per-folder master key (matches AES-256 key size).
///
/// The platform shell exports this from the MLS credential via
/// `MlsKeyProvider::vault_folder_master_secret`. We re-state the
/// constraint here so a caller threading raw bytes through
/// `KMailClient::seal_vault_envelope` gets a typed early-rejection
/// rather than a downstream AES-GCM "invalid key length" error.
pub const FOLDER_MASTER_KEY_LEN: usize = 32;

/// Seal `plaintext` under a per-folder master key into the Vault
/// envelope format consumed by [`open`].
///
/// A fresh 12-byte nonce is sampled from `OsRng` per call. The
/// nonce is the load-bearing freshness input: a fresh nonce
/// produces a fresh DEK via HKDF, which means a fresh
/// (key, nonce) pair is fed into AES-GCM even though the same
/// 12 bytes appear in both roles. See the module-level comment
/// for the safety argument.
///
/// `aad` is bound into the AES-GCM tag but NOT encrypted —
/// platform shells pass the email ID / mailbox ID / epoch tuple
/// here so that a server-side replay of an old envelope into a
/// different mailbox is detected as authentication failure.
pub fn seal(folder_master_key: &[u8], plaintext: &[u8], aad: &[u8]) -> Result<AeadEnvelope> {
    if folder_master_key.len() != FOLDER_MASTER_KEY_LEN {
        return Err(Error::InvalidArgument(format!(
            "folder master key must be {FOLDER_MASTER_KEY_LEN} bytes, got {}",
            folder_master_key.len()
        )));
    }
    let mut nonce = [0u8; aead::NONCE_LEN];
    rand::thread_rng().fill_bytes(&mut nonce);
    let dek = hkdf_derive(&nonce, folder_master_key, KdfLabel::VaultFolderMaster, 32)?;
    aes_gcm_encrypt(&dek, &nonce, plaintext, aad)
}

/// Inverse of [`seal`].
///
/// `envelope.nonce` MUST be the nonce that was sealed under the
/// envelope; flipping it (or any byte of `ciphertext` / `aad`)
/// will produce a different DEK, the AES-GCM tag will not
/// verify, and the call returns
/// `Error::Decryption("authentication failed")` — exactly the
/// same surface the existing `aead::aes_gcm_decrypt` produces
/// for a tag-mismatch under a constant key, so UX can render
/// "this message is corrupted or not for you" without branching
/// on the underlying cause.
pub fn open(folder_master_key: &[u8], envelope: &AeadEnvelope) -> Result<Vec<u8>> {
    if folder_master_key.len() != FOLDER_MASTER_KEY_LEN {
        return Err(Error::InvalidArgument(format!(
            "folder master key must be {FOLDER_MASTER_KEY_LEN} bytes, got {}",
            folder_master_key.len()
        )));
    }
    let dek = hkdf_derive(
        &envelope.nonce,
        folder_master_key,
        KdfLabel::VaultFolderMaster,
        32,
    )?;
    aes_gcm_decrypt(&dek, envelope)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;

    fn fixture_key() -> [u8; FOLDER_MASTER_KEY_LEN] {
        // Arbitrary but deterministic test key. NOT a real MLS
        // exporter output — purely a stand-in so the round-trip
        // doesn't depend on an MLS implementation in test scope.
        let mut k = [0u8; FOLDER_MASTER_KEY_LEN];
        for (i, b) in k.iter_mut().enumerate() {
            *b = i as u8;
        }
        k
    }

    #[test]
    fn roundtrip_smoke() {
        let key = fixture_key();
        let env = seal(&key, b"hello vault", b"email:42 mbx:vault").unwrap();
        let pt = open(&key, &env).unwrap();
        assert_eq!(pt, b"hello vault");
    }

    /// The empty plaintext case is the one most likely to trip a
    /// length-check bug. Pinning it here so an over-eager
    /// "non-empty plaintext required" guard added in the future
    /// gets caught.
    #[test]
    fn roundtrip_empty_plaintext() {
        let key = fixture_key();
        let env = seal(&key, b"", b"aad bytes").unwrap();
        // AES-GCM emits a 16-byte tag for empty plaintext.
        assert_eq!(env.ciphertext.len(), aead::TAG_LEN);
        let pt = open(&key, &env).unwrap();
        assert!(pt.is_empty());
    }

    /// Empty AAD is also valid per AES-GCM. The Vault wire
    /// contract typically passes a non-empty AAD (email+mailbox
    /// IDs), but the primitive must work without it so that the
    /// CLI and tests can exercise the path without forging an
    /// AAD.
    #[test]
    fn roundtrip_empty_aad() {
        let key = fixture_key();
        let env = seal(&key, b"some plaintext", b"").unwrap();
        let pt = open(&key, &env).unwrap();
        assert_eq!(pt, b"some plaintext");
    }

    /// Larger payload: 1 MiB. The Vault flow doesn't currently
    /// chunk on the SDK side (chunking happens at the zk-object-
    /// fabric layer), so the primitive must accept multi-MiB
    /// inputs without truncating or panicking.
    #[test]
    fn roundtrip_one_mib() {
        let key = fixture_key();
        let pt = vec![0xABu8; 1024 * 1024];
        let env = seal(&key, &pt, b"aad").unwrap();
        let out = open(&key, &env).unwrap();
        assert_eq!(out, pt);
    }

    /// Tamper the ciphertext → AES-GCM tag MUST refuse to
    /// authenticate. This is the load-bearing zero-access
    /// property: the server cannot rewrite the payload without
    /// the client noticing.
    #[test]
    fn ciphertext_tamper_fails_authentication() {
        let key = fixture_key();
        let mut env = seal(&key, b"top secret", b"aad").unwrap();
        env.ciphertext[0] ^= 0x01;
        let err = open(&key, &env).unwrap_err();
        assert!(
            matches!(err, Error::Decryption(_)),
            "expected Decryption error, got {err:?}"
        );
    }

    /// Tamper the AAD → AES-GCM tag MUST refuse to authenticate.
    /// AAD is bound into the tag, so flipping a byte of it
    /// invalidates the message just like flipping a ciphertext
    /// byte.
    #[test]
    fn aad_tamper_fails_authentication() {
        let key = fixture_key();
        let mut env = seal(&key, b"top secret", b"original aad").unwrap();
        env.aad[0] ^= 0x01;
        let err = open(&key, &env).unwrap_err();
        assert!(
            matches!(err, Error::Decryption(_)),
            "expected Decryption error, got {err:?}"
        );
    }

    /// Tamper the nonce → because the nonce participates in the
    /// HKDF derivation of the DEK, flipping a byte produces a
    /// DEK whose AES-GCM authentication won't pass. Verifies
    /// the "nonce as salt" construction's tamper-evidence.
    #[test]
    fn nonce_tamper_fails_authentication() {
        let key = fixture_key();
        let mut env = seal(&key, b"top secret", b"aad").unwrap();
        env.nonce[0] ^= 0x01;
        let err = open(&key, &env).unwrap_err();
        assert!(
            matches!(err, Error::Decryption(_)),
            "expected Decryption error, got {err:?}"
        );
    }

    /// Wrong key → authentication failure. The DEK derived from a
    /// different master key is statistically unrelated to the
    /// one used for sealing.
    #[test]
    fn wrong_key_fails_authentication() {
        let key = fixture_key();
        let env = seal(&key, b"top secret", b"aad").unwrap();
        let mut wrong = key;
        wrong[0] ^= 0x01;
        let err = open(&wrong, &env).unwrap_err();
        assert!(
            matches!(err, Error::Decryption(_)),
            "expected Decryption error, got {err:?}"
        );
    }

    /// Short-key inputs MUST be rejected at the seal site with
    /// `Error::InvalidArgument`, not allowed to fall through to
    /// the AES-GCM layer's "invalid key length" error.
    #[test]
    fn short_key_is_rejected() {
        let key = [0u8; FOLDER_MASTER_KEY_LEN - 1];
        let err = seal(&key, b"pt", b"aad").unwrap_err();
        assert!(
            matches!(err, Error::InvalidArgument(_)),
            "expected InvalidArgument, got {err:?}"
        );
    }

    /// Same for `open` — short-key inputs MUST surface as
    /// `InvalidArgument`, not a downstream AES-GCM error.
    #[test]
    fn short_key_is_rejected_on_open() {
        let key = fixture_key();
        let env = seal(&key, b"pt", b"aad").unwrap();
        let short = [0u8; FOLDER_MASTER_KEY_LEN - 1];
        let err = open(&short, &env).unwrap_err();
        assert!(
            matches!(err, Error::InvalidArgument(_)),
            "expected InvalidArgument, got {err:?}"
        );
    }

    /// Load-bearing safety invariant: nonces MUST be unique
    /// across `seal` calls under the same key. We can't prove
    /// uniqueness for all future calls, but we CAN assert it for
    /// any finite sample — and since the nonce is sampled from
    /// `OsRng`, a collision in 4096 samples would indicate the
    /// underlying RNG is broken or the seal function is reusing
    /// a fixed nonce by accident.
    ///
    /// 4096 samples is large enough to flag a constant-nonce bug
    /// instantly (collision count would be 4095) but small enough
    /// to keep the test under 1 second on CI.
    #[test]
    fn seal_produces_unique_nonces() {
        let key = fixture_key();
        let mut seen = HashSet::with_capacity(4096);
        for _ in 0..4096 {
            let env = seal(&key, b"pt", b"aad").unwrap();
            assert!(
                seen.insert(env.nonce),
                "duplicate nonce produced by seal: {:?}",
                env.nonce
            );
        }
    }

    /// Sealing the SAME plaintext + AAD under the SAME key twice
    /// MUST produce two different ciphertexts (because the nonce
    /// — and hence the DEK — differs). This is the IND-CPA
    /// property; without it, an observer can tell that two
    /// envelopes carry the same plaintext just by byte-comparing
    /// the ciphertexts.
    #[test]
    fn seal_is_non_deterministic() {
        let key = fixture_key();
        let a = seal(&key, b"identical plaintext", b"identical aad").unwrap();
        let b = seal(&key, b"identical plaintext", b"identical aad").unwrap();
        assert_ne!(
            a.ciphertext, b.ciphertext,
            "two seals of the same plaintext produced identical ciphertexts \
             — nonce is not being randomised per call"
        );
        // Open both — both must round-trip.
        assert_eq!(open(&key, &a).unwrap(), b"identical plaintext");
        assert_eq!(open(&key, &b).unwrap(), b"identical plaintext");
    }
}
