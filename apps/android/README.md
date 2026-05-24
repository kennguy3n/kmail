# KMail Android SDK

Kotlin/Android library that wraps the Rust `kmail-ffi` crate via
UniFFI-generated Kotlin bindings. Provides the same JMAP client,
offline sync, Zero-Access Vault decrypt, MLS bridge, and push
token registration surface as the iOS Swift Package at
`apps/ios/`.

This module produces a `kmail-sdk-release.aar` AAR plus a Maven
POM. Consuming Android apps (KChat for Android) depend on the
published `com.kmail:sdk` artifact.

## Layout

```
apps/android/
├── settings.gradle.kts           Root Gradle settings + module list
├── build.gradle.kts              Plugin version pinning
├── gradle.properties             AndroidX + JVM toolchain
├── kmail-sdk/
│   ├── build.gradle.kts          Android library module config
│   ├── consumer-rules.pro        ProGuard rules merged into consumers
│   └── src/
│       ├── main/
│       │   ├── AndroidManifest.xml
│       │   ├── kotlin/
│       │   │   ├── com/kmail/sdk/
│       │   │   │   ├── KMail.kt              Facade (typealiases + KMailClient)
│       │   │   │   └── MlsKeyProvider.kt     Lambda adapter
│       │   │   └── uniffi/kmail_ffi/         UniFFI output (gitignored)
│       │   │       └── kmail_ffi.kt          Package: uniffi.kmail_ffi
│       │   └── jniLibs/                 Per-ABI .so files (gitignored)
│       │       ├── arm64-v8a/libkmail_ffi.so
│       │       ├── armeabi-v7a/libkmail_ffi.so
│       │       ├── x86_64/libkmail_ffi.so
│       │       └── x86/libkmail_ffi.so
│       └── test/
│           └── kotlin/com/kmail/sdk/
│               └── KMailIntegrationTests.kt   JNA-backed JVM tests
└── .gitignore
```

The `uniffi/kmail_ffi/` Kotlin sources and `jniLibs/` .so files are
populated by `sdk/scripts/build-android-aar.sh`. Both are
gitignored to keep build artefacts out of the repo. The
`uniffi/` directory name is set by uniffi-bindgen itself based on
the FFI crate name — the facade imports from `uniffi.kmail_ffi.*`
to match.

## Build prerequisites

- **Rust toolchain** (1.78+) via rustup
- **cargo-ndk** — `cargo install cargo-ndk`
- **Android NDK r25c+** — set `ANDROID_NDK_HOME` to the install root
- **JDK 17** — AGP 8 requires Java 17
- **Gradle 8.0+** (the wrapper resolves this; running `gradle`
  directly requires a host install)

## Local build

```bash
# 1. Cross-compile the four Android targets + generate Kotlin
#    bindings + stage the host JNA .so for unit tests.
./sdk/scripts/build-android-aar.sh

# 2. Assemble the release AAR.
cd apps/android
gradle :kmail-sdk:assembleRelease

# 3. Run the host-JVM unit tests against the linux-x86_64 .so.
gradle :kmail-sdk:test
```

The assembled AAR lands at
`apps/android/kmail-sdk/build/outputs/aar/kmail-sdk-release.aar`.

## CI

`.github/workflows/sdk-build-android.yml` runs the full build on
every PR that touches `sdk/**`, `apps/android/**`, or the build
script. It uses `ubuntu-latest` (NDK + JDK install via setup
actions) and skips the macOS-only iOS pipeline that lives in
`sdk-build-ios.yml`.

## Integration

Consumer apps depend on the AAR via GitHub Packages:

```kotlin
// settings.gradle.kts
dependencyResolutionManagement {
    repositories {
        maven {
            name = "GitHubPackages"
            url = uri("https://maven.pkg.github.com/kennguy3n/kmail")
            credentials {
                username = System.getenv("GITHUB_ACTOR")
                password = System.getenv("GITHUB_TOKEN")
            }
        }
    }
}

// app/build.gradle.kts
dependencies {
    implementation("com.kmail:sdk:0.1.0")
}
```

The host app must declare:

```xml
<uses-permission android:name="android.permission.INTERNET" />
<uses-permission android:name="android.permission.ACCESS_NETWORK_STATE" />
```

## Sample usage

```kotlin
import com.kmail.sdk.ClientConfiguration
import com.kmail.sdk.KMailClient
import com.kmail.sdk.EmailDraft
import com.kmail.sdk.SerializableEmailAddress

val client = KMailClient(
    ClientConfiguration(
        bffUrl = "https://kmail.example.com",
        bearerToken = oidcAccessToken,
        databasePath = File(filesDir, "kmail.sqlite").absolutePath,
    )
)

// Delta-pull sync.
val summary = client.sync()

// Send an email.
val id = client.sendEmail(
    EmailDraft(
        mailboxIds = mapOf("mb-drafts" to true),
        from = listOf(SerializableEmailAddress(email = "me@example.com")),
        to = listOf(SerializableEmailAddress(email = "you@example.com")),
        subject = "Hello",
        textBody = "From the KMail Android SDK.",
    )
)
```

## Cross-binding parity

Drift between Swift / Kotlin / Electron defaults is
architecturally prevented by sourcing every default from the FFI
helper `defaultClientConfig(...)`. See:

- Kotlin side: `KMail.kt::ClientConfiguration.Companion.sdkDefaults`
- Swift side: `KMail.swift::ClientConfiguration.sdkDefaults`
- Rust source-of-truth: `sdk/kmail-core/src/client.rs::ClientConfig::new`
- Locked-down test: `KMailIntegrationTests.kt::kotlinDefaultsMatchRustDefaults`
- Cross-FFI test: `sdk/kmail-ffi/src/lib.rs::client_open_matches_napi_lowering_for_string_tier`
