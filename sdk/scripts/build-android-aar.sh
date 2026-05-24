#!/usr/bin/env bash
#
# Build the kmail-android-sdk AAR consumed by the Gradle module at
# `apps/android/kmail-sdk/`. This script is the source of truth
# for the Android binary build pipeline — both local developers
# and CI run it.
#
# Outputs (relative to the repo root):
#
#   apps/android/kmail-sdk/src/main/jniLibs/
#       Four-ABI staticlib directory packaged into the AAR:
#         - arm64-v8a/libkmail_ffi.so       (aarch64-linux-android)
#         - armeabi-v7a/libkmail_ffi.so     (armv7-linux-androideabi)
#         - x86_64/libkmail_ffi.so          (x86_64-linux-android,
#                                            emulator)
#         - x86/libkmail_ffi.so             (i686-linux-android,
#                                            32-bit emulator)
#
#   apps/android/kmail-sdk/src/main/kotlin/uniffi/kmail_ffi/
#       kmail_ffi.kt — the Kotlin binding source emitted by
#       `uniffi-bindgen`. The package name (`uniffi.kmail_ffi`)
#       is determined by uniffi-bindgen based on the FFI crate's
#       name; the Gradle module picks the file up under
#       `src/main/kotlin/uniffi/kmail_ffi/` and the public facade
#       at `apps/android/kmail-sdk/src/main/kotlin/com/kmail/sdk/KMail.kt`
#       imports the relevant types from `uniffi.kmail_ffi.*`.
#
#   apps/android/kmail-sdk/build/host-jna/
#       libkmail_ffi.so — host-arch linux .so used by the
#       JVM-on-Linux unit test runner via JNA. This is NOT
#       packaged into the AAR. It exists so the Gradle
#       `unitTests` task can exercise the real FFI surface
#       without spinning up an Android emulator — same idea as
#       the iOS Swift Package using the simulator slice for
#       `swift test`.
#
# Requirements:
#   - Rust toolchain (1.78+) via rustup
#   - cargo-ndk (`cargo install cargo-ndk`) — wraps `cargo build`
#     with the right `CC` / `CXX` / `AR` / `RANLIB` / `LINKER`
#     env vars for each Android target, sourced from the NDK
#     install at `${ANDROID_NDK_HOME}`.
#   - Android NDK r25c+ (LTS). `ANDROID_NDK_HOME` must point to
#     the NDK install root (e.g. `/opt/android-ndk-r25c` or
#     `${ANDROID_HOME}/ndk/25.2.9519653`).
#   - Android API level pinned via `ANDROID_API_LEVEL` (default
#     26 — Android 8.0 Oreo, the lowest level KChat supports).
#
# This script is idempotent: re-running it tears down and rebuilds
# the jniLibs + generated sources, so a stale binding can never
# sneak into a Gradle build.

set -euo pipefail

# Resolve the repo root regardless of where the script is invoked
# from. The `realpath` indirection handles symlinked checkouts.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SDK_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${SDK_DIR}/.." && pwd)"

# Target list pinned in one place so the rustup-target install
# loop below cannot drift from the cargo-ndk invocation further
# down. Order: device targets first, emulator targets after.
declare -ra ANDROID_TARGETS=(
    "aarch64-linux-android"
    "armv7-linux-androideabi"
    "x86_64-linux-android"
    "i686-linux-android"
)

# Mapping from Rust target triple to Android ABI directory name
# (the path Gradle's AGP scans inside `jniLibs/`). The Rust
# triples and Android ABIs do NOT correspond 1:1 (e.g.
# `armv7-linux-androideabi` -> `armeabi-v7a`), so the mapping is
# encoded here explicitly rather than derived by string mangling.
declare -rA ABI_FOR_TRIPLE=(
    ["aarch64-linux-android"]="arm64-v8a"
    ["armv7-linux-androideabi"]="armeabi-v7a"
    ["x86_64-linux-android"]="x86_64"
    ["i686-linux-android"]="x86"
)

# Custom profile defined in sdk/Cargo.toml. MUST retain symbols
# for both bindgen metadata extraction AND staticlib linking.
# See the `[profile.release-with-symbols]` comment block in
# sdk/Cargo.toml for the full reasoning. The same profile is
# used by the iOS XCFramework build for the same reason.
readonly PROFILE="release-with-symbols"

# Android API level the .so files target. r25 NDK supports
# API 21+. We default to 26 (Android 8 Oreo) because that's
# the KChat-wide floor and matches Stalwart's documented
# Android compatibility note in docs/SDK.md.
readonly ANDROID_API_LEVEL="${ANDROID_API_LEVEL:-26}"

# Output paths, all relative to the repo root so Gradle's
# `apps/android/kmail-sdk/build.gradle.kts` can reference them
# deterministically.
readonly MODULE_DIR="${REPO_ROOT}/apps/android/kmail-sdk"
readonly JNILIBS_OUT="${MODULE_DIR}/src/main/jniLibs"
# The uniffi-bindgen Kotlin generator emits files under a
# directory tree matching the generated package name
# (`uniffi.kmail_ffi`). We copy the whole `uniffi/` tree into
# `src/main/kotlin/` so the Kotlin compiler picks up the
# generated source under the expected package.
readonly BINDINGS_OUT="${MODULE_DIR}/src/main/kotlin/uniffi"
readonly HOST_JNA_OUT="${MODULE_DIR}/build/host-jna"
readonly STAGING_DIR="${SDK_DIR}/target/android-staging"

# Sanity-check the NDK install. cargo-ndk reads the linker /
# clang / ar paths from this directory; if the var is unset or
# wrong the build fails with a confusing "linker not found"
# error several minutes into the cross-compile. We fail loudly
# up front instead.
if [[ -z "${ANDROID_NDK_HOME:-}" ]]; then
    # Fall back to common install locations before erroring.
    for candidate in \
        "${HOME}/Library/Android/sdk/ndk-bundle" \
        "${HOME}/Android/Sdk/ndk-bundle" \
        "${ANDROID_HOME:-/dev/null}/ndk-bundle" \
        "/opt/android-ndk" \
        "/opt/android-ndk-r25c"
    do
        if [[ -d "${candidate}" ]]; then
            export ANDROID_NDK_HOME="${candidate}"
            echo "==> Auto-detected ANDROID_NDK_HOME=${ANDROID_NDK_HOME}"
            break
        fi
    done
fi

if [[ -z "${ANDROID_NDK_HOME:-}" || ! -d "${ANDROID_NDK_HOME}" ]]; then
    echo "error: ANDROID_NDK_HOME is unset or does not point to an NDK install." >&2
    echo "error: install NDK r25c+ and set ANDROID_NDK_HOME (e.g. /opt/android-ndk-r25c)." >&2
    exit 2
fi

if ! command -v cargo >/dev/null 2>&1; then
    echo "error: cargo not found on PATH (install via rustup)" >&2
    exit 2
fi

if ! command -v cargo-ndk >/dev/null 2>&1; then
    echo "error: cargo-ndk not found on PATH" >&2
    echo "error: install with: cargo install cargo-ndk" >&2
    exit 2
fi

echo "==> Installing rustup Android targets"
for target in "${ANDROID_TARGETS[@]}"; do
    # `rustup target add` is idempotent and re-uses cached
    # toolchain artifacts when the target is already installed.
    rustup target add "${target}"
done

echo "==> Cleaning previous artifacts"
rm -rf "${JNILIBS_OUT}" "${BINDINGS_OUT}" "${HOST_JNA_OUT}" "${STAGING_DIR}"
mkdir -p "${JNILIBS_OUT}" "${BINDINGS_OUT}" "${HOST_JNA_OUT}" "${STAGING_DIR}"

echo "==> Building host cdylib for binding extraction + JVM tests"
# We build the host cdylib so `uniffi-bindgen` can read the
# `UNIFFI_META_*` symbols out of it AND the Gradle unitTests
# task can dlopen it via JNA. The cdylib targets the host arch
# (linux-x86_64 on CI, varies locally). It MUST have symbols,
# hence the release-with-symbols profile.
cd "${SDK_DIR}"
cargo build --profile "${PROFILE}" -p kmail-ffi
cargo build --profile "${PROFILE}" -p uniffi-bindgen

# Find the host cdylib. The extension varies by OS:
#   linux:   libkmail_ffi.so
#   macos:   libkmail_ffi.dylib
#   windows: kmail_ffi.dll
# CI always runs on linux, but local dev on a macOS host should
# still work so we discover the path instead of hard-coding it.
HOST_CDYLIB=""
for candidate in \
    "${SDK_DIR}/target/${PROFILE}/libkmail_ffi.so" \
    "${SDK_DIR}/target/${PROFILE}/libkmail_ffi.dylib" \
    "${SDK_DIR}/target/${PROFILE}/kmail_ffi.dll"
do
    if [[ -f "${candidate}" ]]; then
        HOST_CDYLIB="${candidate}"
        break
    fi
done
if [[ -z "${HOST_CDYLIB}" ]]; then
    echo "error: could not find host cdylib in ${SDK_DIR}/target/${PROFILE}/" >&2
    ls -la "${SDK_DIR}/target/${PROFILE}/" >&2
    exit 1
fi
echo "==> Using host cdylib at ${HOST_CDYLIB}"

# Copy the host cdylib into the Gradle module's host-jna path
# under a name that matches the *actual* binary format on this
# host. JNA's library search uses the OS-conventional extension:
#
#   Linux:   libkmail_ffi.so   (ELF)
#   macOS:   libkmail_ffi.dylib (Mach-O)
#   Windows: kmail_ffi.dll      (PE/COFF)
#
# JNA + java.library.path resolve the correct extension per OS
# automatically — there's no need (and it would be misleading)
# to put a macOS Mach-O file under a `.so` filename. macOS's
# dlopen IS liberal about extensions and would happily load a
# Mach-O bundle named `.so`, but `file libkmail_ffi.so` would
# then mis-report the binary format and confuse anyone
# triaging a JNA load failure.
#
# On macOS dev machines, `./gradlew :kmail-sdk:test` will
# pick up `libkmail_ffi.dylib` via the same JVM args we set
# on `java.library.path` — JNA's `Native.loadLibrary("kmail_ffi")`
# tries the platform-native extension first.
case "${HOST_CDYLIB}" in
    *.so)
        cp "${HOST_CDYLIB}" "${HOST_JNA_OUT}/libkmail_ffi.so"
        ;;
    *.dylib)
        cp "${HOST_CDYLIB}" "${HOST_JNA_OUT}/libkmail_ffi.dylib"
        ;;
    *.dll)
        cp "${HOST_CDYLIB}" "${HOST_JNA_OUT}/kmail_ffi.dll"
        ;;
    *)
        echo "error: unrecognised host cdylib extension on ${HOST_CDYLIB}" >&2
        exit 1
        ;;
esac

echo "==> Generating Kotlin bindings from host cdylib"
# Generate into a scratch directory first, then move the
# pieces to their final homes. This avoids partial-generation
# failures leaving stale files in the Gradle module.
GEN_TMP="${STAGING_DIR}/bindgen-out"
rm -rf "${GEN_TMP}"
mkdir -p "${GEN_TMP}"
"${SDK_DIR}/target/${PROFILE}/uniffi-bindgen" \
    generate \
    --library "${HOST_CDYLIB}" \
    --language kotlin \
    --out-dir "${GEN_TMP}"

# uniffi-bindgen emits the Kotlin source under a directory
# tree matching the package declaration, which for our crate
# resolves to `uniffi/kmail_ffi/kmail_ffi.kt`. We mirror that
# whole directory into our `src/main/kotlin/uniffi/` so the
# generated `package uniffi.kmail_ffi` declaration resolves
# correctly when AGP compiles the Kotlin source set.
if [[ ! -d "${GEN_TMP}/uniffi" ]]; then
    echo "error: uniffi-bindgen did not produce expected ${GEN_TMP}/uniffi/ tree" >&2
    ls -laR "${GEN_TMP}" >&2
    exit 1
fi

KOTLIN_COUNT=$(find "${GEN_TMP}/uniffi" -type f -name '*.kt' | wc -l)
if [[ "${KOTLIN_COUNT}" -eq 0 ]]; then
    echo "error: uniffi-bindgen produced no .kt files under ${GEN_TMP}/uniffi/" >&2
    exit 1
fi

# Recursively copy the whole `uniffi/` tree into the Kotlin
# source set. The `mkdir -p "${BINDINGS_OUT}"` earlier already
# ensured the destination exists; `cp -R` of the inner contents
# preserves the package layout (uniffi/kmail_ffi/kmail_ffi.kt).
cp -R "${GEN_TMP}/uniffi/." "${BINDINGS_OUT}/"
echo "    Kotlin sources (${KOTLIN_COUNT} files) copied into ${BINDINGS_OUT}/"

echo "==> Building staticlibs for each Android target"
# cargo-ndk handles per-target CC/AR/LINKER setup based on the
# NDK install. The single invocation builds all four targets
# back-to-back; cargo's per-target target dir layout
# (target/<triple>/release-with-symbols/) keeps the outputs
# distinct.
cargo ndk \
    -t aarch64-linux-android \
    -t armv7-linux-androideabi \
    -t x86_64-linux-android \
    -t i686-linux-android \
    --platform "${ANDROID_API_LEVEL}" \
    build \
    --profile "${PROFILE}" \
    -p kmail-ffi

echo "==> Staging .so files into AGP jniLibs layout"
# Strip the cross-compiled .so files with the NDK's llvm-strip.
# The `release-with-symbols` profile keeps debug symbols so
# `uniffi-bindgen` can read `UNIFFI_META_*` symbols from the
# host cdylib (see the profile comment block in sdk/Cargo.toml).
# Those symbols are NOT needed at runtime on Android — the .so
# is loaded via System.loadLibrary and resolved dynamically.
# Without stripping, each Android .so is ~3-6x larger than
# necessary (uniffi metadata alone is ~2 MiB / arch). Strip them
# here so the AAR ships at the size a production library expects.
#
# llvm-strip lives at `<NDK>/toolchains/llvm/prebuilt/<host>/bin/llvm-strip`.
# We discover the host-prebuilt dir generically (linux-x86_64 on
# CI, darwin-x86_64 / darwin-arm64 locally) so this works on any
# NDK install.
LLVM_STRIP=""
for candidate in \
    "${ANDROID_NDK_HOME}/toolchains/llvm/prebuilt/linux-x86_64/bin/llvm-strip" \
    "${ANDROID_NDK_HOME}/toolchains/llvm/prebuilt/darwin-x86_64/bin/llvm-strip" \
    "${ANDROID_NDK_HOME}/toolchains/llvm/prebuilt/darwin-arm64/bin/llvm-strip" \
    "${ANDROID_NDK_HOME}/toolchains/llvm/prebuilt/windows-x86_64/bin/llvm-strip.exe"
do
    if [[ -x "${candidate}" ]]; then
        LLVM_STRIP="${candidate}"
        break
    fi
done
if [[ -z "${LLVM_STRIP}" ]]; then
    echo "warning: llvm-strip not found under ${ANDROID_NDK_HOME}/toolchains/llvm/prebuilt/*/bin/" >&2
    echo "warning: Android .so files will ship unstripped (larger AAR)" >&2
fi

for triple in "${ANDROID_TARGETS[@]}"; do
    abi="${ABI_FOR_TRIPLE[${triple}]}"
    src="${SDK_DIR}/target/${triple}/${PROFILE}/libkmail_ffi.so"
    dst_dir="${JNILIBS_OUT}/${abi}"
    dst="${dst_dir}/libkmail_ffi.so"
    mkdir -p "${dst_dir}"
    if [[ ! -f "${src}" ]]; then
        echo "error: expected staticlib at ${src} after cargo-ndk build" >&2
        exit 1
    fi
    cp "${src}" "${dst}"
    if [[ -n "${LLVM_STRIP}" ]]; then
        # `--strip-unneeded` drops debug symbols and unreferenced
        # symbols while keeping the dynamic-linker symbols
        # (uniffi's `extern "C"` entrypoints) that Android's
        # System.loadLibrary actually needs.
        before=$(stat -c '%s' "${dst}" 2>/dev/null || stat -f '%z' "${dst}")
        "${LLVM_STRIP}" --strip-unneeded "${dst}"
        after=$(stat -c '%s' "${dst}" 2>/dev/null || stat -f '%z' "${dst}")
        echo "    ${triple} -> ${abi}/libkmail_ffi.so (stripped: ${before} -> ${after} bytes)"
    else
        echo "    ${triple} -> ${abi}/libkmail_ffi.so (unstripped, no llvm-strip available)"
    fi
done

# Sanity-check: AGP requires at least one .so per declared ABI
# filter in the AAR. The Gradle module's build.gradle.kts
# declares all four ABIs in `ndk.abiFilters`; if one is
# missing here the AAR will be assembled but the abiFilters
# validation step in AGP will fail at packaging.
EXPECTED_ABIS=("arm64-v8a" "armeabi-v7a" "x86_64" "x86")
for abi in "${EXPECTED_ABIS[@]}"; do
    if [[ ! -f "${JNILIBS_OUT}/${abi}/libkmail_ffi.so" ]]; then
        echo "error: missing ${abi}/libkmail_ffi.so in jniLibs after build" >&2
        exit 1
    fi
done

echo ""
echo "Done."
echo "  jniLibs:        ${JNILIBS_OUT}"
echo "  Kotlin binding: ${BINDINGS_OUT}"
echo "  Host JNA .so:   ${HOST_JNA_OUT}"
echo ""
echo "Next: cd apps/android && ./gradlew :kmail-sdk:assembleRelease test"
