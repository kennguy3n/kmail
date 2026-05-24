// HKDF-SHA256 per RFC 5869.
//
// Used for two flows:
//
//  1. Confidential-Send DEK wrapping key derivation. The MLS leaf
//     exporter hands the SDK a 32-byte secret; the SDK runs HKDF
//     with a domain-tagged label to produce the AES key that
//     unwraps the per-message DEK.
//
//  2. Zero-Access Vault folder master key derivation. The MLS
//     credential exporter hands the SDK a 32-byte secret; HKDF
//     with a different label derives the folder master key.
//
// The same primitive supports both — the salt + info label is what
// keeps the two key hierarchies separate. Both labels are defined
// in this module so the BFF, the React client (which never sees
// these labels — they're client-side only), and the native shells
// all agree on the wire format.

use crate::error::{Error, Result};
use hkdf::Hkdf;
use sha2::Sha256;

/// Domain labels used as the `info` argument to HKDF-Expand. The
/// strings are part of the public API surface — changing them
/// rotates every derived key for every user, so they are versioned
/// (`v1`) and must NEVER be changed in-place. New flows pick a new
/// label string.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum KdfLabel {
    /// Confidential-Send DEK wrapping key (derived from MLS leaf).
    ConfidentialSendDekWrap,
    /// Zero-Access Vault folder master key (derived from MLS credential).
    VaultFolderMaster,
    /// Generic per-call label. Callers supply their own bytes —
    /// useful for tests and for future flows that don't have a
    /// dedicated variant yet.
    Custom(&'static [u8]),
}

impl KdfLabel {
    pub fn as_bytes(&self) -> &[u8] {
        match self {
            KdfLabel::ConfidentialSendDekWrap => b"kmail/v1/confidential-send/dek-wrap-key",
            KdfLabel::VaultFolderMaster => b"kmail/v1/vault/folder-master-key",
            KdfLabel::Custom(b) => b,
        }
    }
}

/// HKDF-Extract step from RFC 5869 §2.2.
///
/// Returns the PRK (pseudorandom key) — 32 bytes when using
/// SHA-256. `salt` may be empty (RFC 5869 §2.2 paragraph 3).
pub fn hkdf_extract(salt: &[u8], ikm: &[u8]) -> [u8; 32] {
    let (prk, _) = Hkdf::<Sha256>::extract(Some(salt), ikm);
    let mut out = [0u8; 32];
    out.copy_from_slice(&prk[..32]);
    out
}

/// HKDF-Expand step from RFC 5869 §2.3.
///
/// `len` must be ≤ 255 * HashLen (= 8160 bytes for SHA-256), per
/// RFC 5869 §2.3.
pub fn hkdf_expand_only(prk: &[u8], info: &[u8], len: usize) -> Result<Vec<u8>> {
    let hk = Hkdf::<Sha256>::from_prk(prk)
        .map_err(|e| Error::KeyDerivation(format!("invalid prk length: {e}")))?;
    let mut out = vec![0u8; len];
    hk.expand(info, &mut out)
        .map_err(|e| Error::KeyDerivation(format!("expand failed: {e}")))?;
    Ok(out)
}

/// Combined HKDF-Extract-then-Expand, with a domain-tagged label.
///
/// The recommended entry point for callers — produces a `len`-byte
/// derived key bound to the (`salt`, `ikm`, `label`) tuple.
pub fn hkdf_derive(salt: &[u8], ikm: &[u8], label: KdfLabel, len: usize) -> Result<Vec<u8>> {
    let hk = Hkdf::<Sha256>::new(Some(salt), ikm);
    let mut out = vec![0u8; len];
    hk.expand(label.as_bytes(), &mut out)
        .map_err(|e| Error::KeyDerivation(format!("expand failed: {e}")))?;
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// RFC 5869 Appendix A.1 — Test Case 1 (SHA-256, basic case).
    ///
    ///   IKM  = 0x0b * 22
    ///   salt = 0x000102030405060708090a0b0c
    ///   info = 0xf0f1f2f3f4f5f6f7f8f9
    ///   L    = 42
    ///   PRK  = 077709362c2e32df0ddc3f0dc47bba63
    ///          90b6c73bb50f9c3122ec844ad7c2b3e5
    ///   OKM  = 3cb25f25faacd57a90434f64d0362f2a
    ///          2d2d0a90cf1a5a4c5db02d56ecc4c5bf
    ///          34007208d5b887185865
    #[test]
    fn rfc5869_test_case_1() {
        let ikm = hex::decode("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b").unwrap();
        let salt = hex::decode("000102030405060708090a0b0c").unwrap();
        let info = hex::decode("f0f1f2f3f4f5f6f7f8f9").unwrap();

        let prk = hkdf_extract(&salt, &ikm);
        assert_eq!(
            hex::encode(prk),
            "077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5"
        );

        let okm = hkdf_expand_only(&prk, &info, 42).unwrap();
        assert_eq!(
            hex::encode(okm),
            "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865"
        );
    }

    /// RFC 5869 Appendix A.2 — Test Case 2 (SHA-256, longer
    /// inputs/outputs).
    ///
    /// Validates HKDF behaviour across multiple T(i) blocks
    /// (L=82 > HashLen=32, so the expand step iterates).
    #[test]
    fn rfc5869_test_case_2() {
        let ikm = hex::decode(
            "000102030405060708090a0b0c0d0e0f\
             101112131415161718191a1b1c1d1e1f\
             202122232425262728292a2b2c2d2e2f\
             303132333435363738393a3b3c3d3e3f\
             404142434445464748494a4b4c4d4e4f",
        )
        .unwrap();
        let salt = hex::decode(
            "606162636465666768696a6b6c6d6e6f\
             707172737475767778797a7b7c7d7e7f\
             808182838485868788898a8b8c8d8e8f\
             909192939495969798999a9b9c9d9e9f\
             a0a1a2a3a4a5a6a7a8a9aaabacadaeaf",
        )
        .unwrap();
        let info = hex::decode(
            "b0b1b2b3b4b5b6b7b8b9babbbcbdbebf\
             c0c1c2c3c4c5c6c7c8c9cacbcccdcecf\
             d0d1d2d3d4d5d6d7d8d9dadbdcdddedf\
             e0e1e2e3e4e5e6e7e8e9eaebecedeeef\
             f0f1f2f3f4f5f6f7f8f9fafbfcfdfeff",
        )
        .unwrap();

        let prk = hkdf_extract(&salt, &ikm);
        assert_eq!(
            hex::encode(prk),
            "06a6b88c5853361a06104c9ceb35b45cef760014904671014a193f40c15fc244"
        );

        let okm = hkdf_expand_only(&prk, &info, 82).unwrap();
        assert_eq!(
            hex::encode(okm),
            "b11e398dc80327a1c8e7f78c596a4934\
             4f012eda2d4efad8a050cc4c19afa97c\
             59045a99cac7827271cb41c65e590e09\
             da3275600c2f09b8367793a9aca3db71\
             cc30c58179ec3e87c14c01d5c1f3434f\
             1d87"
        );
    }

    /// RFC 5869 Appendix A.3 — Test Case 3 (SHA-256, zero-length
    /// salt and info).
    ///
    /// HKDF-Extract with salt="" must replace it with a HashLen-byte
    /// zero string per RFC 5869 §2.2.
    #[test]
    fn rfc5869_test_case_3() {
        let ikm = hex::decode("0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b").unwrap();
        let salt = b"";
        let info = b"";

        let prk = hkdf_extract(salt, &ikm);
        assert_eq!(
            hex::encode(prk),
            "19ef24a32c717b167f33a91d6f648bdf96596776afdb6377ac434c1c293ccb04"
        );

        let okm = hkdf_expand_only(&prk, info, 42).unwrap();
        assert_eq!(
            hex::encode(okm),
            "8da4e775a563c18f715f802a063c5a31\
             b8a11f5c5ee1879ec3454e5f3c738d2d\
             9d201395faa4b61a96c8"
        );
    }

    /// The high-level `hkdf_derive` must equal an Extract+Expand
    /// composition with the same inputs. Guards against regressions
    /// where the convenience wrapper accidentally diverges (e.g.
    /// reordering inputs or mixing in extra context bytes).
    #[test]
    fn hkdf_derive_matches_extract_then_expand() {
        let salt = b"some-salt";
        let ikm = b"some-ikm";
        let info = b"custom-label";

        let one_shot = hkdf_derive(salt, ikm, KdfLabel::Custom(info), 64).unwrap();
        let prk = hkdf_extract(salt, ikm);
        let two_step = hkdf_expand_only(&prk, info, 64).unwrap();

        assert_eq!(one_shot, two_step);
    }

    /// Domain labels must differ in output even when IKM + salt
    /// are identical. This is the property the SDK relies on to
    /// keep Confidential-Send and Vault key hierarchies separate.
    #[test]
    fn distinct_labels_produce_distinct_keys() {
        let ikm = [0x99u8; 32];
        let salt = b"";
        let confidential = hkdf_derive(salt, &ikm, KdfLabel::ConfidentialSendDekWrap, 32).unwrap();
        let vault = hkdf_derive(salt, &ikm, KdfLabel::VaultFolderMaster, 32).unwrap();
        assert_ne!(confidential, vault);
    }

    /// Requesting more than 255 * HashLen bytes is invalid per RFC
    /// 5869 §2.3.
    #[test]
    fn oversized_expand_is_rejected() {
        let prk = [0u8; 32];
        let res = hkdf_expand_only(&prk, b"info", 255 * 32 + 1);
        assert!(matches!(res, Err(Error::KeyDerivation(_))));
    }
}
