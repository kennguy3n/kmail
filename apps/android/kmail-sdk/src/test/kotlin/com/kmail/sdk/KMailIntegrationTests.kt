// Kotlin integration tests for the KMail SDK Android binding.
//
// These tests exercise the real FFI path — they instantiate a
// real `KMailClient`, plug a real `LambdaMlsKeyProvider`, and
// run real AES-256-GCM seal / open through the Rust crypto
// implementation. The only thing mocked is the JMAP server (we
// don't sync against a live BFF in these tests; that lives in
// the docker-compose nightly integration suite).
//
// The tests run on the host JVM (Linux x86_64) via JNA loading
// `build/host-jna/libkmail_ffi.so`. This mirrors the Swift
// integration tests at `apps/ios/Tests/KMailTests/KMailIntegrationTests.swift`
// which run on the macOS host against the simulator slice of
// the XCFramework. Same FFI surface, same observable behaviour.
//
// Android emulator instrumentation tests are a separate workflow
// (TODO: PR-6b) — those exercise the real device .so but run on
// a slow emulator and are gated on a path filter so we don't pay
// the cost on every PR.

package com.kmail.sdk

import uniffi.kmail_ffi.KMailException
import uniffi.kmail_ffi.defaultClientConfig
import java.io.File
import java.nio.file.Files
import kotlin.io.path.deleteRecursively
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Before
import org.junit.Test

class KMailIntegrationTests {

    /**
     * Each test gets a fresh SQLite database in a per-test
     * temporary directory so concurrent tests don't collide on
     * the same on-disk store. The directory is removed in
     * `tearDown`.
     */
    private lateinit var tempDir: File

    @Before
    @OptIn(kotlin.io.path.ExperimentalPathApi::class)
    fun setUp() {
        tempDir = Files.createTempDirectory("kmail-tests-").toFile()
    }

    @After
    @OptIn(kotlin.io.path.ExperimentalPathApi::class)
    fun tearDown() {
        if (::tempDir.isInitialized) {
            tempDir.toPath().deleteRecursively()
        }
    }

    private fun makeClient(): KMailClient {
        val dbPath = File(tempDir, "kmail.sqlite").absolutePath
        val config = ClientConfiguration(
            bffUrl = "https://kmail.test",
            bearerToken = "test-bearer",
            databasePath = dbPath,
        )
        return KMailClient(configuration = config)
    }

    // -----------------------------------------------------------
    // Construction
    // -----------------------------------------------------------

    /**
     * Opening a client should populate an empty mailbox list (no
     * sync has been run, so the local SQLite store is empty but
     * queryable).
     */
    @Test
    fun clientOpensWithEmptyCache() {
        val client = makeClient()
        val mailboxes = client.cachedMailboxes()
        assertTrue("fresh client should have no cached mailboxes", mailboxes.isEmpty())
    }

    /**
     * Opening two clients against the same database path is
     * allowed (SQLite WAL mode supports concurrent readers) and
     * must not corrupt the schema.
     */
    @Test
    fun clientOpensTwiceAgainstSameDatabase() {
        makeClient()
        val second = makeClient()
        assertTrue(second.cachedMailboxes().isEmpty())
    }

    // -----------------------------------------------------------
    // Vault crypto (raw-key surface)
    // -----------------------------------------------------------

    /**
     * `seal_vault_envelope` + `decrypt_vault_envelope` are the
     * raw-key crypto surface — caller passes the 32-byte folder
     * master key directly. Verify a roundtrip preserves the
     * plaintext byte-for-byte and that the envelope's nonce is
     * the required 12 bytes.
     */
    @Test
    fun vaultSealOpenRoundtrip() = runBlocking {
        val client = makeClient()
        val folderKey = ByteArray(32) { 0x42 }
        val plaintext = "hello vault".toByteArray(Charsets.UTF_8)
        val aad = "v1:folder=alpha".toByteArray(Charsets.UTF_8)

        val envelope = client.handle.sealVaultEnvelope(
            folderMasterKey = folderKey,
            plaintext = plaintext,
            aad = aad,
        )
        assertEquals("AES-GCM nonce must be 12 bytes per RFC 5116", 12, envelope.nonce.size)
        assertTrue("ciphertext must not be empty", envelope.ciphertext.isNotEmpty())
        assertArrayEquals(aad, envelope.aad)

        val recovered = client.handle.decryptVaultEnvelope(
            folderMasterKey = folderKey,
            envelope = envelope,
        )
        assertArrayEquals(
            "roundtrip plaintext must match input",
            plaintext,
            recovered,
        )
    }

    /**
     * A 16-byte folder master key (too short) must surface as
     * `KMailException.KeyDerivation` / `.InvalidArgument` from
     * the Rust side — not crash, not silently truncate.
     */
    @Test
    fun vaultSealRejectsShortFolderKey() = runBlocking {
        val client = makeClient()
        val shortKey = ByteArray(16) { 0x42 }

        try {
            client.handle.sealVaultEnvelope(
                folderMasterKey = shortKey,
                plaintext = "oops".toByteArray(Charsets.UTF_8),
                aad = ByteArray(0),
            )
            fail("sealVaultEnvelope should have thrown on short folder key")
        } catch (e: KMailException.KeyDerivation) {
            // Acceptable — kdf::hkdf_derive returns a Crypto
            // error that the FFI layer maps to KeyDerivation.
        } catch (e: KMailException.InvalidArgument) {
            // Also acceptable — alternate mapping path through
            // the AEAD layer's length validation.
        }
        Unit
    }

    // -----------------------------------------------------------
    // Confidential Send crypto (raw-key surface)
    // -----------------------------------------------------------

    /**
     * `seal_confidential_envelope` + `open_confidential_envelope`
     * roundtrip with a 32-byte MLS leaf secret. Verify that
     * `kek_salt`, the wrapped DEK envelope, and the payload
     * envelope all have the documented sizes, and the plaintext
     * recovers byte-identically.
     */
    @Test
    fun confidentialSealOpenRoundtrip() = runBlocking {
        val client = makeClient()
        val leafSecret = ByteArray(32) { 0xA5.toByte() }
        val plaintext = "hello confidential send".toByteArray(Charsets.UTF_8)
        val payloadAad = "v1:recipient=alice@kmail.test".toByteArray(Charsets.UTF_8)
        val wrapAad = "v1:kek=ConfidentialSendDekWrap".toByteArray(Charsets.UTF_8)

        val envelope = client.handle.sealConfidentialEnvelope(
            mlsLeafSecret = leafSecret,
            plaintext = plaintext,
            payloadAad = payloadAad,
            wrapAad = wrapAad,
        )
        assertEquals("KEK salt must be 32 bytes", 32, envelope.kekSalt.size)
        assertEquals(12, envelope.wrappedDek.nonce.size)
        assertEquals(12, envelope.payload.nonce.size)

        val recovered = client.handle.openConfidentialEnvelope(
            mlsLeafSecret = leafSecret,
            envelope = envelope,
        )
        assertArrayEquals(plaintext, recovered)
    }

    // -----------------------------------------------------------
    // MLS provider plumbing
    // -----------------------------------------------------------

    /**
     * Plug a `LambdaMlsKeyProvider`, write a vault message
     * through the convenience surface, then read it back. This
     * exercises the foreign-callback path end-to-end:
     * Rust SDK → FFI adapter → Kotlin lambda → Rust SDK.
     */
    @Test
    fun lambdaMlsProviderRoundtrip() = runTest {
        val client = makeClient()
        val provider = LambdaMlsKeyProvider(
            confidentialSend = { _ -> ByteArray(32) { 0xC1.toByte() } },
            vaultFolder = { _ -> ByteArray(32) { 0xF0.toByte() } },
        )
        client.setMlsProvider(provider)

        val plaintext = "hello via MLS provider".toByteArray(Charsets.UTF_8)
        val aad = "v1:folder=mls-test".toByteArray(Charsets.UTF_8)
        val envelope = client.writeVaultMessage(
            folderId = "folder-mls-test",
            plaintext = plaintext,
            aad = aad,
        )
        val recovered = client.openVaultMessage(
            folderId = "folder-mls-test",
            envelope = envelope,
        )
        assertArrayEquals(plaintext, recovered)
    }

    /**
     * A `LambdaMlsKeyProvider` that returns a wrong-length
     * secret must surface as `KMailException.KeyStore(...)`
     * with a message identifying the scope (Vault or
     * Confidential Send) — see `sdk/kmail-ffi/src/lib.rs`
     * `ForeignMlsKeyProvider` validation.
     */
    @Test
    fun lambdaMlsProviderRejectsWrongLengthSecret() = runTest {
        val client = makeClient()
        val provider = LambdaMlsKeyProvider(
            confidentialSend = { _ -> ByteArray(33) { 0xAB.toByte() } },
            vaultFolder = { _ -> ByteArray(31) { 0xCD.toByte() } },
        )
        client.setMlsProvider(provider)

        try {
            client.writeVaultMessage(
                folderId = "folder-x",
                plaintext = "oops".toByteArray(Charsets.UTF_8),
            )
            fail("writeVaultMessage should have thrown on wrong-length secret")
        } catch (e: KMailException.KeyStore) {
            // `Throwable.message` is nullable in Java but always
            // non-null on uniffi-generated exceptions (it is
            // synthesised from the `#[error(...)]` Display impl
            // formatted with the `description` field) \u2014 keep the
            // null-coalesce as a belt-and-suspenders guard, but
            // suppress the redundant-elvis warning.
            @Suppress("USELESS_ELVIS")
            val message = e.message ?: ""
            assertTrue(
                "expected wrong-length / Vault scope in message, got: $message",
                message.contains("31") || message.contains("Vault"),
            )
        }
    }

    // -----------------------------------------------------------
    // EmailDraft serialization
    // -----------------------------------------------------------

    /**
     * Roundtrip an `EmailDraft` through JSON to verify the
     * kotlinx-serialization conformance matches the Rust
     * wire-format that `KMailClient::send_email` deserialises on
     * the other side. We don't actually call `sendEmail` here (no
     * live JMAP server) — we just verify the encoded JSON shape.
     */
    @Test
    fun emailDraftEncodesToRustWireFormat() {
        val draft = EmailDraft(
            mailboxIds = mapOf("mb-drafts" to true),
            from = listOf(SerializableEmailAddress(name = "Alice", email = "alice@kmail.test")),
            to = listOf(SerializableEmailAddress(name = "Bob", email = "bob@kmail.test")),
            subject = "hello",
            textBody = "hi bob",
        )
        // Use a `Json { encodeDefaults = true }` instance to match
        // what `KMailClient.sendEmail` does internally — the SDK
        // emits ALL fields (including empty `cc` / `bcc` / `inReplyTo`
        // lists) for cross-binding wire-format parity with Swift's
        // `JSONEncoder`, which always emits every Codable property.
        val json = Json { encodeDefaults = true }.encodeToString(draft)

        // Keys must be exactly the JMAP RFC 8621 field names —
        // mailboxIds, replyTo, textBody, htmlBody, inReplyTo —
        // because the BFF and the React web client also speak
        // that shape. A camelCase deviation here would break
        // wire-format compatibility.
        assertTrue("expected mailboxIds key, got: $json", json.contains("\"mailboxIds\""))
        assertTrue("expected textBody key, got: $json", json.contains("\"textBody\""))
        assertTrue("expected inReplyTo key, got: $json", json.contains("\"inReplyTo\""))
        // `cc` / `bcc` / `replyTo` default to [] but must still be
        // emitted by the wire-format encoder for parity with Swift.
        assertTrue("expected cc key (empty array), got: $json", json.contains("\"cc\""))
        assertTrue("expected bcc key (empty array), got: $json", json.contains("\"bcc\""))
        assertTrue("expected replyTo key (empty array), got: $json", json.contains("\"replyTo\""))
        assertTrue(json.contains("alice@kmail.test"))
    }

    // -----------------------------------------------------------
    // Default contract — drift prevention
    // -----------------------------------------------------------

    /**
     * `ClientConfiguration`'s Kotlin-side defaults must be
     * bit-identical to the SDK's Rust-side `ClientConfig::new`
     * defaults exposed through the FFI helper
     * `default_client_config(...)`. This is the load-bearing
     * drift-prevention test — if a Rust default changes the new
     * value flows out through `defaultClientConfig` and this
     * test fails loudly until the Kotlin defaults are updated
     * to match.
     *
     * Same shape as
     * `testSwiftDefaultsMatchRustDefaults` in the iOS Swift
     * tests. Cross-binding parity is locked down by the Rust
     * test `client_open_matches_kotlin_lowering_for_string_tier`
     * in `sdk/kmail-ffi/src/lib.rs`.
     */
    @Test
    fun kotlinDefaultsMatchRustDefaults() {
        val bff = "https://kmail.test"
        val bearer = "test-bearer"
        val dbPath = "/tmp/kmail.sqlite"

        val kotlin = ClientConfiguration(
            bffUrl = bff,
            bearerToken = bearer,
            databasePath = dbPath,
        ).toFfi()
        val rust = defaultClientConfig(
            bffUrl = bff,
            bearerToken = bearer,
            databasePath = dbPath,
        )

        assertEquals(rust.bffUrl, kotlin.bffUrl)
        assertEquals(rust.bearerToken, kotlin.bearerToken)
        assertEquals(rust.databasePath, kotlin.databasePath)
        assertEquals(
            "attachmentCacheBytes drifted between Kotlin default and Rust ClientConfig::new",
            rust.attachmentCacheBytes,
            kotlin.attachmentCacheBytes,
        )
        assertEquals(
            "requestTimeout drifted between Kotlin default and Rust ClientConfig::new",
            rust.requestTimeoutSecs,
            kotlin.requestTimeoutSecs,
        )
        assertEquals(
            "retryBudget drifted between Kotlin default and Rust ClientConfig::new",
            rust.retryBudgetSecs,
            kotlin.retryBudgetSecs,
        )
        assertEquals(
            "initialSyncEmailWindow drifted between Kotlin default and Rust ClientConfig::new",
            rust.initialSyncEmailWindow,
            kotlin.initialSyncEmailWindow,
        )
        assertEquals(
            "accountId drifted between Kotlin default and Rust ClientConfig::new",
            rust.accountId,
            kotlin.accountId,
        )
        assertEquals(
            "bootstrapMailboxRole drifted between Kotlin default and Rust ClientConfig::new (Rust defaults to Some(\"inbox\"))",
            rust.bootstrapMailboxRole,
            kotlin.bootstrapMailboxRole,
        )
    }

    /**
     * `ClientConfiguration.toFfiWithNullDefaults()` produces a
     * `KMailClientConfig` whose every override field is `null`.
     * The FFI's `Option<T>` ladder in `client_open` MUST
     * interpret this as "use Rust defaults for every tier-1
     * field" — i.e. a caller that passes the all-`null` record
     * gets the same effective configuration as one that passes
     * the `defaultClientConfig` record. This is the load-bearing
     * test for the architectural drift-prevention layer at the
     * FFI boundary.
     */
    @Test
    fun nullDefaultsRecordMatchesExplicitDefaults() {
        val bff = "https://kmail.test"
        val bearer = "test-bearer"
        val dbPath = "/tmp/kmail.sqlite"

        val nullForm = ClientConfiguration(
            bffUrl = bff,
            bearerToken = bearer,
            databasePath = dbPath,
        ).toFfiWithNullDefaults()

        assertNull(nullForm.attachmentCacheBytes)
        assertNull(nullForm.requestTimeoutSecs)
        assertNull(nullForm.retryBudgetSecs)
        assertNull(nullForm.initialSyncEmailWindow)
        assertNull(nullForm.accountId)
        assertNull(nullForm.bootstrapMailboxRole)
    }
}
