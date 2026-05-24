// Convenience adapters for plugging an MLS exporter-secret
// provider into the KMail SDK from Kotlin.
//
// The uniffi-generated `FfiMlsKeyProvider` is a class-shaped
// foreign trait: callers implement it by extending the interface
// and supplying `confidentialSendLeafSecret` and
// `vaultFolderMasterSecret`. This is fine for production shells
// that wrap a long-lived MLS SDK instance, but tests / quick
// prototypes want lambdas.
//
// `LambdaMlsKeyProvider` lets you supply two function references
// and get a fully-conformant `FfiMlsKeyProvider`. It's the same
// pattern as the Swift `ClosureMlsKeyProvider` at
// `apps/ios/Sources/KMail/MlsKeyProvider.swift`.

package com.kmail.sdk

import uniffi.kmail_ffi.FfiMlsKeyProvider

/**
 * Lambda-driven implementation of [MlsKeyProvider].
 *
 * `confidentialSend` and `vaultFolder` MUST return exactly 32
 * bytes for each input. Returning any other length will surface
 * to the SDK as `KMailException.KeyStore(...)` (the FFI adapter
 * validates the contract — see `sdk/kmail-ffi/src/lib.rs`
 * `ForeignMlsKeyProvider`).
 *
 * **Determinism**: both lambdas MUST return the same bytes for
 * the same input across the lifetime of a single MLS epoch. The
 * platform shell should rotate its in-memory cache only when the
 * MLS epoch rotates (typically driven by an `MlsKeyProvider`
 * owned by the KChat MLS SDK).
 *
 * **Threading**: both lambdas may be invoked concurrently from
 * multiple Tokio worker threads inside the SDK. Implementations
 * must be thread-safe — UniFFI's `with_foreign` callbacks are
 * dispatched on whichever runtime thread happens to be free.
 */
public class LambdaMlsKeyProvider(
    private val confidentialSend: (String) -> ByteArray,
    private val vaultFolder: (String) -> ByteArray,
) : FfiMlsKeyProvider {

    override fun confidentialSendLeafSecret(recipientUserId: String): ByteArray =
        confidentialSend(recipientUserId)

    override fun vaultFolderMasterSecret(folderId: String): ByteArray =
        vaultFolder(folderId)
}
