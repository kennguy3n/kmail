# ProGuard / R8 rules for consumers of the kmail-sdk AAR.
#
# AGP merges this file into the host app's R8 configuration, so
# anything declared here applies when an app that depends on
# `com.kmail:sdk` runs minification.
#
# The uniffi-generated Kotlin code uses JNA's reflective binding
# (`Native.load(...)` + interface proxies), so the JVM symbols
# must NOT be stripped or renamed. The same applies to our
# `MlsKeyProvider` callback interface, whose methods are invoked
# from native code via the JNA bridge.

# Keep the uniffi-generated package intact. JNA reflects on these
# classes at runtime to discover the FFI symbol layout. uniffi-bindgen
# emits everything under `uniffi.<crate>.*` (here `uniffi.kmail_ffi.*`).
-keep class uniffi.** { *; }
-keepclassmembers class uniffi.** { *; }

# Keep the public KMail SDK API. R8 might otherwise dead-code-
# eliminate methods our consumers reach via reflection (e.g.
# DI frameworks like Hilt or Koin).
-keep class com.kmail.sdk.** { *; }

# JNA's own reflective surface. JNA 5.x requires these rules
# in any release build; without them, R8 will remove the
# Structure / Pointer / Library classes JNA uses internally.
-keep class com.sun.jna.** { *; }
-keep class * implements com.sun.jna.Library { *; }
-keepclassmembers class com.sun.jna.** { *; }

# Coroutines reflective binding. The kotlinx-coroutines library
# uses reflection to discover continuation classes at runtime.
-keepnames class kotlinx.coroutines.internal.MainDispatcherFactory { *; }
-keepclassmembers class kotlinx.coroutines.** {
    volatile <fields>;
}
