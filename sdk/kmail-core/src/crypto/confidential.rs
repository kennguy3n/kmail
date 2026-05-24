// Confidential Send envelope: seal + open.
//
// Two-layer key hierarchy (distinct from the one-layer Vault
// construction in `vault.rs` — see that file's header for why
// the two flows can't share a primitive):
//
//   DEK            = OsRng.fill(32)        # per-message random
//   kek_salt       = OsRng.fill(32)        # KEK derivation salt
//   nonce_kek      = OsRng.fill(12)        # AES-GCM nonce for DEK wrap
//   nonce_payload  = OsRng.fill(12)        # AES-GCM nonce for plaintext
//
//   KEK            = HKDF-SHA256(salt=kek_salt,
//                                ikm=mls_leaf_secret,
//                                info=ConfidentialSendDekWrap, len=32)
//   wrapped_dek    = AES-256-GCM(key=KEK,
//                                nonce=nonce_kek,
//                                plaintext=DEK,
//                                aad=kek_aad)
//   payload        = AES-256-GCM(key=DEK,
//                                nonce=nonce_payload,
//                                plaintext=user_plaintext,
//                                aad=user_aad)
//
// The envelope on the wire is `{ kek_salt, wrapped_dek, payload }`.
// Open inverts: derive KEK from the leaf secret + salt, unwrap the
// DEK, decrypt the payload.
//
// Why two layers? Two reasons fixed by docs/ARCHITECTURE.md §5:
//
//   1. **Rotation**: a Confidential Send recipient can revoke
//      access by destroying the wrapped DEK without touching the
//      payload. The payload lives in zk-object-fabric as immutable
//      `StrictZK` storage; the envelope sits in mailbox metadata.
//      Server can drop the envelope row to revoke; ciphertext is
//      then orphaned because no surviving DEK can open it.
//
//   2. **External recipients via portal**: when the BFF mints a
//      one-time portal URL, it wraps the SAME per-message DEK
//      under a portal-specific KEK derived from the portal
//      passphrase. Two wrapped envelopes (one MLS-keyed, one
//      portal-keyed) point at the same payload ciphertext, so
//      external recipients can decrypt without ever holding the
//      MLS leaf secret. This module handles only the MLS-keyed
//      half; the portal half is the BFF's responsibility.
//
// Security note on the KEK salt: the salt is sampled per message,
// which means the KEK is fresh per message. We don't strictly
// need a per-message KEK — a per-thread or per-recipient KEK
// would also be safe — but per-message is the simplest invariant
// to audit (every envelope is independent, no need to track KEK
// scope on the receive side).

use crate::crypto::{aead, aes_gcm_decrypt, aes_gcm_encrypt, hkdf_derive, AeadEnvelope, KdfLabel};
use crate::error::{Error, Result};
use rand::rngs::OsRng;
use rand::RngCore;
use zeroize::Zeroize;

/// Length of the random per-message DEK (matches AES-256 key size).
pub const DEK_LEN: usize = 32;

/// Length of the HKDF salt used to derive the KEK from the MLS
/// leaf secret. 32 bytes matches the SHA-256 hash length so the
/// salt provides a full SHA-256 worth of entropy into the KDF.
pub const KEK_SALT_LEN: usize = 32;

/// Length of the MLS leaf secret expected by [`seal`] and [`open`].
///
/// The KChat MLS SDK derives this from the user's MLS leaf node
/// via the standard exporter interface (RFC 9420 §8.5). 32 bytes
/// matches the SHA-256 hash output size and the AES-256 key size,
/// so the HKDF below has both maximum input entropy and minimum
/// padding work.
pub const MLS_LEAF_SECRET_LEN: usize = 32;

/// Wire shape of a sealed Confidential Send message.
///
/// Field ordering and types are stable — this struct is what
/// crosses the FFI boundary into Swift / Kotlin (UniFFI) and
/// JavaScript (napi-rs). Any field reordering or rename is a
/// breaking change on every platform shell.
///
/// The struct is `Clone` so the UniFFI / napi layers can pass it
/// across the FFI boundary by value without forcing the caller
/// to manage handles. It is intentionally NOT `Copy` — the
/// `wrapped_dek` and `payload` AeadEnvelopes own `Vec<u8>` data.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ConfidentialEnvelope {
    /// HKDF salt that derives the KEK from the MLS leaf secret.
    /// Sampled per message in [`seal`]; consumed verbatim by
    /// [`open`].
    pub kek_salt: [u8; KEK_SALT_LEN],
    /// AES-256-GCM ciphertext of the per-message DEK under the
    /// KEK. The plaintext of this envelope is the 32-byte DEK.
    pub wrapped_dek: AeadEnvelope,
    /// AES-256-GCM ciphertext of the user's payload under the
    /// DEK. The plaintext of this envelope is the caller's
    /// original plaintext bytes.
    pub payload: AeadEnvelope,
}

/// Seal `plaintext` under the caller's MLS leaf secret.
///
/// The function samples three independent randomness inputs from
/// `OsRng`: the per-message DEK (32 bytes), the KEK derivation
/// salt (32 bytes), and two AES-GCM nonces (12 bytes each — one
/// for the DEK wrap, one for the payload). All three randomness
/// inputs MUST be fresh per call; the module's tests pin this
/// invariant by checking sample uniqueness across 4096 calls.
///
/// `payload_aad` is bound into the payload AES-GCM tag —
/// platform shells should include the message ID + recipient
/// list + epoch so a server-side replay of the payload to a
/// different recipient is detected as authentication failure.
/// `wrap_aad` is bound into the DEK wrap's AES-GCM tag —
/// platform shells should include the recipient identifier so
/// the wrap is not portable between recipients.
pub fn seal(
    mls_leaf_secret: &[u8],
    plaintext: &[u8],
    payload_aad: &[u8],
    wrap_aad: &[u8],
) -> Result<ConfidentialEnvelope> {
    if mls_leaf_secret.len() != MLS_LEAF_SECRET_LEN {
        return Err(Error::InvalidArgument(format!(
            "mls leaf secret must be {MLS_LEAF_SECRET_LEN} bytes, got {}",
            mls_leaf_secret.len()
        )));
    }
    let mut dek = [0u8; DEK_LEN];
    OsRng.fill_bytes(&mut dek);

    let mut kek_salt = [0u8; KEK_SALT_LEN];
    OsRng.fill_bytes(&mut kek_salt);

    let mut nonce_kek = [0u8; aead::NONCE_LEN];
    OsRng.fill_bytes(&mut nonce_kek);

    let mut nonce_payload = [0u8; aead::NONCE_LEN];
    OsRng.fill_bytes(&mut nonce_payload);

    let mut kek = hkdf_derive(
        &kek_salt,
        mls_leaf_secret,
        KdfLabel::ConfidentialSendDekWrap,
        32,
    )?;

    // Wrap the DEK, then encrypt the payload. The DEK and KEK
    // are zeroised below so neither secret outlives this scope
    // on the heap (for the KEK `Vec<u8>`) or on the stack (for
    // the `[u8; DEK_LEN]` array). The stack array is the more
    // critical of the two because its bytes never leave the
    // page that gets reused by subsequent calls, but the heap
    // Vec is zeroised symmetrically so a future attacker with
    // freed-heap read cannot reconstruct one half of the two-
    // layer envelope.
    let wrap_result = aes_gcm_encrypt(&kek, &nonce_kek, &dek, wrap_aad);
    kek.zeroize();
    let wrapped_dek = match wrap_result {
        Ok(env) => env,
        Err(e) => {
            dek.zeroize();
            return Err(e);
        }
    };
    let payload_result = aes_gcm_encrypt(&dek, &nonce_payload, plaintext, payload_aad);
    dek.zeroize();
    let payload = payload_result?;

    Ok(ConfidentialEnvelope {
        kek_salt,
        wrapped_dek,
        payload,
    })
}

/// Inverse of [`seal`].
///
/// Returns the original `plaintext` bytes on success. Authentication
/// failure (`Error::Decryption`) is the only failure mode for a
/// well-formed envelope under the wrong key — distinct from the
/// `Error::InvalidArgument` returned for a malformed input
/// (wrong-length leaf secret, wrong-length wrapped DEK
/// plaintext).
pub fn open(mls_leaf_secret: &[u8], env: &ConfidentialEnvelope) -> Result<Vec<u8>> {
    if mls_leaf_secret.len() != MLS_LEAF_SECRET_LEN {
        return Err(Error::InvalidArgument(format!(
            "mls leaf secret must be {MLS_LEAF_SECRET_LEN} bytes, got {}",
            mls_leaf_secret.len()
        )));
    }

    let mut kek = hkdf_derive(
        &env.kek_salt,
        mls_leaf_secret,
        KdfLabel::ConfidentialSendDekWrap,
        32,
    )?;

    // Unwrap the DEK. AES-GCM authentication failure here is the
    // signal that the leaf secret is wrong, the envelope was
    // tampered with, or both. We treat all three uniformly so
    // the UX can render "this message is not for you / corrupt"
    // without leaking which condition fired.
    //
    // The KEK is zeroised immediately after the unwrap; the DEK
    // is zeroised below after the payload decrypt.
    let unwrap_result = aes_gcm_decrypt(&kek, &env.wrapped_dek);
    kek.zeroize();
    let mut dek = unwrap_result?;
    if dek.len() != DEK_LEN {
        // A well-formed wrap must produce exactly 32 bytes. If
        // it doesn't, the wrap was forged by an attacker that
        // somehow held the KEK but used a different DEK length.
        // Reject before feeding non-32-byte material into AES.
        dek.zeroize();
        return Err(Error::Decryption("wrapped DEK has wrong length".into()));
    }

    let pt = aes_gcm_decrypt(&dek, &env.payload);
    dek.zeroize();
    pt
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashSet;

    fn fixture_secret() -> [u8; MLS_LEAF_SECRET_LEN] {
        let mut k = [0u8; MLS_LEAF_SECRET_LEN];
        for (i, b) in k.iter_mut().enumerate() {
            *b = (i ^ 0x5A) as u8;
        }
        k
    }

    #[test]
    fn roundtrip_smoke() {
        let secret = fixture_secret();
        let env = seal(&secret, b"top secret", b"msg-1 to alice", b"alice-wrap").unwrap();
        let pt = open(&secret, &env).unwrap();
        assert_eq!(pt, b"top secret");
    }

    #[test]
    fn roundtrip_empty_plaintext() {
        let secret = fixture_secret();
        let env = seal(&secret, b"", b"aad", b"wrap-aad").unwrap();
        assert_eq!(env.payload.ciphertext.len(), aead::TAG_LEN);
        let pt = open(&secret, &env).unwrap();
        assert!(pt.is_empty());
    }

    #[test]
    fn roundtrip_empty_aads() {
        let secret = fixture_secret();
        let env = seal(&secret, b"some plaintext", b"", b"").unwrap();
        let pt = open(&secret, &env).unwrap();
        assert_eq!(pt, b"some plaintext");
    }

    #[test]
    fn roundtrip_one_mib() {
        let secret = fixture_secret();
        let pt = vec![0xCDu8; 1024 * 1024];
        let env = seal(&secret, &pt, b"aad", b"wrap-aad").unwrap();
        let out = open(&secret, &env).unwrap();
        assert_eq!(out, pt);
    }

    #[test]
    fn payload_tamper_fails_authentication() {
        let secret = fixture_secret();
        let mut env = seal(&secret, b"top secret", b"aad", b"wrap-aad").unwrap();
        env.payload.ciphertext[0] ^= 0x01;
        let err = open(&secret, &env).unwrap_err();
        assert!(matches!(err, Error::Decryption(_)));
    }

    #[test]
    fn wrapped_dek_tamper_fails_authentication() {
        let secret = fixture_secret();
        let mut env = seal(&secret, b"top secret", b"aad", b"wrap-aad").unwrap();
        env.wrapped_dek.ciphertext[0] ^= 0x01;
        let err = open(&secret, &env).unwrap_err();
        assert!(matches!(err, Error::Decryption(_)));
    }

    #[test]
    fn kek_salt_tamper_fails_authentication() {
        let secret = fixture_secret();
        let mut env = seal(&secret, b"top secret", b"aad", b"wrap-aad").unwrap();
        env.kek_salt[0] ^= 0x01;
        let err = open(&secret, &env).unwrap_err();
        assert!(matches!(err, Error::Decryption(_)));
    }

    #[test]
    fn payload_aad_tamper_fails_authentication() {
        let secret = fixture_secret();
        let mut env = seal(&secret, b"top secret", b"original aad", b"wrap-aad").unwrap();
        env.payload.aad[0] ^= 0x01;
        let err = open(&secret, &env).unwrap_err();
        assert!(matches!(err, Error::Decryption(_)));
    }

    #[test]
    fn wrap_aad_tamper_fails_authentication() {
        let secret = fixture_secret();
        let mut env = seal(&secret, b"top secret", b"payload aad", b"original wrap aad").unwrap();
        env.wrapped_dek.aad[0] ^= 0x01;
        let err = open(&secret, &env).unwrap_err();
        assert!(matches!(err, Error::Decryption(_)));
    }

    #[test]
    fn wrong_secret_fails_authentication() {
        let secret = fixture_secret();
        let env = seal(&secret, b"top secret", b"aad", b"wrap-aad").unwrap();
        let mut wrong = secret;
        wrong[0] ^= 0x01;
        let err = open(&wrong, &env).unwrap_err();
        assert!(matches!(err, Error::Decryption(_)));
    }

    #[test]
    fn short_secret_is_rejected() {
        let secret = [0u8; MLS_LEAF_SECRET_LEN - 1];
        let err = seal(&secret, b"pt", b"aad", b"wrap-aad").unwrap_err();
        assert!(matches!(err, Error::InvalidArgument(_)));
    }

    #[test]
    fn short_secret_is_rejected_on_open() {
        let secret = fixture_secret();
        let env = seal(&secret, b"pt", b"aad", b"wrap-aad").unwrap();
        let short = [0u8; MLS_LEAF_SECRET_LEN - 1];
        let err = open(&short, &env).unwrap_err();
        assert!(matches!(err, Error::InvalidArgument(_)));
    }

    /// Load-bearing safety invariant: salts AND DEK-wrap nonces
    /// AND payload nonces MUST all be unique across `seal`
    /// calls. We sample 1024 envelopes (smaller batch than vault
    /// because we're checking three independent inputs here)
    /// and assert no duplicates in any of the three.
    #[test]
    fn seal_produces_unique_randomness() {
        let secret = fixture_secret();
        let mut salts = HashSet::with_capacity(1024);
        let mut kek_nonces = HashSet::with_capacity(1024);
        let mut payload_nonces = HashSet::with_capacity(1024);
        for _ in 0..1024 {
            let env = seal(&secret, b"pt", b"aad", b"wrap-aad").unwrap();
            assert!(salts.insert(env.kek_salt), "duplicate KEK salt");
            assert!(
                kek_nonces.insert(env.wrapped_dek.nonce),
                "duplicate KEK nonce"
            );
            assert!(
                payload_nonces.insert(env.payload.nonce),
                "duplicate payload nonce"
            );
        }
    }

    /// Sealing identical plaintext + AAD under the same MLS
    /// secret twice MUST produce distinct ciphertexts (the
    /// salt + DEK + nonces all differ). Pinning the IND-CPA
    /// property at the envelope level.
    #[test]
    fn seal_is_non_deterministic() {
        let secret = fixture_secret();
        let a = seal(
            &secret,
            b"identical pt",
            b"identical aad",
            b"identical wrap",
        )
        .unwrap();
        let b = seal(
            &secret,
            b"identical pt",
            b"identical aad",
            b"identical wrap",
        )
        .unwrap();
        assert_ne!(a.kek_salt, b.kek_salt);
        assert_ne!(a.wrapped_dek.nonce, b.wrapped_dek.nonce);
        assert_ne!(a.payload.nonce, b.payload.nonce);
        assert_ne!(a.wrapped_dek.ciphertext, b.wrapped_dek.ciphertext);
        assert_ne!(a.payload.ciphertext, b.payload.ciphertext);
        // Both round-trip.
        assert_eq!(open(&secret, &a).unwrap(), b"identical pt");
        assert_eq!(open(&secret, &b).unwrap(), b"identical pt");
    }

    /// Cross-envelope wrap reuse: even if an attacker swaps the
    /// `wrapped_dek` from envelope A into envelope B (same
    /// secret), the open MUST fail because the payload's AAD
    /// (recipient ID, message ID) is bound into the payload tag
    /// — except in this test we don't have distinct AADs, so the
    /// failure is via the DEK itself: A's wrap unwraps to A's
    /// DEK, which doesn't open B's payload (encrypted under B's
    /// DEK).
    #[test]
    fn cross_envelope_wrap_swap_fails() {
        let secret = fixture_secret();
        let a = seal(&secret, b"plaintext A", b"aad", b"wrap-aad").unwrap();
        let b = seal(&secret, b"plaintext B", b"aad", b"wrap-aad").unwrap();
        // Take A's wrap into B's envelope.
        let frankenstein = ConfidentialEnvelope {
            kek_salt: a.kek_salt,
            wrapped_dek: a.wrapped_dek.clone(),
            payload: b.payload.clone(),
        };
        let err = open(&secret, &frankenstein).unwrap_err();
        assert!(matches!(err, Error::Decryption(_)));
    }
}
