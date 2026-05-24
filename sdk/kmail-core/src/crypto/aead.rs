// AES-256-GCM AEAD wrapper.
//
// Why this exists rather than re-exporting `aes-gcm` types: the
// rest of the SDK (and the FFI / napi wrappers) want a single
// "envelope" shape — `(ciphertext, nonce, tag, aad)` — to
// serialise across the FFI boundary. Doing the framing in one
// place keeps the wire format in lock-step with the BFF's
// understanding of a Confidential-Send / Vault payload.

use crate::error::{Error, Result};
use aes_gcm::{
    aead::{Aead, KeyInit, Payload},
    Aes256Gcm, Nonce,
};

/// AES-GCM standard nonce length (RFC 5116 §5.1 — 96 bits).
pub const NONCE_LEN: usize = 12;
/// AES-GCM authentication tag length (RFC 5116 §5.1 — 128 bits).
pub const TAG_LEN: usize = 16;
/// AES-256 key length.
pub const KEY_LEN: usize = 32;

/// On-the-wire envelope. `ciphertext` already includes the trailing
/// 16-byte GCM tag — matches the `aes-gcm` crate's `encrypt` output
/// shape and the format zk-object-fabric uses for `StrictZK`
/// payloads (see zk-object-fabric `internal/envelope` package).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AeadEnvelope {
    pub nonce: [u8; NONCE_LEN],
    /// `ciphertext || tag` concatenation. Length == plaintext_len + 16.
    pub ciphertext: Vec<u8>,
    /// Additional Authenticated Data. Bound into the tag; mutating
    /// it after the fact invalidates the message.
    pub aad: Vec<u8>,
}

/// Encrypt `plaintext` under a 32-byte AES-256 key. `aad` is
/// bound into the GCM tag but not encrypted. Caller is
/// responsible for passing a unique 12-byte nonce per key.
///
/// Returns an `AeadEnvelope` containing the supplied nonce + the
/// `aes-gcm` ciphertext blob (which embeds the tag).
pub fn aes_gcm_encrypt(
    key: &[u8],
    nonce: &[u8; NONCE_LEN],
    plaintext: &[u8],
    aad: &[u8],
) -> Result<AeadEnvelope> {
    if key.len() != KEY_LEN {
        return Err(Error::InvalidArgument(format!(
            "aes-256-gcm requires a {KEY_LEN}-byte key, got {}",
            key.len()
        )));
    }
    let cipher = Aes256Gcm::new_from_slice(key)
        .map_err(|e| Error::InvalidArgument(format!("invalid key: {e}")))?;
    let nonce_obj = Nonce::from_slice(nonce);
    let ct = cipher
        .encrypt(
            nonce_obj,
            Payload {
                msg: plaintext,
                aad,
            },
        )
        .map_err(|_| Error::Decryption("encrypt failed".into()))?;
    Ok(AeadEnvelope {
        nonce: *nonce,
        ciphertext: ct,
        aad: aad.to_vec(),
    })
}

/// Decrypt the envelope. Returns an error if the tag does not
/// authenticate — distinct from a plaintext error so callers can
/// surface tamper-detected vs. wrong-key as the same UX.
pub fn aes_gcm_decrypt(key: &[u8], envelope: &AeadEnvelope) -> Result<Vec<u8>> {
    if key.len() != KEY_LEN {
        return Err(Error::InvalidArgument(format!(
            "aes-256-gcm requires a {KEY_LEN}-byte key, got {}",
            key.len()
        )));
    }
    if envelope.ciphertext.len() < TAG_LEN {
        return Err(Error::Decryption("ciphertext shorter than GCM tag".into()));
    }
    let cipher = Aes256Gcm::new_from_slice(key)
        .map_err(|e| Error::InvalidArgument(format!("invalid key: {e}")))?;
    let nonce_obj = Nonce::from_slice(&envelope.nonce);
    cipher
        .decrypt(
            nonce_obj,
            Payload {
                msg: &envelope.ciphertext,
                aad: &envelope.aad,
            },
        )
        .map_err(|_| Error::Decryption("authentication failed".into()))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// NIST CAVS GCM Test Vector — Count=0 from gcmEncryptExtIV256.rsp:
    ///   Key       = b52c505a37d78eda5dd34f20c22540ea1b58963cf8e5bf8ffa85f9f2492505b4
    ///   IV        = 516c33929df5a3284ff463d7
    ///   PT        = ""
    ///   AAD       = ""
    ///   CT        = ""
    ///   Tag       = bdc1ac884d332457a1d2664f168c76f0
    ///
    /// Encrypting an empty plaintext under this key + nonce must
    /// reproduce the published tag exactly; otherwise the
    /// underlying AES-GCM implementation is broken.
    #[test]
    fn nist_gcm_kat_empty_plaintext_empty_aad() {
        let key = hex::decode("b52c505a37d78eda5dd34f20c22540ea1b58963cf8e5bf8ffa85f9f2492505b4")
            .unwrap();
        let nonce_bytes = hex::decode("516c33929df5a3284ff463d7").unwrap();
        let mut nonce = [0u8; NONCE_LEN];
        nonce.copy_from_slice(&nonce_bytes);

        let env = aes_gcm_encrypt(&key, &nonce, b"", b"").unwrap();
        assert_eq!(env.ciphertext.len(), TAG_LEN, "tag-only output");
        assert_eq!(
            hex::encode(&env.ciphertext),
            "bdc1ac884d332457a1d2664f168c76f0"
        );
        let pt = aes_gcm_decrypt(&key, &env).unwrap();
        assert!(pt.is_empty());
    }

    /// McGrew & Viega "The Galois/Counter Mode of Operation (GCM)"
    /// Test Case 13 — AES-256 key, all-zero IV, single-block zero
    /// plaintext, no AAD. Published values:
    ///
    ///   K   = 0000...0000  (256-bit zero)
    ///   IV  = 0000...0000  (96-bit zero)
    ///   P   = 00...00      (128-bit zero)
    ///   A   = ""
    ///   C   = cea7403d4d606b6e074ec5d3baf39d18
    ///   T   = d0d1c8a799996bf0265b98b5d48ab919
    ///
    /// This KAT is widely cited (NIST SP 800-38D, the original
    /// McGrew/Viega paper, and the RustCrypto aes-gcm test suite)
    /// so any divergence here means the AEAD primitive is broken.
    #[test]
    fn nist_gcm_kat_test_case_13() {
        let key = [0u8; KEY_LEN];
        let nonce = [0u8; NONCE_LEN];
        let pt = [0u8; 16];

        let env = aes_gcm_encrypt(&key, &nonce, &pt, b"").unwrap();
        assert_eq!(
            hex::encode(&env.ciphertext),
            // ct || tag concatenation (RustCrypto's encode shape).
            "cea7403d4d606b6e074ec5d3baf39d18d0d1c8a799996bf0265b98b5d48ab919",
            "AES-256-GCM diverged from McGrew & Viega Test Case 13"
        );

        let decrypted = aes_gcm_decrypt(&key, &env).unwrap();
        assert_eq!(decrypted, pt);
    }

    /// Tampering with the AAD must surface as a decryption error.
    /// This is the property Zero-Access Vault relies on: the BFF
    /// can't substitute AAD without breaking the tag.
    #[test]
    fn tampered_aad_fails_authentication() {
        let key = [0x11u8; KEY_LEN];
        let nonce = [0x22u8; NONCE_LEN];
        let mut env = aes_gcm_encrypt(&key, &nonce, b"secret", b"bound-context").unwrap();
        env.aad = b"different-context".to_vec();
        assert!(matches!(
            aes_gcm_decrypt(&key, &env),
            Err(Error::Decryption(_))
        ));
    }

    /// Flipping a single ciphertext bit must surface as a
    /// decryption error.
    #[test]
    fn tampered_ciphertext_fails_authentication() {
        let key = [0x11u8; KEY_LEN];
        let nonce = [0x22u8; NONCE_LEN];
        let mut env = aes_gcm_encrypt(&key, &nonce, b"secret", b"").unwrap();
        env.ciphertext[0] ^= 0x01;
        assert!(matches!(
            aes_gcm_decrypt(&key, &env),
            Err(Error::Decryption(_))
        ));
    }

    /// Wrong key length must be rejected before the AEAD layer.
    #[test]
    fn wrong_key_length_is_invalid_argument() {
        let res = aes_gcm_encrypt(&[0u8; 31], &[0u8; NONCE_LEN], b"", b"");
        assert!(matches!(res, Err(Error::InvalidArgument(_))));
    }
}
