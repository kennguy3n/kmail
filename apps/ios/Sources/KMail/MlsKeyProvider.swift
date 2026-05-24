// Convenience adapters for plugging an MLS exporter-secret
// provider into the KMail SDK from Swift.
//
// The uniffi-generated `FfiMlsKeyProvider` is a class-shaped
// foreign trait: callers implement it by subclassing or by
// declaring a final class that conforms to the protocol. This is
// fine for production shells that wrap a long-lived MLS SDK
// instance, but tests / quick prototypes want closures.
//
// `ClosureMlsKeyProvider` lets you supply two closures and get a
// fully-conformant `FfiMlsKeyProvider`. It's the same pattern as
// `UICollectionViewDiffableDataSource`'s closure-based snapshot
// adapter — the heavy class is hidden behind a value-typed
// interface that's pleasant in Swift code.

import Foundation

/// Closure-driven implementation of `FfiMlsKeyProvider`.
///
/// `confidentialSendLeaf` and `vaultFolderMaster` MUST return
/// exactly 32 bytes for each input. Returning any other length
/// will surface to the SDK as `KMailError.keyStore(...)` (the
/// FFI adapter validates the contract — see
/// `sdk/kmail-ffi/src/lib.rs` `ForeignMlsKeyProvider`).
///
/// Determinism: both closures MUST return the same bytes for the
/// same input across the lifetime of a single MLS epoch. The
/// platform shell should rotate its in-memory cache only when
/// the MLS epoch rotates (typically driven by a `MlsKeyProvider`
/// owned by the KChat MLS SDK).
///
/// Threading: both closures may be invoked concurrently from
/// multiple Tokio worker threads inside the SDK. Implementations
/// must be thread-safe — UniFFI's `with_foreign` callbacks are
/// dispatched on whichever runtime thread happens to be free.
public final class ClosureMlsKeyProvider: FfiMlsKeyProvider {
    public typealias SecretProducer = (String) throws -> Data

    private let confidentialSend: SecretProducer
    private let vaultFolder: SecretProducer

    public init(
        confidentialSend: @escaping SecretProducer,
        vaultFolder: @escaping SecretProducer
    ) {
        self.confidentialSend = confidentialSend
        self.vaultFolder = vaultFolder
    }

    public func confidentialSendLeafSecret(recipientUserId: String) throws -> [UInt8] {
        let data = try confidentialSend(recipientUserId)
        return Array(data)
    }

    public func vaultFolderMasterSecret(folderId: String) throws -> [UInt8] {
        let data = try vaultFolder(folderId)
        return Array(data)
    }
}
