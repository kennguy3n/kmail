// Root Gradle settings for the KMail Android SDK shell.
//
// This project hosts the Kotlin library wrapper around the Rust
// `kmail-ffi` crate (compiled to per-ABI .so files and packaged
// into an AAR). Consumers depend on the published `com.kmail:sdk`
// Maven artifact (see `kmail-sdk/build.gradle.kts` for the
// publishing block).
//
// The project is intentionally minimal: one library module
// (`kmail-sdk`). There is no sample app module here — sample
// integration code lives in the docs, not in a separate Gradle
// module, to avoid the dependency-resolution overhead of a
// second module on every CI run.

pluginManagement {
    repositories {
        google {
            // Restrict the google() repo to AGP / kotlin-android
            // / lint coordinates only. This is the canonical
            // pattern from the Android Gradle Plugin docs and
            // prevents accidental resolution of non-Android
            // artifacts from Google's Maven mirror.
            content {
                includeGroupByRegex("com\\.android.*")
                includeGroupByRegex("com\\.google.*")
                includeGroupByRegex("androidx.*")
            }
        }
        mavenCentral()
        gradlePluginPortal()
    }
}

dependencyResolutionManagement {
    // FAIL_ON_PROJECT_REPOS rejects any `repositories {}` block
    // declared inside a module's build.gradle.kts. All repository
    // declarations MUST live in this file, which is the AGP
    // 7.0+ recommended pattern (single source of truth for
    // dependency provenance, and easier supply-chain audit).
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google {
            content {
                includeGroupByRegex("com\\.android.*")
                includeGroupByRegex("com\\.google.*")
                includeGroupByRegex("androidx.*")
            }
        }
        mavenCentral()
    }
}

rootProject.name = "kmail-android"
include(":kmail-sdk")
