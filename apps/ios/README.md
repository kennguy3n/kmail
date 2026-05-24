# KMail iOS Swift Package

Swift Package Manager package wrapping the KMail SDK for iOS / iPadOS / Mac Catalyst consumers.

## What's in this package

| Module | Path | Description |
|---|---|---|
| `KMail` | `Sources/KMail/` | Public Swift API — `KMailClient`, `EmailDraft`, `Mailbox`, `EmailSummary`, MLS provider helpers, and `LocalizedError` conformance for `KMailError`. |
| `KMail/Generated/` | `Sources/KMail/Generated/KMailFFI.swift` | Auto-generated UniFFI Swift bindings. **Not checked into git.** Built by `sdk/scripts/build-ios-xcframework.sh`. |
| `KMailFFI` | `Frameworks/KMailFFI.xcframework/` | Vendored XCFramework containing the `kmail-ffi` Rust staticlib for `ios-arm64`, `ios-arm64-simulator`, and `ios-x86_64-simulator`. **Not checked into git.** Built by the same script. |
| Tests | `Tests/KMailTests/` | Swift integration tests exercising the real FFI roundtrip (vault seal/open, Confidential Send seal/open, MLS provider plumbing, `EmailDraft` JSON wire-format). |

## Building the XCFramework

Both the XCFramework and the generated Swift bindings must be produced before `swift build` / `swift test` can succeed. Run:

```bash
cd path/to/kmail
./sdk/scripts/build-ios-xcframework.sh
```

The script:

1. Installs the three required `rustup` targets (`aarch64-apple-ios`, `aarch64-apple-ios-sim`, `x86_64-apple-ios`).
2. Builds the host cdylib (with debug symbols preserved) and runs `uniffi-bindgen` against it to emit the Swift bindings.
3. Builds the three iOS staticlibs with the `release-with-symbols` Cargo profile.
4. `lipo`s the two simulator slices into a single fat staticlib.
5. Assembles the XCFramework via `xcodebuild -create-xcframework`.

Output:

- `apps/ios/Frameworks/KMailFFI.xcframework`
- `apps/ios/Sources/KMail/Generated/KMailFFI.swift`

The script is **macOS-only** (Linux lacks `lipo` and `xcodebuild`). CI runs it on a `macos-14` runner.

## Adding to an iOS app

In your app's `Package.swift`:

```swift
dependencies: [
    .package(path: "../kmail/apps/ios")
],
targets: [
    .executableTarget(
        name: "MyKMailApp",
        dependencies: [.product(name: "KMail", package: "ios")]
    )
]
```

Or in Xcode: File → Add Package Dependencies → Add Local Package → select `apps/ios/`.

## Minimum versions

| Component | Version |
|---|---|
| iOS / iPadOS | 16.0 |
| Mac Catalyst | 16.0 |
| Swift | 5.9 |
| Xcode | 15.0 |
| Rust toolchain | 1.78 |

## Usage example

```swift
import KMail

let client = try KMailClient(configuration: ClientConfiguration(
    bffURL: URL(string: "https://api.kmail.example.com")!,
    bearerToken: oidcAccessToken,
    databaseURL: FileManager.default
        .urls(for: .documentDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("kmail.sqlite")
))

// Plug the MLS exporter-secret provider (production shells wrap
// the KChat MLS SDK; tests use ClosureMlsKeyProvider).
try await client.setMLSProvider(ClosureMlsKeyProvider(
    confidentialSend: { recipientUserID in
        try mlsSDK.exportConfidentialSendLeafSecret(for: recipientUserID)
    },
    vaultFolder: { folderID in
        try mlsSDK.exportVaultFolderMasterSecret(for: folderID)
    }
))

// Delta-pull JMAP state.
let summary = try await client.sync()
print("synced \(summary.emailsCreated) new emails")

// Read cached state without hitting the network.
let inbox = try client.cachedMailboxes().first(where: { $0.role == "inbox" })!
let recent = try client.cachedEmails(in: inbox.id, limit: 50)

// Send an email.
let draft = EmailDraft(
    mailboxIds: [inbox.id: true],
    from: [EmailAddress(name: "Alice", email: "alice@example.com")],
    to: [EmailAddress(name: "Bob", email: "bob@example.com")],
    subject: "hello",
    textBody: "hi bob"
)
let emailID = try await client.sendEmail(draft)

// Decrypt a vault message.
let plaintext = try await client.openVaultMessage(
    folderID: "vault-2024",
    envelope: vaultEnvelopeFromJMAP
)
```

## Error handling

`KMailError` conforms to `LocalizedError` — `.localizedDescription` produces a user-presentable string for every variant. Switch on the enum case when you need to react programmatically (e.g. `.RateLimit(let retryAfterSeconds)` to schedule a retry).

## Threading

`KMailClient` is internally `Send + Sync` (it forwards to a `Send + Sync` Rust struct). You can call any method from any thread. Async methods bounce through a dedicated Tokio runtime owned by the FFI layer — you don't need to manage a runtime yourself.

## Architecture notes

See `docs/SDK.md` for the full SDK architecture, including the JMAP transport, sync engine, and crypto module design.
