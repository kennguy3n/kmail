// napi-rs build helper.
//
// Emits the platform-specific linker flags so the resulting cdylib
// loads cleanly as a Node.js / Electron addon (`.node`). This is
// the canonical pattern from the napi-rs template — without it the
// build succeeds on Linux but fails to link Symbol(`napi_*`) on
// macOS + Windows.

fn main() {
    napi_build::setup();
}
