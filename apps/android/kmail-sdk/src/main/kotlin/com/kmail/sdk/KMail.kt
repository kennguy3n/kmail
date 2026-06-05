// Public Kotlin facade over the uniffi-generated kmail_ffi
// bindings.
//
// Consumers `import com.kmail.sdk.*` and use idiomatic Kotlin
// types (`Uri` ⇄ `String` on the consumer side, `kotlinx-coroutines`
// suspend functions, sealed-class-based errors via the generated
// enum) instead of the FFI-flavoured `FfiMailbox` / `FfiEmailSummary`
// records.
//
// The facade mirrors the Swift facade at
// `apps/ios/Sources/KMail/KMail.swift`. Cross-binding shape parity
// is load-bearing because the same JMAP / sync / crypto code runs
// behind both. The Rust `client_open` lowering ladder (the UniFFI
// entry-point that BOTH Swift and Kotlin call into) is locked
// against the parallel napi binding by
// `client_open_matches_napi_lowering_for_string_tier` in
// `sdk/kmail-ffi/src/lib.rs`, and the Kotlin-specific
// `sdkDefaults` invariant is locked by
// `kotlinDefaultsMatchRustDefaults` in the integration test
// at `apps/android/kmail-sdk/src/test/.../KMailIntegrationTests.kt`.
//
// The facade owns three responsibilities:
//
//   1. **Naming.** Drop the `Ffi` prefix from the public surface
//      via typealiases. Consumers never see `FfiMailbox` in their
//      code — they see `Mailbox`. The Ffi-prefixed types remain
//      available for advanced use cases via the `generated` package.
//   2. **Type bridging.** Defaults sourced dynamically from the
//      Rust SDK via `defaultClientConfig(...)`, so a future change
//      to a Rust default automatically flows into Kotlin on the
//      next AAR rebuild — no Kotlin literal to update.
//   3. **Convenience.** A high-level `KMailClient` wrapper around
//      `KMailClientHandle` that accepts `Json`-encodable
//      `EmailDraft` instead of a JSON string, and suspending
//      methods that bridge to the FFI's tokio runtime.

package com.kmail.sdk

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import uniffi.kmail_ffi.FfiAeadEnvelope
import uniffi.kmail_ffi.FfiBackgroundSyncHandle
import uniffi.kmail_ffi.FfiConfidentialEnvelope
import uniffi.kmail_ffi.FfiEmailAddress
import uniffi.kmail_ffi.FfiEmailSummary
import uniffi.kmail_ffi.FfiLocalNotification
import uniffi.kmail_ffi.FfiMailbox
import uniffi.kmail_ffi.FfiMlsKeyProvider
import uniffi.kmail_ffi.FfiPushIngestOutcome
import uniffi.kmail_ffi.FfiSyncSummary
import uniffi.kmail_ffi.KMailClientConfig
import uniffi.kmail_ffi.KMailClientHandle
import uniffi.kmail_ffi.KMailException
import uniffi.kmail_ffi.clientOpen
import uniffi.kmail_ffi.defaultClientConfig

// ---------------------------------------------------------------
// Public typealiases
// ---------------------------------------------------------------

/**
 * A KMail account mailbox (e.g. Inbox, Drafts, Sent, Vault).
 *
 * Re-exported from the FFI layer without modification — the fields
 * already speak in primitive types Kotlin handles natively.
 */
public typealias Mailbox = FfiMailbox

/** One participant in an email's From / To / Cc / Bcc header sets. */
public typealias EmailAddress = FfiEmailAddress

/** Metadata-only summary of an email row (no body / attachments). */
public typealias EmailSummary = FfiEmailSummary

/**
 * Result of a single `sync()` call, broken down by what happened
 * in the JMAP delta-pull + outbound action drain.
 */
public typealias SyncSummary = FfiSyncSummary

/**
 * Zero-Access Vault envelope. Wraps `AeadEnvelope` from the FFI
 * layer with the same field layout (nonce + ciphertext + AAD).
 */
public typealias VaultEnvelope = FfiAeadEnvelope

/**
 * Confidential Send envelope. Two-layer construction (per-message
 * random DEK wrapped by an MLS-derived KEK).
 */
public typealias ConfidentialEnvelope = FfiConfidentialEnvelope

/**
 * MLS exporter-secret provider plugged into the SDK by the platform
 * shell. Subclass to provide leaf and folder secrets from the host
 * MLS implementation (typically KChat's MLS SDK on Android).
 *
 * See `MlsKeyProvider.kt` for a closure-driven convenience
 * implementation (`LambdaMlsKeyProvider`) and the public docs at
 * `apps/ios/Sources/KMail/MlsKeyProvider.swift` for the parallel
 * Swift surface.
 */
public typealias MlsKeyProvider = FfiMlsKeyProvider

/**
 * A ready-to-render local notification parsed from a push payload.
 * Map onto `NotificationCompat.Builder` in the FCM service.
 */
public typealias LocalNotification = FfiLocalNotification

/**
 * Result of ingesting a push payload: the parsed notification (if
 * any), whether a preview row was cached, and whether a delta
 * `sync()` is still required.
 */
public typealias PushIngestOutcome = FfiPushIngestOutcome

/**
 * Handle to a running background sync worker. Call `stop()` to halt
 * it; releasing the handle also stops the worker.
 */
public typealias BackgroundSyncHandle = FfiBackgroundSyncHandle

// ---------------------------------------------------------------
// Email draft
// ---------------------------------------------------------------

/**
 * Outbound email draft.
 *
 * Mirrors `kmail_core::EmailDraft` on the Rust side — both
 * serialise to the same JSON shape so the Swift / Kotlin / Electron
 * shells emit byte-identical `Email/set create` payloads. See
 * `sdk/kmail-core/src/models.rs` for the canonical wire-format
 * documentation.
 *
 * The JSON key names match RFC 8621 (`mailboxIds`, `replyTo`,
 * `textBody`, `htmlBody`, `inReplyTo`) so the BFF doesn't need
 * per-client translation.
 */
@Serializable
public data class EmailDraft(
    @SerialName("mailboxIds") val mailboxIds: Map<String, Boolean>,
    @SerialName("from") val from: List<SerializableEmailAddress> = emptyList(),
    @SerialName("to") val to: List<SerializableEmailAddress> = emptyList(),
    @SerialName("cc") val cc: List<SerializableEmailAddress> = emptyList(),
    @SerialName("bcc") val bcc: List<SerializableEmailAddress> = emptyList(),
    @SerialName("replyTo") val replyTo: List<SerializableEmailAddress> = emptyList(),
    @SerialName("subject") val subject: String = "",
    @SerialName("textBody") val textBody: String? = null,
    @SerialName("htmlBody") val htmlBody: String? = null,
    @SerialName("inReplyTo") val inReplyTo: List<String> = emptyList(),
    @SerialName("references") val references: List<String> = emptyList(),
)

/**
 * Serializable mirror of `FfiEmailAddress`.
 *
 * The uniffi-generated `FfiEmailAddress` is a plain data class
 * without `@Serializable`, so kotlinx-serialization cannot encode
 * an `EmailDraft` that references it directly. Mirroring the
 * fields here keeps the wire shape identical (`{ "name", "email" }`)
 * while letting the kotlinx-serialization codegen produce the
 * encoder/decoder at compile time.
 *
 * `EmailAddress.toSerializable()` / `SerializableEmailAddress.toFfi()`
 * convert between the two representations for callers who already
 * hold an `EmailAddress` from the cached email list.
 */
@Serializable
public data class SerializableEmailAddress(
    @SerialName("name") val name: String = "",
    @SerialName("email") val email: String,
) {
    public fun toFfi(): EmailAddress = FfiEmailAddress(name = name, email = email)
}

/** Convert an FFI-layer `EmailAddress` into the Codable mirror used by `EmailDraft`. */
public fun EmailAddress.toSerializable(): SerializableEmailAddress =
    SerializableEmailAddress(name = this.name, email = this.email)

/**
 * Canonical JSON encoder for the SDK's wire-format payloads
 * (`EmailDraft`, keyword maps, etc.).
 *
 * `encodeDefaults = true` is load-bearing for cross-binding parity:
 * Swift's `JSONEncoder` always emits every Codable property
 * regardless of value, but kotlinx-serialization's default `Json`
 * elides fields that match their declared default. Without
 * `encodeDefaults = true`, a Kotlin-encoded `EmailDraft` with no
 * `cc` / `bcc` / `inReplyTo` set would emit a JSON object missing
 * those keys, while the equivalent Swift draft would emit
 * `"cc": []` / `"bcc": []` / `"inReplyTo": []`. The BFF's JMAP
 * proxy treats the two as identical, but downstream observability
 * (diffing the on-wire JSON between iOS and Android sessions of
 * the same user) gets noisy.
 */
internal val wireFormatJson: Json = Json {
    encodeDefaults = true
}

// ---------------------------------------------------------------
// Client configuration
// ---------------------------------------------------------------

/**
 * Configuration for opening a [KMailClient].
 *
 * All fields except `bffUrl` / `bearerToken` / `databasePath` have
 * sensible defaults matching the Rust-side `ClientConfig::new`
 * constructor.
 *
 * **Defaults are sourced from the Rust SDK at runtime, not
 * duplicated as Kotlin literals.** The default parameter
 * expressions read from [sdkDefaults], which is a one-time call
 * to the FFI helper `defaultClientConfig(...)` (`by lazy { ... }`
 * is thread-safe and runs at most once). This eliminates the
 * entire category of drift bugs — a future change to a Rust
 * default automatically flows into Kotlin on the next AAR rebuild,
 * without anyone having to remember to update a Kotlin literal.
 *
 * See `apps/ios/Sources/KMail/KMail.swift` for the parallel Swift
 * implementation. Cross-binding semantic parity is enforced by:
 * - `client_open_matches_napi_lowering_for_string_tier` in
 *   `sdk/kmail-ffi/src/lib.rs` (UniFFI vs napi lowering)
 * - `kotlinDefaultsMatchRustDefaults` (this module's Kotlin-side
 *   integration test) which catches Kotlin literal drift away
 *   from `defaultClientConfig(...)`
 */
public data class ClientConfiguration(
    public val bffUrl: String,
    public val bearerToken: String,
    public val databasePath: String,
    public val attachmentCacheBytes: ULong = sdkDefaults.attachmentCacheBytes!!,
    public val requestTimeoutSecs: UInt = sdkDefaults.requestTimeoutSecs!!,
    public val retryBudgetSecs: UInt = sdkDefaults.retryBudgetSecs!!,
    public val initialSyncEmailWindow: UInt = sdkDefaults.initialSyncEmailWindow!!,
    public val accountId: String? = sdkDefaults.accountId,
    public val bootstrapMailboxRole: String? = sdkDefaults.bootstrapMailboxRole,
) {
    /**
     * Lower the Kotlin-side config into the FFI-shaped record the
     * `clientOpen` factory expects.
     *
     * Every overridable field is wrapped in `Some(...)` because the
     * Kotlin data class stores non-nullable values (resolved from
     * [sdkDefaults] at construction time). The FFI's `Option<T>`
     * distinction between "no override" (`null`) and "explicit
     * override" (`Some`) is preserved at the binding boundary for
     * other foreign callers (Swift / napi / programmatic use) but
     * is collapsed to "always `Some`" on the Kotlin side because
     * Kotlin's idiom is concrete-typed fields with computed
     * defaults rather than nullable fields meaning "use SDK default".
     */
    public fun toFfi(): KMailClientConfig = KMailClientConfig(
        bffUrl = bffUrl,
        bearerToken = bearerToken,
        databasePath = databasePath,
        attachmentCacheBytes = attachmentCacheBytes,
        requestTimeoutSecs = requestTimeoutSecs,
        retryBudgetSecs = retryBudgetSecs,
        initialSyncEmailWindow = initialSyncEmailWindow,
        accountId = accountId,
        bootstrapMailboxRole = bootstrapMailboxRole,
    )

    /**
     * Produce a [KMailClientConfig] that explicitly opts out of
     * every overridable field, instructing the FFI to use its own
     * canonical defaults from `ClientConfig::new`. Mainly useful
     * for tests that exercise the `Option<T>` plumbing in
     * `client_open`.
     */
    public fun toFfiWithNullDefaults(): KMailClientConfig = KMailClientConfig(
        bffUrl = bffUrl,
        bearerToken = bearerToken,
        databasePath = databasePath,
        attachmentCacheBytes = null,
        requestTimeoutSecs = null,
        retryBudgetSecs = null,
        initialSyncEmailWindow = null,
        accountId = null,
        bootstrapMailboxRole = null,
    )

    public companion object {
        /**
         * SDK defaults sourced from the Rust core via the FFI helper.
         *
         * Evaluated once on first access (`by lazy { ... }` is
         * thread-safe). The `bffUrl` / `bearerToken` /
         * `databasePath` arguments are placeholders —
         * `defaultClientConfig` echoes them back verbatim, but
         * `ClientConfiguration` always overwrites them with the
         * caller's real values, so the placeholders are never
         * observable outside this slot.
         *
         * The contract of `default_client_config(...)` is that
         * every tier-1 override field comes back as `Some(value)`
         * — verified by the Rust test
         * `default_client_config_mirrors_core_defaults`. The `!!`
         * force-unwraps on the tier-1 fields below are safe by
         * FFI contract.
         */
        public val sdkDefaults: KMailClientConfig by lazy {
            defaultClientConfig(
                bffUrl = "https://kmail.placeholder.invalid",
                bearerToken = "placeholder",
                databasePath = "/tmp/kmail-placeholder.sqlite",
            )
        }
    }
}

// ---------------------------------------------------------------
// KMailClient
// ---------------------------------------------------------------

/**
 * High-level wrapper around the uniffi-generated
 * [KMailClientHandle].
 *
 * Exposes idiomatic Kotlin API: suspend methods (bridged to the
 * SDK's tokio runtime by uniffi's async support), kotlinx-
 * serialization-based [EmailDraft] instead of the raw JSON-string
 * interface. The handle is retained internally; consumers should
 * keep the [KMailClient] alive for the lifetime of the account
 * session.
 */
public class KMailClient
@Throws(KMailException::class)
constructor(configuration: ClientConfiguration) {
    /**
     * Underlying FFI handle. Public so power-users can drop down
     * to the raw uniffi API when the facade doesn't yet cover
     * their use case — but doing so means missing the type
     * bridging this facade provides, so prefer the facade methods.
     */
    public val handle: KMailClientHandle = clientOpen(config = configuration.toFfi())

    /** Run a delta-pull JMAP sync. */
    @Throws(KMailException::class)
    public suspend fun sync(): SyncSummary = handle.sync()

    /** Hot-swap the OIDC bearer token. */
    @Throws(KMailException::class)
    public fun setBearerToken(token: String) {
        handle.setBearerToken(token = token)
    }

    /** Drop the cached JMAP session. */
    @Throws(KMailException::class)
    public suspend fun invalidateSession() {
        handle.invalidateSession()
    }

    /** All mailboxes currently in the local SQLite store. */
    @Throws(KMailException::class)
    public fun cachedMailboxes(): List<Mailbox> = handle.cachedMailboxes()

    /**
     * All email summaries currently in the local SQLite store for
     * the given mailbox, newest first, capped at [limit].
     */
    @Throws(KMailException::class)
    public fun cachedEmails(mailboxId: String, limit: UInt): List<EmailSummary> =
        handle.cachedEmailsInMailbox(mailboxId = mailboxId, limit = limit)

    /** Queue a keyword-flip (e.g. `\$seen → true`) for an email. */
    @Throws(KMailException::class)
    public fun enqueueSetKeywords(emailId: String, keywords: Map<String, Boolean>) {
        val json = wireFormatJson.encodeToString(keywords)
        handle.enqueueSetKeywords(emailId = emailId, keywordsJson = json)
    }

    /** Send a [EmailDraft]. Returns the JMAP-assigned email id. */
    @Throws(KMailException::class)
    public suspend fun sendEmail(draft: EmailDraft): String {
        val json = wireFormatJson.encodeToString(draft)
        return handle.sendEmail(draftJson = json)
    }

    /** Register an FCM device token with the BFF. */
    @Throws(KMailException::class)
    public suspend fun registerFcmToken(token: String) {
        handle.registerFcmToken(token = token)
    }

    /**
     * Ingest a remote-message payload (the FCM `RemoteMessage.data`
     * map) from `FirebaseMessagingService.onMessageReceived`.
     *
     * The returned [PushIngestOutcome] carries a ready-to-render
     * [LocalNotification] (when the payload named a specific email),
     * tells you whether a preview row was cached for an instant
     * inbox update, and whether a follow-up `sync()` is still
     * required to converge (it almost always is — a push is a hint,
     * not an authoritative delta cursor).
     */
    @Throws(KMailException::class)
    public fun ingestPushDelivery(data: Map<String, String>): PushIngestOutcome =
        handle.ingestPushDelivery(data = data)

    /**
     * Start a background worker that runs `sync()` every
     * [intervalSeconds]. Returns a handle whose `stop()` halts the
     * worker; releasing the handle also stops it. Prefer a
     * `WorkManager` `PeriodicWorkRequest` for OS-scheduled sync —
     * this worker is for foreground-session freshness.
     */
    @Throws(KMailException::class)
    public fun startBackgroundSync(intervalSeconds: ULong): BackgroundSyncHandle =
        handle.startBackgroundSync(intervalSeconds = intervalSeconds)

    // Crypto convenience -------------------------------------------------

    /** Seal a Zero-Access Vault message under the per-folder master key. */
    @Throws(KMailException::class)
    public suspend fun writeVaultMessage(
        folderId: String,
        plaintext: ByteArray,
        aad: ByteArray = ByteArray(0),
    ): VaultEnvelope = handle.writeVaultMessage(
        folderId = folderId,
        plaintext = plaintext,
        aad = aad,
    )

    /** Open a Zero-Access Vault message previously sealed under the same folder's master key. */
    @Throws(KMailException::class)
    public suspend fun openVaultMessage(folderId: String, envelope: VaultEnvelope): ByteArray =
        handle.openVaultMessage(folderId = folderId, envelope = envelope)

    /** Plug in the platform's MLS exporter-secret provider. */
    @Throws(KMailException::class)
    public suspend fun setMlsProvider(provider: MlsKeyProvider) {
        handle.setMlsProvider(provider = provider)
    }

    /** Drop the currently-plugged MLS provider. */
    @Throws(KMailException::class)
    public suspend fun clearMlsProvider() {
        handle.clearMlsProvider()
    }
}
