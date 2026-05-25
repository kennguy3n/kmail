// Thin re-export of `uniffi::uniffi_bindgen_main()`.
//
// UniFFI 0.28 ships the bindgen logic as a library function gated
// behind the `cli` feature on the `uniffi` crate. The canonical
// pattern (see https://mozilla.github.io/uniffi-rs/swift/module.html
// and https://mozilla.github.io/uniffi-rs/kotlin/gradle.html) is
// to add a tiny workspace-local binary that just forwards `argv`
// to `uniffi_bindgen_main()`. This:
//
//   1. Pins the bindgen version to whatever the workspace pins
//      `uniffi` to, so the generator and the runtime scaffolding
//      cannot drift apart silently.
//   2. Avoids requiring CI / contributors to `cargo install
//      uniffi-bindgen-cli`, which would pull a *separately*-
//      versioned binary off crates.io.
//   3. Plays nicely with `cargo run -p uniffi-bindgen -- ...`
//      from any working directory inside the workspace.
//
// The binary takes the full UniFFI bindgen CLI surface; see
// `cargo run -p uniffi-bindgen -- --help` for the available
// subcommands. We use `generate --library <path> --language
// swift --out-dir <dir>` from `scripts/build-ios-xcframework.sh`.
fn main() {
    uniffi::uniffi_bindgen_main()
}
