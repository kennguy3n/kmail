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

/// A ready-to-render local notification parsed from a push payload.
/// Re-exported from the FFI layer; map onto `UNNotificationContent`
/// in the APNs handler.
public typealias LocalNotification = FfiLocalNotification

/// Result of ingesting a push payload: the parsed notification (if
/// any), whether a preview row was cached, and whether a delta
/// `sync()` is still required.
public typealias PushIngestOutcome = FfiPushIngestOutcome

/// Handle to a running background sync worker. Call `stop()` to
/// halt it; releasing the handle also stops the worker.
public typealias BackgroundSyncHandle = FfiBackgroundSyncHandle

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
    ///
    /// The local pattern-binding names below match the actual
    /// FFI field labels (`description` for the single-string
    /// variants, `retryAfterSeconds` for `RateLimit`, etc.). Swift
    /// pattern matching uses positional binding for enum
    /// associated values, so the local name is just a local
    /// variable — but choosing names that match the FFI labels
    /// (as defined in `sdk/kmail-ffi/src/lib.rs:69-98`) keeps
    /// this site grep-friendly: a future contributor searching
    /// for `description` will find both the Rust-side declaration
    /// AND the Swift-side consumer.
    public var errorDescription: String? {
        switch self {
        case .Store(let description):
            return "KMail local store error: \(description)"
        case .Transport(let description):
            return "KMail transport error: \(description)"
        case .Auth(let description):
            return "KMail authentication failed: \(description)"
        case .Forbidden(let description):
            return "KMail forbidden: \(description)"
        case .NotFound(let description):
            return "KMail not found: \(description)"
        case .RateLimit(let retryAfterSeconds):
            return "KMail rate limited: retry after \(retryAfterSeconds)s"
        case .JmapMethod(let code, let description):
            return "KMail JMAP method error [\(code)]: \(description)"
        case .Protocol(let description):
            return "KMail protocol error: \(description)"
        case .HttpClient(let status, let body):
            return "KMail HTTP \(status) error: \(body)"
        case .SyncStateDiverged:
            return "KMail sync state diverged"
        case .Decryption(let description):
            return "KMail decryption error: \(description)"
        case .KeyDerivation(let description):
            return "KMail key derivation error: \(description)"
        case .KeyStore(let description):
            return "KMail keystore error: \(description)"
        case .InvalidArgument(let description):
            return "KMail invalid argument: \(description)"
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

/// Canonical `JSONEncoder` for the SDK's wire-format payloads
/// (`EmailDraft`, keyword maps, etc.).
///
/// **The configuration here is load-bearing for cross-binding
/// parity with Kotlin.** Kotlin's `wireFormatJson` at
/// `apps/android/kmail-sdk/src/main/kotlin/com/kmail/sdk/KMail.kt`
/// sets `encodeDefaults = true` explicitly because
/// kotlinx-serialization's default `Json` *elides* fields that
/// match their declared default (e.g. an `EmailDraft` with empty
/// `cc` / `bcc` / `inReplyTo` would emit an object missing those
/// keys). Swift's `JSONEncoder` always emits every `Codable`
/// property regardless of value, so the parity *happens to hold*
/// today without any explicit configuration — but that's the
/// kind of "implicit guarantee" Devin Review correctly flagged.
///
/// By plumbing every encode call through this factory, the
/// invariant is documented in code: a future contributor who
/// wants to add (say) `keyEncodingStrategy = .convertToSnakeCase`
/// will see this comment block and the cross-binding parity test
/// (`testEmailDraftEncodesToRustWireFormat`) and understand that
/// Kotlin's `wireFormatJson` MUST be kept in lockstep with this
/// factory. Today both encoders are configured to emit:
///
///   - Every property, regardless of default-equality.
///   - JSON keys in property-declaration order (Swift `Codable`'s
///     synthesized order; kotlinx-serialization's declaration
///     order — they coincide because both bindings declare the
///     fields in RFC 8621 wire-format order).
///   - camelCase keys (matches RFC 8621 and the React web client).
///
/// Used by:
///   - `KMailClient.sendEmail(_ draft:)`
///   - `KMailClient.enqueueSetKeywords(emailID:keywords:)`
internal func makeKMailWireFormatJSONEncoder() -> JSONEncoder {
    let encoder = JSONEncoder()
    // No `.outputFormatting = .sortedKeys` here on purpose: Swift's
    // synthesized `Codable` encoder uses property-declaration order,
    // kotlinx-serialization's default emitter does the same, and
    // both bindings declare `EmailDraft` fields in identical
    // declaration order. Sorting keys would diverge from Kotlin
    // (which doesn't sort by default) and force the Kotlin side
    // to adopt the same opt-in, expanding the surface area of the
    // parity contract.
    return encoder
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
///
/// **Defaults are sourced from the Rust SDK at runtime, not
/// duplicated as Swift literals.** The `init` default parameter
/// expressions read from `ClientConfiguration.sdkDefaults`, which
/// is a one-time call to the FFI helper `defaultClientConfig(...)`.
/// This eliminates the entire category of drift bugs the earlier
/// version had — a future change to a Rust default automatically
/// flows into Swift on the next FFI rebuild, without anyone having
/// to remember to update a Swift literal.
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

    /// SDK defaults sourced from the Rust core via the FFI helper.
    ///
    /// Evaluated once on first access (Swift `static let` is lazy and
    /// thread-safe). The `bffUrl` / `bearerToken` / `databasePath`
    /// arguments are placeholders — `defaultClientConfig` echoes them
    /// back verbatim, but `ClientConfiguration.init` always overwrites
    /// them with the caller's real values, so the placeholder strings
    /// are never observable outside this static slot.
    ///
    /// The contract of `default_client_config(...)` is that every
    /// override field comes back as `Some(value)` (it returns "what
    /// values would I use if you passed `None` for every override?")
    /// — so the force-unwraps below are safe by FFI contract.
    public static let sdkDefaults: KMailClientConfig = defaultClientConfig(
        bffUrl: "https://kmail.placeholder.invalid",
        bearerToken: "placeholder",
        databasePath: "/tmp/kmail-placeholder.sqlite"
    )

    public init(
        bffURL: URL,
        bearerToken: String,
        databaseURL: URL,
        // Defaults come from `ClientConfiguration.sdkDefaults`, which
        // is a one-shot FFI call to `defaultClientConfig(...)` returning
        // the canonical Rust `ClientConfig::new` defaults. Swift literal
        // duplication is impossible here by construction. If
        // `ClientConfig::new` ever changes a default (e.g. retry budget
        // from 60 to 90), every iOS caller using default settings picks
        // up the new value automatically on the next XCFramework rebuild.
        //
        // The force-unwraps on `sdkDefaults.*!` are safe by FFI contract:
        // `default_client_config(...)` always returns `Some(value)` for
        // every overridable field (see `sdk/kmail-ffi/src/lib.rs` for the
        // contract). The `Rust unit test default_client_config_mirrors_
        // core_defaults` locks this down.
        attachmentCacheBytes: UInt64 = ClientConfiguration.sdkDefaults.attachmentCacheBytes!,
        requestTimeout: TimeInterval = TimeInterval(ClientConfiguration.sdkDefaults.requestTimeoutSecs!),
        retryBudget: TimeInterval = TimeInterval(ClientConfiguration.sdkDefaults.retryBudgetSecs!),
        initialSyncEmailWindow: UInt32 = ClientConfiguration.sdkDefaults.initialSyncEmailWindow!,
        accountID: String? = ClientConfiguration.sdkDefaults.accountId,
        bootstrapMailboxRole: String? = ClientConfiguration.sdkDefaults.bootstrapMailboxRole
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
    ///
    /// Every overridable field is wrapped in `Some(...)` because the
    /// Swift struct stores non-optional values (resolved from
    /// `sdkDefaults` at init time). The FFI's `Option<T>` distinction
    /// between "no override" (`None`) and "explicit override" (`Some`)
    /// is preserved at the binding boundary for other foreign callers
    /// (Kotlin / napi / programmatic use) but is collapsed to "always
    /// `Some`" on the Swift side because Swift's idiom is to expose
    /// concrete-typed fields with computed defaults rather than nilable
    /// fields meaning "use SDK default".
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
        // The FFI fields are `Option<T>` (`T?` in Swift) so `Some(value)`
        // / `nil` distinguishes "override" from "use SDK default" for
        // other foreign callers. On the Swift side, every field is
        // already resolved (to a Rust default or an explicit override),
        // so we always pass `Some(value)`.
        return KMailClientConfig(
            bffUrl: bffURL.absoluteString,
            bearerToken: bearerToken,
            databasePath: databaseURL.path,
            attachmentCacheBytes: Optional(attachmentCacheBytes),
            requestTimeoutSecs: Optional(requestTimeoutSecs),
            retryBudgetSecs: Optional(retryBudgetSecs),
            initialSyncEmailWindow: Optional(initialSyncEmailWindow),
            accountId: accountID,
            bootstrapMailboxRole: bootstrapMailboxRole
        )
    }

    /// Helper: produce a `KMailClientConfig` with every optional
    /// override field set to `nil`.
    ///
    /// **The two field tiers do not behave the same way on the FFI
    /// side**, so this is NOT equivalent to "use SDK defaults for
    /// everything":
    ///
    /// * **Tier 1 — numeric fields** (`attachmentCacheBytes`,
    ///   `requestTimeoutSecs`, `retryBudgetSecs`,
    ///   `initialSyncEmailWindow`). `nil` means "inherit the
    ///   `ClientConfig::new` default" — the FFI ladder only fires
    ///   the override when the value is `Some(...)`. So `nil` here
    ///   produces the same observable value as
    ///   `defaultClientConfig(...)`'s numeric fields.
    /// * **Tier 2 — string fields** (`accountId`,
    ///   `bootstrapMailboxRole`). `nil` is a verbatim "no value"
    ///   and **overrides** the Rust default. In particular,
    ///   `bootstrapMailboxRole: nil` overrides the
    ///   `ClientConfig::new` default of `Some("inbox")` to `None`,
    ///   so a client opened with this record will skip the
    ///   inbox-bootstrap step. `accountId: nil` is also verbatim,
    ///   but its Rust default is already `None` so there's no
    ///   observable difference.
    ///
    /// This helper exists ONLY for the FFI `Option<T>` plumbing
    /// tests — production callers that want SDK defaults should
    /// construct a `ClientConfiguration` (whose tier-2 fields are
    /// seeded from `ClientConfiguration.sdkDefaults`, which sources
    /// `Some("inbox")` from the Rust side) and call
    /// `toFFI()` instead. The shared lowering helper in
    /// `kmail-core` (`ClientConfig::apply_optional_overrides`) is
    /// where these two tiers are defined; the doc comment there is
    /// the canonical reference.
    public func toFFIWithNoneDefaults() -> KMailClientConfig {
        KMailClientConfig(
            bffUrl: bffURL.absoluteString,
            bearerToken: bearerToken,
            databasePath: databaseURL.path,
            attachmentCacheBytes: nil,
            requestTimeoutSecs: nil,
            retryBudgetSecs: nil,
            initialSyncEmailWindow: nil,
            accountId: nil,
            bootstrapMailboxRole: nil
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
        // Routed through `makeKMailWireFormatJSONEncoder()` so the
        // cross-binding parity invariant with Kotlin's
        // `wireFormatJson` is documented at every call site rather
        // than relying on Swift's implicit default-emission behaviour.
        let json = try makeKMailWireFormatJSONEncoder().encode(keywords)
        guard let str = String(data: json, encoding: .utf8) else {
            throw KMailError.InvalidArgument(description: "keywords dictionary did not encode to UTF-8")
        }
        try handle.enqueueSetKeywords(emailId: emailID, keywordsJson: str)
    }

    /// Send a Codable `EmailDraft`. Returns the JMAP-assigned
    /// email id. Internally encodes the draft to JSON and threads
    /// it through the FFI's string-based interface.
    public func sendEmail(_ draft: EmailDraft) async throws -> String {
        // Routed through `makeKMailWireFormatJSONEncoder()` so the
        // cross-binding parity invariant with Kotlin's
        // `wireFormatJson` is documented at the call site. A future
        // contributor changing this configuration must also change
        // the Kotlin side or break the byte-for-byte JMAP payload
        // parity that the BFF observability dashboards rely on.
        let encoder = makeKMailWireFormatJSONEncoder()
        let data = try encoder.encode(draft)
        guard let json = String(data: data, encoding: .utf8) else {
            throw KMailError.InvalidArgument(description: "EmailDraft did not encode to UTF-8")
        }
        return try await handle.sendEmail(draftJson: json)
    }

    /// Register an APNs device token with the BFF. Token bytes
    /// must already be hex-encoded — the FFI / BFF expect a
    /// string, not raw `Data`.
    public func registerAPNsToken(_ token: String) async throws {
        try await handle.registerApnsToken(token: token)
    }

    /// Ingest a remote-notification payload (the APNs `userInfo`
    /// dictionary, flattened to `[String: String]`).
    ///
    /// Call this from `application(_:didReceiveRemoteNotification:)`
    /// or the `UNNotificationServiceExtension`. The returned
    /// `PushIngestOutcome` carries a ready-to-render
    /// `LocalNotification` (when the payload named a specific
    /// email), tells you whether a preview row was cached for an
    /// instant inbox update, and whether a follow-up `sync()` is
    /// still required to converge (it almost always is — a push is
    /// a hint, not an authoritative delta cursor).
    public func ingestPushDelivery(_ payload: [String: String]) throws -> PushIngestOutcome {
        try handle.ingestPushDelivery(data: payload)
    }

    /// Start a background worker that runs `sync()` every
    /// `interval`. Returns a handle whose `stop()` halts the
    /// worker; releasing the handle also stops it.
    ///
    /// `interval` is rounded to whole seconds (the SDK's periodic
    /// tick granularity) and clamped to at least one second.
    @discardableResult
    public func startBackgroundSync(interval: TimeInterval) throws -> BackgroundSyncHandle {
        let seconds = KMailClient.clampIntervalSecs(interval)
        return try handle.startBackgroundSync(intervalSeconds: seconds)
    }

    /// Clamp a `TimeInterval` (a `Double`, so it admits `NaN`,
    /// `±Infinity`, fractional and out-of-range values) to a whole
    /// number of seconds suitable for the FFI's `u64` interval.
    ///
    /// `UInt64(max(1, interval.rounded()))` is NOT enough: `max` only
    /// raises the floor, so `+Infinity` or any finite value above
    /// `UInt64.max` (~1.8e19) flows straight into the `UInt64(_:)`
    /// initialiser and *traps* at runtime. We mirror
    /// `ClientConfiguration.clampToU32`: floor at 1s (a sub-second
    /// background-sync cadence is meaningless and 0 would panic the
    /// Rust worker), map `+Infinity` / overflow to `UInt64.max`, and
    /// round everything else to the nearest second.
    private static func clampIntervalSecs(_ interval: TimeInterval) -> UInt64 {
        if interval.isNaN { return 1 }
        let rounded = interval.rounded()
        if rounded < 1 { return 1 }
        if rounded >= TimeInterval(UInt64.max) { return UInt64.max }
        return UInt64(rounded)
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
