// Public Swift facade over the uniffi-generated KMailFFI bindings.
//
// Consumers import this module and use idiomatic Swift types
// (`URL`, `Date`, `Result`, native `async throws`) instead of the
// FFI-flavoured `FfiMailbox` / `FfiEmailSummary` records.
//
// The facade owns three responsibilities:
//
//   1. **Naming.** Drop the `Ffi` prefix from the public surface
//      via typealiases. Consumers never see `FfiMailbox` in their
//      code — they see `Mailbox`. The Ffi-prefixed types remain
//      available for advanced use cases via `KMail.FFI`.
//   2. **Type bridging.** `URL` ⇄ `String`, `Date` ⇄ Unix epoch
//      `Int64`, `Data` ⇄ `[UInt8]`. These conversions are local
//      to this file so future ABI changes are contained.
//   3. **Convenience.** A high-level `KMailClient` wrapper around
//      `KMailClientHandle` that accepts strongly-typed `URL`s and
//      a Codable `EmailDraft` instead of a JSON string.

import Foundation

// MARK: - Public typealiases

/// A KMail account mailbox (e.g. Inbox, Drafts, Sent, Vault).
///
/// Re-exported from the FFI layer without modification — the
/// fields already speak in primitive types Swift handles
/// natively.
public typealias Mailbox = FfiMailbox

/// One participant in an email's `From` / `To` / `Cc` / `Bcc`
/// header sets.
public typealias EmailAddress = FfiEmailAddress

/// Metadata-only summary of an email row (no body / attachments).
public typealias EmailSummary = FfiEmailSummary

/// Result of a single `sync()` call, broken down by what
/// happened in the JMAP delta-pull + outbound action drain.
public typealias SyncSummary = FfiSyncSummary

/// Zero-Access Vault envelope. Wraps `AeadEnvelope` from the FFI
/// layer with the same field layout (nonce + ciphertext + AAD).
public typealias VaultEnvelope = FfiAeadEnvelope

/// Confidential Send envelope. Two-layer construction (per-message
/// random DEK wrapped by an MLS-derived KEK).
public typealias ConfidentialEnvelope = FfiConfidentialEnvelope

// MARK: - Error type

extension KMailError: LocalizedError {
    /// `LocalizedError` lets `KMailError` render nicely in
    /// `print()` / SwiftUI `Text(error.localizedDescription)`
    /// without having to switch on every case in the call site.
    ///
    /// The case names mirror the Rust-side `KMailError` enum in
    /// `sdk/kmail-ffi/src/lib.rs` — if a new variant is added
    /// there, the compiler will force this switch to be updated
    /// because `KMailError` is not `@frozen` (UniFFI emits open
    /// enums) but `errorDescription` is exhaustive over the
    /// known cases at binding-generation time. A new variant
    /// will surface as a missing-case compile error.
    public var errorDescription: String? {
        switch self {
        case .Store(let message):
            return "KMail local store error: \(message)"
        case .Transport(let message):
            return "KMail transport error: \(message)"
        case .Auth(let message):
            return "KMail authentication failed: \(message)"
        case .Forbidden(let message):
            return "KMail forbidden: \(message)"
        case .NotFound(let message):
            return "KMail not found: \(message)"
        case .RateLimit(let retryAfterSeconds):
            return "KMail rate limited: retry after \(retryAfterSeconds)s"
        case .JmapMethod(let code, let description):
            return "KMail JMAP method error [\(code)]: \(description)"
        case .Protocol(let message):
            return "KMail protocol error: \(message)"
        case .HttpClient(let status, let body):
            return "KMail HTTP \(status) error: \(body)"
        case .SyncStateDiverged:
            return "KMail sync state diverged"
        case .Decryption(let message):
            return "KMail decryption error: \(message)"
        case .KeyDerivation(let message):
            return "KMail key derivation error: \(message)"
        case .KeyStore(let message):
            return "KMail keystore error: \(message)"
        case .InvalidArgument(let message):
            return "KMail invalid argument: \(message)"
        case .Cancelled:
            return "KMail operation cancelled"
        }
    }
}

// MARK: - Email draft

/// Outbound email draft.
///
/// Mirrors `kmail_core::EmailDraft` on the Rust side — both serialise
/// to the same JSON shape so the Swift / Kotlin / Electron shells
/// emit byte-identical `Email/set create` payloads. See
/// `sdk/kmail-core/src/models.rs` for the canonical wire-format
/// documentation.
///
/// The JSON key names match RFC 8621 (`mailboxIds`, `replyTo`,
/// `textBody`, `htmlBody`, `inReplyTo`) so the BFF doesn't need
/// per-client translation.
public struct EmailDraft: Codable {
    public var mailboxIds: [String: Bool]
    public var from: [EmailAddress]
    public var to: [EmailAddress]
    public var cc: [EmailAddress]
    public var bcc: [EmailAddress]
    public var replyTo: [EmailAddress]
    public var subject: String
    public var textBody: String?
    public var htmlBody: String?
    public var inReplyTo: [String]
    public var references: [String]

    public init(
        mailboxIds: [String: Bool],
        from: [EmailAddress] = [],
        to: [EmailAddress] = [],
        cc: [EmailAddress] = [],
        bcc: [EmailAddress] = [],
        replyTo: [EmailAddress] = [],
        subject: String = "",
        textBody: String? = nil,
        htmlBody: String? = nil,
        inReplyTo: [String] = [],
        references: [String] = []
    ) {
        self.mailboxIds = mailboxIds
        self.from = from
        self.to = to
        self.cc = cc
        self.bcc = bcc
        self.replyTo = replyTo
        self.subject = subject
        self.textBody = textBody
        self.htmlBody = htmlBody
        self.inReplyTo = inReplyTo
        self.references = references
    }
}

// Bridge `EmailAddress` (the FFI-emitted `FfiEmailAddress`) into
// `Codable` so `EmailDraft` can be encoded. Without this, swift's
// synthesized Codable conformance on EmailDraft would fail to
// compile because `FfiEmailAddress` is a uniffi-generated struct
// that doesn't conform to Codable by default.
extension FfiEmailAddress: Codable {
    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        let name = try c.decodeIfPresent(String.self, forKey: .name) ?? ""
        let email = try c.decode(String.self, forKey: .email)
        self.init(name: name, email: email)
    }

    public func encode(to encoder: Encoder) throws {
        var c = encoder.container(keyedBy: CodingKeys.self)
        try c.encode(name, forKey: .name)
        try c.encode(email, forKey: .email)
    }

    enum CodingKeys: String, CodingKey {
        case name
        case email
    }
}

// MARK: - Client configuration

/// Configuration for opening a `KMailClient`.
///
/// All fields except `bffURL` / `bearerToken` / `databaseURL` have
/// sensible defaults matching the Rust-side `ClientConfig::new`
/// constructor. The Swift wrapper accepts `URL` rather than
/// `String` for the BFF endpoint and the database path — invalid
/// values fail at type-check time on the Swift side instead of at
/// runtime inside the SDK.
public struct ClientConfiguration {
    public var bffURL: URL
    public var bearerToken: String
    public var databaseURL: URL
    public var attachmentCacheBytes: UInt64
    public var requestTimeout: TimeInterval
    public var retryBudget: TimeInterval
    public var initialSyncEmailWindow: UInt32
    public var accountID: String?
    public var bootstrapMailboxRole: String?

    public init(
        bffURL: URL,
        bearerToken: String,
        databaseURL: URL,
        attachmentCacheBytes: UInt64 = 256 * 1024 * 1024, // 256 MiB
        requestTimeout: TimeInterval = 30,
        retryBudget: TimeInterval = 30,
        initialSyncEmailWindow: UInt32 = 200,
        accountID: String? = nil,
        bootstrapMailboxRole: String? = nil
    ) {
        self.bffURL = bffURL
        self.bearerToken = bearerToken
        self.databaseURL = databaseURL
        self.attachmentCacheBytes = attachmentCacheBytes
        self.requestTimeout = requestTimeout
        self.retryBudget = retryBudget
        self.initialSyncEmailWindow = initialSyncEmailWindow
        self.accountID = accountID
        self.bootstrapMailboxRole = bootstrapMailboxRole
    }

    /// Lower the Swift-side config into the FFI-shaped record the
    /// `client_open` factory expects.
    func toFFI() -> KMailClientConfig {
        // Truncation guard: TimeInterval is a Double whose seconds
        // value can legally be fractional, very large, ±Infinity, or
        // NaN. The FFI accepts u32 seconds; `clampToU32` rounds to the
        // nearest integer (schoolbook), clamps to the u32 range, and
        // maps +Infinity to UInt32.max (not 0) so a caller that asks
        // for the longest possible timeout does NOT silently get a
        // 0-second deadline that fails every reqwest call immediately.
        // NaN and negative values are treated as misconfiguration and
        // collapse to 0 (fail fast on the Rust side rather than allow
        // a UInt32.max-second hang to leak through).
        let requestTimeoutSecs = ClientConfiguration.clampToU32(requestTimeout)
        let retryBudgetSecs = ClientConfiguration.clampToU32(retryBudget)
        return KMailClientConfig(
            bffUrl: bffURL.absoluteString,
            bearerToken: bearerToken,
            databasePath: databaseURL.path,
            attachmentCacheBytes: attachmentCacheBytes,
            requestTimeoutSecs: requestTimeoutSecs,
            retryBudgetSecs: retryBudgetSecs,
            initialSyncEmailWindow: initialSyncEmailWindow,
            accountId: accountID,
            bootstrapMailboxRole: bootstrapMailboxRole
        )
    }

    private static func clampToU32(_ seconds: TimeInterval) -> UInt32 {
        // `TimeInterval` (Double) admits NaN, ±Infinity, and negative
        // values. The Rust SDK expects a positive u32 second count;
        // reqwest interprets `Duration::from_secs(0)` as a zero-deadline
        // that fails every request immediately, so we must NOT return 0
        // for `+Infinity` (the caller asked for the longest bounded
        // timeout, the opposite of "fail fast").
        //
        //   - NaN / negative → 0 (invalid configuration — fail fast
        //                          so the misconfiguration surfaces
        //                          immediately rather than silently
        //                          becoming a UInt32.max-second hang)
        //   - +Infinity      → UInt32.max ("longest possible timeout")
        //   - finite ≥ u32max→ UInt32.max
        //   - otherwise      → schoolbook rounding (Swift's
        //                       `Double.rounded()` defaults to
        //                       `.toNearestOrAwayFromZero`, so
        //                       0.5 → 1 and 1.5 → 2; this is NOT
        //                       banker's `.toNearestOrEven`), cast
        //                       to UInt32
        //
        // The naive `!seconds.isFinite` predicate would conflate NaN,
        // +Inf, and -Inf into the same `0` bucket — which contradicts
        // the +Inf intent. The two-step screen below handles each
        // case correctly: `.rounded()` and `>=` propagate +Inf through
        // to the `UInt32.max` branch.
        if seconds.isNaN || seconds < 0 { return 0 }
        let rounded = seconds.rounded()
        if rounded >= TimeInterval(UInt32.max) { return UInt32.max }
        return UInt32(rounded)
    }
}

// MARK: - KMailClient

/// High-level wrapper around the uniffi-generated
/// `KMailClientHandle`.
///
/// Exposes idiomatic Swift API: `async throws` methods, native
/// `URL` / `Data` / `Date` types, and a Codable `EmailDraft`
/// instead of the raw JSON-string interface. The handle is
/// retained internally; consumers should keep the `KMailClient`
/// alive for the lifetime of the account session.
public final class KMailClient {
    /// Underlying FFI handle. Public so power-users can drop down
    /// to the raw uniffi API when the facade doesn't yet cover
    /// their use case — but doing so means missing the type
    /// bridging this facade provides, so prefer the facade
    /// methods.
    public let handle: KMailClientHandle

    /// Open a new client from a typed configuration. Persists
    /// the SQLite store at `configuration.databaseURL` and seeds
    /// the JMAP HTTP client with the bearer token.
    ///
    /// Throws `KMailError` on bootstrap failure (typically
    /// `.store` for SQLite errors or `.invalidArgument` for a
    /// malformed config).
    public init(configuration: ClientConfiguration) throws {
        self.handle = try clientOpen(config: configuration.toFFI())
    }

    /// Run a delta-pull JMAP sync, applying every mailbox /
    /// email change since the last successful sync and draining
    /// the outbound action queue.
    public func sync() async throws -> SyncSummary {
        try await handle.sync()
    }

    /// Hot-swap the OIDC bearer token. Use whenever the platform
    /// refreshes the access token; avoids tearing down and
    /// rebuilding the JMAP session.
    public func setBearerToken(_ token: String) throws {
        try handle.setBearerToken(token: token)
    }

    /// Drop the cached JMAP session. The next `sync()` call will
    /// re-fetch `/jmap/session`. Call when the shell observes a
    /// 401 reauth-required or a tenant-rebalanced push.
    public func invalidateSession() async throws {
        try await handle.invalidateSession()
    }

    /// All mailboxes currently in the local SQLite store. Does NOT
    /// hit the network; for a fresh fetch, call `sync()` first.
    public func cachedMailboxes() throws -> [Mailbox] {
        try handle.cachedMailboxes()
    }

    /// All email summaries currently in the local SQLite store
    /// for the given mailbox, newest first, capped at `limit`.
    public func cachedEmails(in mailboxID: String, limit: UInt32) throws -> [EmailSummary] {
        try handle.cachedEmailsInMailbox(mailboxId: mailboxID, limit: limit)
    }

    /// Queue a keyword-flip (e.g. `$seen → true`) for the given
    /// email. The action is persisted in the outbound-action
    /// queue and replayed against JMAP on the next `sync()`.
    public func enqueueSetKeywords(emailID: String, keywords: [String: Bool]) throws {
        let json = try JSONEncoder().encode(keywords)
        guard let str = String(data: json, encoding: .utf8) else {
            throw KMailError.InvalidArgument(message: "keywords dictionary did not encode to UTF-8")
        }
        try handle.enqueueSetKeywords(emailId: emailID, keywordsJson: str)
    }

    /// Send a Codable `EmailDraft`. Returns the JMAP-assigned
    /// email id. Internally encodes the draft to JSON and threads
    /// it through the FFI's string-based interface.
    public func sendEmail(_ draft: EmailDraft) async throws -> String {
        let encoder = JSONEncoder()
        let data = try encoder.encode(draft)
        guard let json = String(data: data, encoding: .utf8) else {
            throw KMailError.InvalidArgument(message: "EmailDraft did not encode to UTF-8")
        }
        return try await handle.sendEmail(draftJson: json)
    }

    /// Register an APNs device token with the BFF. Token bytes
    /// must already be hex-encoded — the FFI / BFF expect a
    /// string, not raw `Data`.
    public func registerAPNsToken(_ token: String) async throws {
        try await handle.registerApnsToken(token: token)
    }

    // MARK: Crypto convenience

    /// Seal a Zero-Access Vault message under the per-folder
    /// master key supplied by the plugged `MlsKeyProvider`.
    public func writeVaultMessage(folderID: String, plaintext: Data, aad: Data = Data())
        async throws -> VaultEnvelope
    {
        try await handle.writeVaultMessage(
            folderId: folderID,
            plaintext: Array(plaintext),
            aad: Array(aad)
        )
    }

    /// Open a Zero-Access Vault message previously sealed under
    /// the same folder's master key.
    public func openVaultMessage(folderID: String, envelope: VaultEnvelope)
        async throws -> Data
    {
        let bytes = try await handle.openVaultMessage(folderId: folderID, envelope: envelope)
        return Data(bytes)
    }

    /// Plug in the platform's MLS exporter-secret provider. The
    /// closure-based variant of this method is in
    /// `MlsKeyProvider.swift`.
    public func setMLSProvider(_ provider: FfiMlsKeyProvider) async throws {
        try await handle.setMlsProvider(provider: provider)
    }

    /// Drop the currently-plugged MLS provider.
    public func clearMLSProvider() async throws {
        try await handle.clearMlsProvider()
    }
}
