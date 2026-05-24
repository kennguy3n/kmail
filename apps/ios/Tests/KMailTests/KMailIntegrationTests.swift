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
            case .KeyStore(let message):
                XCTAssertTrue(
                    message.contains("31") || message.contains("Vault"),
                    "expected wrong-length / Vault scope in message, got: \(message)"
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
        let data = try JSONEncoder().encode(draft)
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
    /// "kmail_ffi.KMailError.Store(message: …)" which is not
    /// user-presentable.
    func testKMailErrorLocalizedDescription() {
        let store = KMailError.Store(message: "schema migration failed")
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
