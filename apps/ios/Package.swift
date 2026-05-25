// swift-tools-version:5.9
//
// Swift Package Manager manifest for the KMail SDK iOS shell.
//
// The package exposes a single top-level `KMail` library that
// downstream apps depend on via:
//
//     dependencies: [
//         .package(path: "path/to/kmail/apps/ios")
//     ],
//     targets: [
//         .target(name: "MyApp", dependencies: [
//             .product(name: "KMail", package: "ios")
//         ])
//     ]
//
// `KMail` is a regular Swift target that re-exports the
// uniffi-generated bindings (`KMailFFI.swift`, sitting in
// `Sources/KMail/Generated/`) plus the hand-written Swift facade
// (`Sources/KMail/KMail.swift`). It depends on a binary target
// `KMailFFI` whose `path` points at the XCFramework that
// `sdk/scripts/build-ios-xcframework.sh` produces.
//
// Both the generated Swift source AND the XCFramework are
// gitignored — every CI run produces a fresh copy from the Rust
// source. Downstream consumers MUST run the build script before
// `swift build` / `swift test`.

import PackageDescription

let package = Package(
    name: "KMail",
    platforms: [
        // iOS 16 is the floor matching the rest of KChat's mobile
        // surface (Face ID Biometric API stability, WidgetKit
        // interactive widgets, MLS 1.0 support in CryptoKit).
        // macCatalyst is included so the iPad-style Mac app target
        // can consume the SDK without spawning a second build of
        // the Electron desktop binary.
        .iOS(.v16),
        .macCatalyst(.v16),
    ],
    products: [
        .library(
            name: "KMail",
            targets: ["KMail"]
        ),
    ],
    targets: [
        // Vendored XCFramework. The path is relative to
        // Package.swift, so `Frameworks/KMailFFI.xcframework` is
        // `apps/ios/Frameworks/KMailFFI.xcframework`.
        //
        // The framework is NOT checked into git; it lives in the
        // `apps/ios/Frameworks/.gitignore` ignore-block and must
        // be built by `sdk/scripts/build-ios-xcframework.sh`
        // before any `swift build` / `swift test` invocation.
        // CI does this as the first step of the sdk-build-ios
        // workflow.
        .binaryTarget(
            name: "KMailFFI",
            path: "Frameworks/KMailFFI.xcframework"
        ),
        // Public Swift facade. Depends on the binary target so
        // the linker can resolve the FFI symbols at link time
        // (Xcode handles the lipo / arch-slice selection
        // automatically based on the XCFramework's Info.plist).
        .target(
            name: "KMail",
            dependencies: ["KMailFFI"],
            path: "Sources/KMail",
            // `Generated/` is excluded from the source-tree
            // glob via the .gitignore, but Swift Package
            // Manager picks up everything under `path` by
            // default. We don't need to opt out because the
            // generated file IS a valid Swift source we want
            // SPM to compile.
            exclude: []
        ),
        .testTarget(
            name: "KMailTests",
            dependencies: ["KMail"],
            path: "Tests/KMailTests"
        ),
    ]
)
