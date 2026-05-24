// Swift integration tests for the KMail SDK iOS binding.
//
// These tests exercise the real FFI path — they instantiate a
// real `KMailClient`, plug a real `ClosureMlsKeyProvider`, and
// run real AES-256-GCM seal / open through the Rust crypto
// implementation. The only thing mocked is the JMAP server (we
// don't sync against a live BFF in these tests; that lives in
// the docker-compose nightly integration suite).
//
// The tests are intentionally Foundation-only (no XCTest async
// helpers) so they run identically on Apple Silicon Mac, Intel
// Mac, and iOS simulator slices of the XCFramework.

import XCTest
@testable import KMail

final class KMailIntegrationTests: XCTestCase {

    /// Each test gets a fresh SQLite database in a per-test
    /// temporary directory so concurrent tests don't collide on
    /// the same on-disk store. The directory is removed in
    /// `tearDownWithError`.
    private var tempDir: URL!

    override func setUpWithError() throws {
        try super.setUpWithError()
        tempDir = FileManager.default
            .temporaryDirectory
            .appendingPathComponent("kmail-tests-\(UUID().uuidString)", isDirectory: true)
        try FileManager.default.createDirectory(
            at: tempDir, withIntermediateDirectories: true
        )
    }

    override func tearDownWithError() throws {
        if let tempDir = tempDir {
            try? FileManager.default.removeItem(at: tempDir)
        }
        try super.tearDownWithError()
    }

    private func makeClient() throws -> KMailClient {
        let config = ClientConfiguration(
            bffURL: URL(string: "https://kmail.test")!,
            bearerToken: "test-bearer",
            databaseURL: tempDir.appendingPathComponent("kmail.sqlite")
        )
        return try KMailClient(configuration: config)
    }

    // MARK: - Construction

    /// Opening a client should populate an empty mailbox list (no
    /// sync has been run, so the local SQLite store is empty but
    /// queryable).
    func testClientOpensWithEmptyCache() throws {
        let client = try makeClient()
        let mailboxes = try client.cachedMailboxes()
        XCTAssertTrue(mailboxes.isEmpty, "fresh client should have no cached mailboxes")
    }

    /// Opening two clients against the same database path is
    /// allowed (SQLite WAL mode supports concurrent readers) and
    /// must not corrupt the schema.
    func testClientOpensTwiceAgainstSameDatabase() throws {
        let _ = try makeClient()
        let second = try makeClient()
        XCTAssertTrue(try second.cachedMailboxes().isEmpty)
    }

    // MARK: - Vault crypto (raw-key surface)

    /// `seal_vault_envelope` + `decrypt_vault_envelope` are the
    /// raw-key crypto surface — caller passes the 32-byte
    /// folder master key directly. Verify a roundtrip preserves
    /// the plaintext byte-for-byte and that the envelope's nonce
    /// is the required 12 bytes.
    func testVaultSealOpenRoundtrip() throws {
        let client = try makeClient()
        let folderKey = Data(repeating: 0x42, count: 32) // 32 = HKDF output
        let plaintext = Data("hello vault".utf8)
        let aad = Data("v1:folder=alpha".utf8)

        let envelope = try client.handle.sealVaultEnvelope(
            folderMasterKey: Array(folderKey),
            plaintext: Array(plaintext),
            aad: Array(aad)
        )
        XCTAssertEqual(envelope.nonce.count, 12, "AES-GCM nonce must be 12 bytes per RFC 5116")
        XCTAssertFalse(envelope.ciphertext.isEmpty)
        XCTAssertEqual(envelope.aad, Array(aad))

        let recovered = try client.handle.decryptVaultEnvelope(
            folderMasterKey: Array(folderKey),
            envelope: envelope
        )
        XCTAssertEqual(Data(recovered), plaintext, "roundtrip plaintext must match input")
    }

    /// A 16-byte folder master key (too short) must surface as
    /// `KMailError.invalidArgument` / `.keyDerivation` from the
    /// Rust side — not crash, not silently truncate.
    func testVaultSealRejectsShortFolderKey() throws {
        let client = try makeClient()
        let shortKey = Data(repeating: 0x42, count: 16)

        XCTAssertThrowsError(
            try client.handle.sealVaultEnvelope(
                folderMasterKey: Array(shortKey),
                plaintext: Array(Data("oops".utf8)),
                aad: Array()
            )
        ) { error in
            guard let kmailError = error as? KMailError else {
                return XCTFail("expected KMailError, got \(type(of: error)): \(error)")
            }
            // The exact case depends on where in the crypto
            // stack the length is rejected — `kdf::hkdf_derive`
            // returns a Crypto error and the FFI layer maps
            // it to `KeyDerivation`. Either KeyDerivation or
            // InvalidArgument is acceptable; both surface the
            // bad input to the caller as a typed error.
            switch kmailError {
            case .KeyDerivation, .InvalidArgument:
                break
            default:
                XCTFail("expected KeyDerivation or InvalidArgument, got \(kmailError)")
            }
        }
    }

    // MARK: - Confidential Send crypto (raw-key surface)

    /// `seal_confidential_envelope` + `open_confidential_envelope`
    /// roundtrip with a 32-byte MLS leaf secret. Verify that
    /// `kek_salt`, the wrapped DEK envelope, and the payload
    /// envelope all have the documented sizes, and the
    /// plaintext recovers byte-identically.
    func testConfidentialSealOpenRoundtrip() throws {
        let client = try makeClient()
        let leafSecret = Data(repeating: 0xA5, count: 32)
        let plaintext = Data("hello confidential send".utf8)
        let payloadAad = Data("v1:recipient=alice@kmail.test".utf8)
        let wrapAad = Data("v1:kek=ConfidentialSendDekWrap".utf8)

        let envelope = try client.handle.sealConfidentialEnvelope(
            mlsLeafSecret: Array(leafSecret),
            plaintext: Array(plaintext),
            payloadAad: Array(payloadAad),
            wrapAad: Array(wrapAad)
        )
        XCTAssertEqual(envelope.kekSalt.count, 32, "KEK salt must be 32 bytes")
        XCTAssertEqual(envelope.wrappedDek.nonce.count, 12)
        XCTAssertEqual(envelope.payload.nonce.count, 12)

        let recovered = try client.handle.openConfidentialEnvelope(
            mlsLeafSecret: Array(leafSecret),
            envelope: envelope
        )
        XCTAssertEqual(Data(recovered), plaintext)
    }

    // MARK: - MLS provider plumbing

    /// Plug a `ClosureMlsKeyProvider`, write a vault message
    /// through the convenience surface, then read it back. This
    /// exercises the foreign-callback path end-to-end:
    /// Rust SDK → FFI adapter → Swift closure → Rust SDK.
    func testClosureMlsProviderRoundtrip() async throws {
        let client = try makeClient()
        let provider = ClosureMlsKeyProvider(
            confidentialSend: { _ in
                Data(repeating: 0xC1, count: 32)
            },
            vaultFolder: { _ in
                Data(repeating: 0xF0, count: 32)
            }
        )
        try await client.setMLSProvider(provider)

        let plaintext = Data("hello via MLS provider".utf8)
        let aad = Data("v1:folder=mls-test".utf8)
        let envelope = try await client.writeVaultMessage(
            folderID: "folder-mls-test",
            plaintext: plaintext,
            aad: aad
        )
        let recovered = try await client.openVaultMessage(
            folderID: "folder-mls-test", envelope: envelope
        )
        XCTAssertEqual(recovered, plaintext)
    }

    /// A `ClosureMlsKeyProvider` that returns a wrong-length
    /// secret must surface as `KMailError.keyStore(...)` with a
    /// message identifying the scope (Vault or Confidential
    /// Send) — see `sdk/kmail-ffi/src/lib.rs`
    /// `ForeignMlsKeyProvider` validation.
    func testClosureMlsProviderRejectsWrongLengthSecret() async throws {
        let client = try makeClient()
        let provider = ClosureMlsKeyProvider(
            confidentialSend: { _ in Data(repeating: 0xAB, count: 33) },
            vaultFolder: { _ in Data(repeating: 0xCD, count: 31) }
        )
        try await client.setMLSProvider(provider)

        do {
            _ = try await client.writeVaultMessage(
                folderID: "folder-x",
                plaintext: Data("oops".utf8)
            )
            XCTFail("writeVaultMessage should have thrown on wrong-length secret")
        } catch let error as KMailError {
            switch error {
            case .KeyStore(let description):
                // Local binding matches the FFI field label `description`
                // (see `sdk/kmail-ffi/src/lib.rs::KMailError::KeyStore`).
                // Swift pattern matching uses positional binding for enum
                // associated values, so the local name is grep-friendly,
                // not semantically required.
                XCTAssertTrue(
                    description.contains("31") || description.contains("Vault"),
                    "expected wrong-length / Vault scope in description, got: \(description)"
                )
            default:
                XCTFail("expected KeyStore error, got \(error)")
            }
        }
    }

    // MARK: - EmailDraft codable

    /// Roundtrip an `EmailDraft` through JSON to verify the
    /// Codable conformance matches the Rust wire-format that
    /// `KMailClient::send_email` deserialises on the other side.
    /// We don't actually call `sendEmail` here (no live JMAP
    /// server) — we just verify the encoded JSON shape.
    func testEmailDraftEncodesToRustWireFormat() throws {
        let draft = EmailDraft(
            mailboxIds: ["mb-drafts": true],
            from: [EmailAddress(name: "Alice", email: "alice@kmail.test")],
            to: [EmailAddress(name: "Bob", email: "bob@kmail.test")],
            subject: "hello",
            textBody: "hi bob"
        )
        // Encode via the same `makeKMailWireFormatJSONEncoder()`
        // factory that `KMailClient.sendEmail` uses in production.
        // Using a fresh `JSONEncoder()` here would let the test pass
        // even if the production encoder later gained a configuration
        // (e.g. snake_case key conversion) that broke cross-binding
        // parity with Kotlin's `wireFormatJson`.
        let data = try makeKMailWireFormatJSONEncoder().encode(draft)
        let json = try XCTUnwrap(String(data: data, encoding: .utf8))

        // Keys must be exactly the JMAP RFC 8621 field names —
        // mailboxIds, replyTo, textBody, htmlBody, inReplyTo —
        // because the BFF and the React web client also speak
        // that shape. A camelCase deviation here would break
        // wire-format compatibility.
        XCTAssertTrue(json.contains("\"mailboxIds\""), "expected mailboxIds key, got: \(json)")
        XCTAssertTrue(json.contains("\"textBody\""), "expected textBody key, got: \(json)")
        XCTAssertTrue(json.contains("\"inReplyTo\""), "expected inReplyTo key, got: \(json)")
        XCTAssertTrue(json.contains("alice@kmail.test"))

        // Lock in the cross-binding parity invariant: every
        // optional/empty-default field must be emitted, even when
        // its value matches the declared default. This is the
        // Swift-side mirror of Kotlin's `encodeDefaults = true`
        // (see `KMail.kt`'s `wireFormatJson` doc block).
        XCTAssertTrue(json.contains("\"cc\""), "expected cc key (empty), got: \(json)")
        XCTAssertTrue(json.contains("\"bcc\""), "expected bcc key (empty), got: \(json)")
        XCTAssertTrue(json.contains("\"replyTo\""), "expected replyTo key (empty), got: \(json)")
        XCTAssertTrue(json.contains("\"references\""), "expected references key (empty), got: \(json)")
    }

    // MARK: - Default contract

    /// `ClientConfiguration`'s Swift-side defaults must be
    /// bit-identical to the SDK's Rust-side `ClientConfig::new`
    /// defaults exposed through the FFI helper
    /// `default_client_config(...)`. This is the load-bearing
    /// drift-prevention test — if a Rust default changes (e.g.
    /// `retry_budget` moves from 60s to 90s) the new value flows
    /// out through `defaultClientConfig` and this test fails
    /// loudly until the Swift literal is updated to match.
    ///
    /// Without this test, prior to its addition, the Swift
    /// `retryBudget` defaulted to 30s while Rust defaulted to
    /// 60s — every iOS client got half the intended retry budget
    /// because the FFI `client_open` unconditionally overwrites
    /// `core_cfg.retry_budget` with whatever the Swift side
    /// passes in.
    func testSwiftDefaultsMatchRustDefaults() {
        let bff = URL(string: "https://kmail.test")!
        let bearer = "test-bearer"
        let dbURL = URL(fileURLWithPath: "/tmp/kmail.sqlite")

        // ClientConfiguration sources its defaults from
        // `defaultClientConfig(...)` at runtime via the static
        // `sdkDefaults` slot, so calling `.toFFI()` here should produce
        // values bit-identical to a direct call to the FFI helper.
        let swift = ClientConfiguration(
            bffURL: bff,
            bearerToken: bearer,
            databaseURL: dbURL
        ).toFFI()
        let rust = defaultClientConfig(
            bffUrl: bff.absoluteString,
            bearerToken: bearer,
            databasePath: dbURL.path
        )

        XCTAssertEqual(swift.bffUrl, rust.bffUrl)
        XCTAssertEqual(swift.bearerToken, rust.bearerToken)
        XCTAssertEqual(swift.databasePath, rust.databasePath)
        XCTAssertEqual(
            swift.attachmentCacheBytes, rust.attachmentCacheBytes,
            "attachmentCacheBytes drifted between Swift default and Rust ClientConfig::new"
        )
        XCTAssertEqual(
            swift.requestTimeoutSecs, rust.requestTimeoutSecs,
            "requestTimeout drifted between Swift default and Rust ClientConfig::new"
        )
        XCTAssertEqual(
            swift.retryBudgetSecs, rust.retryBudgetSecs,
            "retryBudget drifted between Swift default and Rust ClientConfig::new (was 30 vs 60 — every iOS client got half the intended retry budget)"
        )
        XCTAssertEqual(
            swift.initialSyncEmailWindow, rust.initialSyncEmailWindow,
            "initialSyncEmailWindow drifted between Swift default and Rust ClientConfig::new"
        )
        XCTAssertEqual(
            swift.accountId, rust.accountId,
            "accountID drifted between Swift default and Rust ClientConfig::new"
        )
        XCTAssertEqual(
            swift.bootstrapMailboxRole, rust.bootstrapMailboxRole,
            "bootstrapMailboxRole drifted between Swift default and Rust ClientConfig::new (Rust defaults to Some(\"inbox\"); Swift must match)"
        )
    }

    /// `ClientConfiguration.toFFIWithNoneDefaults()` produces a
    /// `KMailClientConfig` with every overridable field set to
    /// `nil`. The shape of that record is what this test verifies.
    ///
    /// **Important: this is NOT equivalent to `defaultClientConfig(...)`
    /// on the Rust side.** The two records lower to different
    /// observable `ClientConfig` states because of the two-tier
    /// `Option<T>` contract baked into
    /// `ClientConfig::apply_optional_overrides`:
    ///
    /// * Tier 1 (numeric) — `nil` means "inherit Rust default", so
    ///   the all-`nil` record's `attachmentCacheBytes` etc. resolve
    ///   to the same values as `defaultClientConfig(...)`.
    /// * Tier 2 (string) — `nil` is verbatim. The all-`nil` record
    ///   resolves `bootstrapMailboxRole` to `None`, whereas
    ///   `defaultClientConfig(...).bootstrapMailboxRole` is
    ///   `Some("inbox")`. So the two records produce different
    ///   observable configurations.
    ///
    /// The Rust-side test `client_open_lowers_none_to_core_defaults`
    /// (`sdk/kmail-ffi/src/lib.rs`) explicitly documents and asserts
    /// the divergence — see its body for the canonical statement of
    /// the contract. This Swift-side test mirrors it: verify the
    /// shape on the Swift side (all override fields are `nil`,
    /// non-overridable fields echo the caller's values), and assert
    /// the tier-2 divergence against `defaultClientConfig(...)` so
    /// a future contributor who tries to "unify" the two records
    /// (e.g. making `toFFIWithNoneDefaults` echo `Some("inbox")`)
    /// has to update this test deliberately.
    func testNoneDefaultsRecordShapeAndTier2Divergence() {
        let bff = URL(string: "https://kmail.test")!
        let bearer = "test-bearer"
        let dbURL = URL(fileURLWithPath: "/tmp/kmail.sqlite")

        let noneForm = ClientConfiguration(
            bffURL: bff,
            bearerToken: bearer,
            databaseURL: dbURL
        ).toFFIWithNoneDefaults()

        // Shape: every overridable field is `nil`.
        XCTAssertNil(noneForm.attachmentCacheBytes)
        XCTAssertNil(noneForm.requestTimeoutSecs)
        XCTAssertNil(noneForm.retryBudgetSecs)
        XCTAssertNil(noneForm.initialSyncEmailWindow)
        XCTAssertNil(noneForm.accountId)
        XCTAssertNil(noneForm.bootstrapMailboxRole)

        // Non-overridable: echo the caller's values.
        XCTAssertEqual(noneForm.bffUrl, bff.absoluteString)
        XCTAssertEqual(noneForm.bearerToken, bearer)
        XCTAssertEqual(noneForm.databasePath, dbURL.path)

        // Tier-2 divergence against `defaultClientConfig(...)`. The
        // all-`nil` record's `bootstrapMailboxRole` is `nil`,
        // whereas `defaultClientConfig(...)` returns `Some("inbox")`.
        // A future contributor who tries to make these match (e.g.
        // by changing `toFFIWithNoneDefaults` to echo `Some("inbox")`
        // for tier-2 fields) must intentionally update this assertion.
        let defaultsForm = defaultClientConfig(
            bffUrl: bff.absoluteString,
            bearerToken: bearer,
            databasePath: dbURL.path
        )
        XCTAssertNil(noneForm.bootstrapMailboxRole)
        XCTAssertEqual(defaultsForm.bootstrapMailboxRole, "inbox")
        XCTAssertNotEqual(
            noneForm.bootstrapMailboxRole, defaultsForm.bootstrapMailboxRole,
            "tier-2 fields diverge: all-nil overrides Some(\"inbox\") to nil"
        )
    }

    // MARK: - Timeout clamp

    /// `ClientConfiguration.toFFI()` lowers `TimeInterval` (Double)
    /// timeout fields into u32-second fields the Rust FFI expects.
    /// The clamp logic must preserve the user's intent across the
    /// Double → UInt32 narrowing:
    ///
    ///   - NaN / negative → 0 (fail fast on invalid configuration)
    ///   - +Infinity → UInt32.max (caller asked for the longest
    ///     possible bounded timeout)
    ///   - finite that overflows u32 → UInt32.max
    ///   - small fractional values → rounded to nearest u32
    ///
    /// The +Infinity → UInt32.max case is load-bearing for the
    /// "no deadline" idiom — without it, a caller that sets
    /// `requestTimeout = .infinity` would get `Duration::from_secs(0)`
    /// on the Rust side, which reqwest interprets as a zero-deadline
    /// timeout that immediately fails every request.
    func testClientConfigurationClampsTimeoutsCorrectly() {
        let baseURL = URL(string: "https://kmail.test")!
        let bearer = "test-bearer"
        let dbURL = URL(fileURLWithPath: "/tmp/kmail.sqlite")

        func makeConfig(timeout: TimeInterval, retry: TimeInterval) -> KMailClientConfig {
            ClientConfiguration(
                bffURL: baseURL,
                bearerToken: bearer,
                databaseURL: dbURL,
                requestTimeout: timeout,
                retryBudget: retry
            ).toFFI()
        }

        // +Infinity → UInt32.max (longest bounded timeout).
        let infinite = makeConfig(timeout: .infinity, retry: .infinity)
        XCTAssertEqual(
            infinite.requestTimeoutSecs, UInt32.max,
            "+Infinity must clamp to UInt32.max, not 0 (would cause immediate-fail reqwest)"
        )
        XCTAssertEqual(infinite.retryBudgetSecs, UInt32.max)

        // NaN → 0 (invalid input fails fast).
        let nan = makeConfig(timeout: .nan, retry: .nan)
        XCTAssertEqual(nan.requestTimeoutSecs, 0, "NaN must clamp to 0 (invalid input fails fast)")
        XCTAssertEqual(nan.retryBudgetSecs, 0)

        // Negative → 0.
        let negative = makeConfig(timeout: -5, retry: -100)
        XCTAssertEqual(negative.requestTimeoutSecs, 0)
        XCTAssertEqual(negative.retryBudgetSecs, 0)

        // -Infinity → 0 (same as negative).
        let negInf = makeConfig(timeout: -.infinity, retry: -.infinity)
        XCTAssertEqual(negInf.requestTimeoutSecs, 0)
        XCTAssertEqual(negInf.retryBudgetSecs, 0)

        // Fractional → schoolbook rounding (.toNearestOrAwayFromZero).
        // Swift's `Double.rounded()` defaults to schoolbook rounding,
        // so 0.5 → 1 (rounded away from zero, *not* banker's rounding
        // to 0) and 1.4 → 1.
        let fractional = makeConfig(timeout: 0.5, retry: 1.4)
        XCTAssertEqual(fractional.requestTimeoutSecs, 1)
        XCTAssertEqual(fractional.retryBudgetSecs, 1)

        // Large but finite values that exceed UInt32.max → UInt32.max.
        let huge = makeConfig(timeout: 1e20, retry: 1e30)
        XCTAssertEqual(huge.requestTimeoutSecs, UInt32.max)
        XCTAssertEqual(huge.retryBudgetSecs, UInt32.max)

        // Normal values pass through unchanged.
        let normal = makeConfig(timeout: 30, retry: 60)
        XCTAssertEqual(normal.requestTimeoutSecs, 30)
        XCTAssertEqual(normal.retryBudgetSecs, 60)
    }

    /// Sanity-check `KMailError.localizedDescription` for the
    /// common cases. Without `LocalizedError` conformance, Swift
    /// would render the error as something like
    /// "kmail_ffi.KMailError.Store(description: …)" which is not
    /// user-presentable.
    func testKMailErrorLocalizedDescription() {
        let store = KMailError.Store(description: "schema migration failed")
        XCTAssertEqual(
            store.localizedDescription, "KMail local store error: schema migration failed"
        )

        let cancelled = KMailError.Cancelled
        XCTAssertEqual(cancelled.localizedDescription, "KMail operation cancelled")

        let rateLimit = KMailError.RateLimit(retryAfterSeconds: 30)
        XCTAssertEqual(
            rateLimit.localizedDescription, "KMail rate limited: retry after 30s"
        )
    }
}
