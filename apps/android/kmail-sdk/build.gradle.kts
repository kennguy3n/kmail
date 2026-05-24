// kmail-sdk module: Kotlin/Android library wrapping the Rust
// `kmail-ffi` crate via UniFFI-generated Kotlin bindings.
//
// Build inputs (produced by `sdk/scripts/build-android-aar.sh`):
//
//   src/main/jniLibs/{arm64-v8a,armeabi-v7a,x86_64,x86}/libkmail_ffi.so
//       Per-ABI cross-compiled staticlibs, packaged into the AAR
//       by AGP's `jniLibs` source-set convention.
//
//   src/main/kotlin/uniffi/kmail_ffi/kmail_ffi.kt
//       UniFFI-generated Kotlin binding source (package
//       `uniffi.kmail_ffi`, derived by uniffi-bindgen from the
//       FFI crate name). Imported by the facade at
//       `src/main/kotlin/com/kmail/sdk/KMail.kt` via the
//       `uniffi.kmail_ffi.*` package path.
//
//   build/host-jna/libkmail_ffi.so
//       Host-arch (Linux x86_64 on CI) .so used by the JVM-only
//       unit test runner via JNA. NOT packaged into the AAR.
//
// Output: an AAR + Maven POM at `build/outputs/aar/kmail-sdk-release.aar`
// publishable to a Maven repository (GitHub Packages on CI).

plugins {
    id("com.android.library")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.serialization")
    `maven-publish`
}

android {
    namespace = "com.kmail.sdk"

    // compileSdk 34 = Android 14. The compileSdk version is the
    // API surface the kotlin sources compile against; it is
    // independent of the minSdk (= runtime floor). Using the
    // latest stable compileSdk lets the SDK use newer Android
    // APIs guarded by `Build.VERSION.SDK_INT >= ...` checks.
    compileSdk = 34

    defaultConfig {
        // minSdk 26 = Android 8.0 Oreo (2017). This is the
        // KChat-wide floor (covers ~96% of active devices per
        // Google's distribution dashboard at the time of writing).
        // The Rust NDK build defaults to `ANDROID_API_LEVEL=26`
        // in `sdk/scripts/build-android-aar.sh` so the Rust
        // staticlib's symbol resolution matches.
        minSdk = 26
        // No targetSdk on a library module — that's an app-level
        // concern. Consumers of the AAR set their own targetSdk.

        // Limit the AAR to the four ABIs the Rust build produces.
        // If a consumer app declares additional ABIs, AGP will
        // automatically filter them out when merging the AAR's
        // jniLibs with the app's own native libs.
        ndk {
            abiFilters += listOf("arm64-v8a", "armeabi-v7a", "x86_64", "x86")
        }

        // JNA library name. uniffi-generated Kotlin calls
        // `Native.load("kmail_ffi", ...)` which JNA translates to
        // `libkmail_ffi.so` on Android. The .so files in
        // src/main/jniLibs/<abi>/ ship under the same name so
        // the runtime loader finds them automatically.
        consumerProguardFiles("consumer-rules.pro")
    }

    sourceSets {
        named("main") {
            // Pick up the uniffi-generated Kotlin source from
            // the `generated/` subdirectory. AGP scans the
            // default `kotlin/` source set, but listing it here
            // explicitly is a tripwire in case a future
            // refactor renames the directory.
            kotlin.srcDirs("src/main/kotlin")
            // jniLibs path is the AGP default but listed
            // explicitly for the same defence-in-depth reason.
            jniLibs.srcDirs("src/main/jniLibs")
        }
    }

    buildTypes {
        // Release: minify off (the .so is the bulk of the AAR
        // and ProGuard can't shrink it), R8 disabled (no
        // resources to shrink in a library AAR).
        getByName("release") {
            isMinifyEnabled = false
        }
        // Debug: identical to release for now; we may add
        // signing-config divergence later.
        getByName("debug") {
            isMinifyEnabled = false
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    testOptions {
        unitTests {
            // CRITICAL: this lets unit tests load JNA-backed
            // native libraries from the host JVM (linux-x86_64
            // libkmail_ffi.so produced by build-android-aar.sh).
            // Without this, the test classpath can't dlopen the
            // .so and JNA throws UnsatisfiedLinkError when it
            // tries to bind the uniffi-generated symbols.
            isReturnDefaultValues = false
            isIncludeAndroidResources = false

            all {
                // Point both `jna.library.path` AND
                // `java.library.path` at `build/host-jna`. JNA
                // consults its own property first (and falls
                // back to the JVM's `java.library.path`); we
                // set both so the .so resolves regardless of
                // how the test harness/IDE invokes the JVM.
                // The path is relative to the module directory;
                // `project.projectDir` resolves it
                // deterministically across CI and local runs.
                val hostJnaDir = "${project.projectDir}/build/host-jna"
                jvmArgs(
                    "-Djna.library.path=$hostJnaDir",
                    "-Djava.library.path=$hostJnaDir",
                )
            }
        }
    }

    publishing {
        singleVariant("release") {
            withSourcesJar()
            // No `withJavadocJar()` — Dokka generation is out
            // of scope for this PR; the public Kotlin API is
            // documented inline. A follow-up PR can add Dokka
            // if Maven Central publication needs it.
        }
    }
}

kotlin {
    // Match the AGP JDK target so the kotlin compiler emits
    // JVM 17 bytecode compatible with AGP 8's `coreLibraryDesugaring`
    // baseline. Modern Android devices (minSdk 26+) ship a
    // Java 8+ runtime; Kotlin 1.9 happily targets JVM 17 there.
    jvmToolchain(17)
}

dependencies {
    // JNA is the runtime that the uniffi-generated Kotlin uses
    // to bind into the .so file. UniFFI 0.28 generates code
    // against JNA 5.13+; we pin a slightly newer version that
    // ships pre-built ARM64 + x86_64 + armv7 dispatchers for
    // Android.
    implementation("net.java.dev.jna:jna:5.14.0@aar")

    // Kotlin coroutines for the `KMailClient` async surface.
    // The uniffi-generated code emits `suspend fun` for async
    // FFI methods; we add `kotlinx-coroutines-core` so consumers
    // can `runBlocking { client.sync() }` or use `launch`.
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-core:1.8.0")

    // kotlinx-serialization for `EmailDraft` / `SerializableEmailAddress`.
    // The wire JSON shape matches the Rust-side `EmailDraft` struct so the
    // Swift / Kotlin / Electron shells emit byte-identical `Email/set create`
    // payloads.
    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.6.3")

    // Unit-test dependencies. We use JUnit 4 because Android's
    // unit-test classpath is JVM-only and JUnit 4 has the
    // smallest dependency footprint. Coroutines test support
    // gives us `runTest { ... }` for testing suspend functions.
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.jetbrains.kotlinx:kotlinx-coroutines-test:1.8.0")
}

publishing {
    publications {
        register<MavenPublication>("release") {
            groupId = "com.kmail"
            artifactId = "sdk"
            // Version is tracked by the workspace `Cargo.toml`.
            // CI substitutes the real version from the published
            // tag; local builds default to `0.1.0-SNAPSHOT`.
            version = (project.findProperty("kmailSdkVersion") as String?) ?: "0.1.0-SNAPSHOT"

            afterEvaluate {
                from(components["release"])
            }

            pom {
                name.set("KMail Android SDK")
                description.set(
                    "Native Android SDK for KMail. Wraps the kmail-ffi " +
                        "Rust crate via UniFFI-generated Kotlin bindings. " +
                        "Provides JMAP client, offline sync, Zero-Access " +
                        "Vault decrypt, MLS bridge, and push token registration."
                )
                url.set("https://github.com/kennguy3n/kmail")
                licenses {
                    license {
                        name.set("AGPL-3.0-or-later")
                        url.set("https://www.gnu.org/licenses/agpl-3.0.txt")
                    }
                }
            }
        }
    }
    repositories {
        // GitHub Packages Maven repo. The actual URL + creds
        // come from CI environment variables; locally, `publish`
        // falls back to `mavenLocal()` via the auto-applied
        // `maven-publish` plugin.
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
