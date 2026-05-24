// Root build.gradle.kts for the KMail Android shell.
//
// All real configuration lives in the module-level
// `kmail-sdk/build.gradle.kts`. This file exists to declare the
// AGP + Kotlin plugin versions in a single place so every module
// resolves the same plugin coordinates.
//
// We use the modern `plugins {}` DSL with `apply false` at the
// root so plugins are downloaded once and applied selectively per
// module — the AGP 7.0+ recommended pattern.

plugins {
    // Android Gradle Plugin. 8.x requires Gradle 8.0+ and JDK 17.
    // 8.5.x is the current LTS at the time of writing (May 2026)
    // and is what the CI runner ships via setup-android-gradle.
    id("com.android.library") version "8.5.0" apply false
    // Kotlin / Android. 1.9.24 matches AGP 8.5's compatibility
    // matrix and is what uniffi 0.28's generated Kotlin requires.
    id("org.jetbrains.kotlin.android") version "1.9.24" apply false
    // kotlinx-serialization plugin. Generates the encoder/decoder
    // pairs for `@Serializable` data classes (only `EmailDraft` /
    // `SerializableEmailAddress` use this in the SDK, but the
    // plugin must be applied at the module level).
    id("org.jetbrains.kotlin.plugin.serialization") version "1.9.24" apply false
}
