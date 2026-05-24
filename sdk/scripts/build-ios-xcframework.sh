#!/usr/bin/env bash
#
# Build the KMailFFI.xcframework consumed by the Swift Package at
# `apps/ios/`. This script is the source of truth for the iOS
# binary build pipeline — both local developers and CI run it.
#
# Outputs (relative to the repo root):
#
#   apps/ios/Frameworks/KMailFFI.xcframework/
#       Three-slice XCFramework:
#         - ios-arm64                  (real device, aarch64-apple-ios)
#         - ios-arm64-simulator        (Apple Silicon sim,
#                                       aarch64-apple-ios-sim)
#         - ios-x86_64-simulator       (Intel Mac sim,
#                                       x86_64-apple-ios)
#       The two simulator slices are lipo'd into a single fat
#       staticlib so the XCFramework only has to declare one
#       simulator variant.
#
#   apps/ios/Sources/KMail/Generated/KMailFFI.swift
#       The Swift binding source emitted by `uniffi-bindgen`. The
#       Swift Package picks this up as a regular source file in
#       the `KMail` target and re-exports it through the facade.
#
# Requirements (macOS host only):
#   - Xcode 15+ (provides xcodebuild, lipo, ld)
#   - Rust toolchain (1.78+) via rustup
#   - cargo
#
# This script is idempotent: re-running it tears down and rebuilds
# the XCFramework + generated sources, so a stale binding can
# never sneak into a Swift build.

set -euo pipefail

# Resolve the repo root regardless of where the script is invoked
# from. The `realpath` indirection handles symlinked checkouts.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SDK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${SDK_DIR}/.." && pwd)"

# Target list pinned in one place so the rustup-target install loop
# below cannot drift from the xcframework assembly loop further
# down. Order matters: the host-cdylib build for bindgen runs
# first, then each iOS target, then the lipo + xcframework steps.
declare -ra IOS_TARGETS=(
    "aarch64-apple-ios"
    "aarch64-apple-ios-sim"
    "x86_64-apple-ios"
)

# Custom profile defined in sdk/Cargo.toml. MUST retain symbols
# for both bindgen metadata extraction AND iOS staticlib linking.
# See the `[profile.release-with-symbols]` comment block in
# sdk/Cargo.toml for the full reasoning.
readonly PROFILE="release-with-symbols"

# Output paths, all relative to the repo root so Xcode's
# `apps/ios/Package.swift` can reference them deterministically.
readonly FRAMEWORK_OUT="${REPO_ROOT}/apps/ios/Frameworks/KMailFFI.xcframework"
readonly BINDINGS_OUT="${REPO_ROOT}/apps/ios/Sources/KMail/Generated"
readonly STAGING_DIR="${SDK_DIR}/target/ios-staging"

# Sanity-check the host. The script is hard macOS-only because the
# `lipo` and `xcodebuild -create-xcframework` steps don't exist on
# Linux. We fail loudly rather than silently producing a broken
# artifact.
if [[ "$(uname -s)" != "Darwin" ]]; then
    echo "error: build-ios-xcframework.sh requires macOS (this host is $(uname -s))" >&2
    echo "error: the iOS staticlib build needs lipo + xcodebuild, which are Apple-toolchain-only." >&2
    exit 2
fi

if ! command -v xcodebuild >/dev/null 2>&1; then
    echo "error: xcodebuild not found on PATH (install Xcode + Command Line Tools)" >&2
    exit 2
fi

if ! command -v lipo >/dev/null 2>&1; then
    echo "error: lipo not found on PATH (ships with Xcode Command Line Tools)" >&2
    exit 2
fi

if ! command -v cargo >/dev/null 2>&1; then
    echo "error: cargo not found on PATH (install via rustup)" >&2
    exit 2
fi

echo "==> Installing rustup iOS targets"
for target in "${IOS_TARGETS[@]}"; do
    # `rustup target add` is idempotent and re-uses cached
    # toolchain artifacts when the target is already installed.
    rustup target add "${target}"
done

echo "==> Cleaning previous artifacts"
rm -rf "${FRAMEWORK_OUT}" "${BINDINGS_OUT}" "${STAGING_DIR}"
mkdir -p "${BINDINGS_OUT}" "${STAGING_DIR}"

echo "==> Building host cdylib for binding extraction"
# We build the host cdylib so `uniffi-bindgen` can read the
# `UNIFFI_META_*` symbols out of it. The cdylib we produce here
# is throwaway — never shipped — but it MUST have symbols, hence
# the release-with-symbols profile.
cd "${SDK_DIR}"
cargo build --profile "${PROFILE}" -p kmail-ffi
cargo build --profile "${PROFILE}" -p uniffi-bindgen

# `uniffi-bindgen generate --library` reads the binding ABI out
# of the cdylib and emits a single .swift file plus a C header
# and a modulemap. We then re-arrange them: the .swift file goes
# into the Swift Package source tree, the header + modulemap go
# inside each XCFramework slice's `Headers/` directory.
echo "==> Generating Swift bindings from host cdylib"
HOST_CDYLIB="${SDK_DIR}/target/${PROFILE}/libkmail_ffi.dylib"
if [[ ! -f "${HOST_CDYLIB}" ]]; then
    # rustc on macOS produces `.dylib`; on Linux it would be
    # `.so`. We hard-coded `.dylib` because this script is
    # macOS-only (see the uname -s check above).
    echo "error: expected host cdylib at ${HOST_CDYLIB} but it does not exist" >&2
    exit 1
fi

# Generate into a scratch directory first, then move the pieces
# to their final homes. This avoids the failure mode where a
# partial generation leaves stale files in the Swift Package and
# the next swift build picks up a mismatched header / .swift
# pair.
GEN_TMP="${STAGING_DIR}/bindgen-out"
rm -rf "${GEN_TMP}"
mkdir -p "${GEN_TMP}"
"${SDK_DIR}/target/${PROFILE}/uniffi-bindgen" \
    generate \
    --library "${HOST_CDYLIB}" \
    --language swift \
    --out-dir "${GEN_TMP}"

# Sanity: uniffi-bindgen emits these three files (the .swift name
# is derived from the crate name + "FFI" suffix for the header /
# modulemap). If the names ever change in a uniffi-rs minor
# upgrade we want a loud failure here, not a silently empty
# XCFramework two steps down.
SWIFT_SRC="${GEN_TMP}/kmail_ffi.swift"
C_HEADER="${GEN_TMP}/kmail_ffiFFI.h"
MODULEMAP="${GEN_TMP}/kmail_ffiFFI.modulemap"
for f in "${SWIFT_SRC}" "${C_HEADER}" "${MODULEMAP}"; do
    if [[ ! -f "${f}" ]]; then
        echo "error: uniffi-bindgen did not produce expected file ${f}" >&2
        ls -la "${GEN_TMP}" >&2
        exit 1
    fi
done

# Move the Swift binding source into the Swift Package. The
# `Generated/` subdirectory is gitignored — every CI run produces
# a fresh copy, and committing it would bloat the repo and create
# merge conflicts on every API change.
cp "${SWIFT_SRC}" "${BINDINGS_OUT}/KMailFFI.swift"

echo "==> Building staticlibs for each iOS target"
for target in "${IOS_TARGETS[@]}"; do
    echo "    -> ${target}"
    cargo build --profile "${PROFILE}" -p kmail-ffi --target "${target}"
done

# Combine the two simulator slices into a single fat staticlib.
# XCFramework expects at most ONE library per platform variant,
# so we collapse aarch64-sim + x86_64-sim into one lib.
echo "==> Lipo-ing simulator slices"
SIM_FAT="${STAGING_DIR}/libkmail_ffi-sim.a"
lipo -create \
    "${SDK_DIR}/target/aarch64-apple-ios-sim/${PROFILE}/libkmail_ffi.a" \
    "${SDK_DIR}/target/x86_64-apple-ios/${PROFILE}/libkmail_ffi.a" \
    -output "${SIM_FAT}"

# Stage the device slice too, just so the header / modulemap
# layout is consistent across slices below.
DEVICE_LIB="${SDK_DIR}/target/aarch64-apple-ios/${PROFILE}/libkmail_ffi.a"

# Each XCFramework slice needs its own copy of the C header and
# modulemap. The modulemap declares the framework module name
# Swift code uses to `import` the low-level FFI shim. We use the
# crate-derived name `kmail_ffiFFI` so the generated Swift's
# `import kmail_ffiFFI` line resolves without post-processing.
echo "==> Staging headers + modulemap per slice"
DEVICE_HEADERS="${STAGING_DIR}/headers-device"
SIM_HEADERS="${STAGING_DIR}/headers-sim"
mkdir -p "${DEVICE_HEADERS}" "${SIM_HEADERS}"
cp "${C_HEADER}" "${DEVICE_HEADERS}/"
cp "${MODULEMAP}" "${DEVICE_HEADERS}/module.modulemap"
cp "${C_HEADER}" "${SIM_HEADERS}/"
cp "${MODULEMAP}" "${SIM_HEADERS}/module.modulemap"

echo "==> Assembling XCFramework"
mkdir -p "$(dirname "${FRAMEWORK_OUT}")"
xcodebuild -create-xcframework \
    -library "${DEVICE_LIB}" -headers "${DEVICE_HEADERS}" \
    -library "${SIM_FAT}" -headers "${SIM_HEADERS}" \
    -output "${FRAMEWORK_OUT}"

# Sanity-check the assembled output. The XCFramework's Info.plist
# is the canonical declaration of which slices are present; if it
# doesn't exist the create-xcframework step silently failed and
# our `swift test` step would explode later with a confusing
# missing-module error.
if [[ ! -f "${FRAMEWORK_OUT}/Info.plist" ]]; then
    echo "error: xcodebuild produced an XCFramework without Info.plist" >&2
    exit 1
fi

echo ""
echo "Done."
echo "  XCFramework: ${FRAMEWORK_OUT}"
echo "  Swift binding: ${BINDINGS_OUT}/KMailFFI.swift"
echo ""
echo "Next: cd apps/ios && swift test"
