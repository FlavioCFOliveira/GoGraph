// Package crashpoint holds the production-callable half of the
// crash-injection machinery: the [Breakpoint] hook and the environment
// variables that drive it. It deliberately depends on nothing beyond
// os and syscall so that production packages (for example store/wal and
// store/checkpoint) can embed crash-injection points without dragging
// the testing package — and the test-only subprocess runner — into
// their production binaries.
//
// # Build-tag gating
//
// [Breakpoint] has two implementations selected at compile time by the
// gograph_crashinject build tag:
//
//   - crashpoint_disabled.go (//go:build !gograph_crashinject) — the
//     default. Breakpoint is an empty function that the compiler elides
//     entirely: it reads no environment variable and links no syscall.
//     This is what every released binary contains, so GOGRAPH_CRASH_AT
//     can never make a production process kill itself, and there is no
//     per-call overhead on the WAL-truncate and checkpoint paths.
//   - crashpoint_enabled.go (//go:build gograph_crashinject) — the
//     active hook. Breakpoint reads GOGRAPH_CRASH_AT and, on a matching
//     name, sends SIGKILL to the current process. Only the deterministic
//     crash-injection battery (internal/crashinject and
//     cmd/crashinject-helper) is built with this tag.
//
// The exported API ([Breakpoint], [EnvCrashAt], [EnvCrashDir]) is
// identical in both modes, so call sites never change. To exercise the
// crash battery, build and test with -tags gograph_crashinject.
//
// The subprocess harness that spawns a child, sets these environment
// variables, and inspects the torn artefacts lives in the sibling
// package internal/crashinject; it re-exports the names defined here so
// existing call sites keep working.
package crashpoint

// EnvCrashAt is the environment variable read by [Breakpoint] (in the
// gograph_crashinject build) to decide which named point should trigger
// a crash. Without that build tag [Breakpoint] ignores the variable
// entirely.
const EnvCrashAt = "GOGRAPH_CRASH_AT"

// EnvCrashDir is the environment variable that tells the helper binary
// where to place its artefacts (WAL files, temp data).
const EnvCrashDir = "GOGRAPH_CRASH_DIR"

// Breakpoint is the crash-injection hook embedded in production
// durability paths (for example store/wal.Writer.Truncate and
// store/checkpoint). Its behaviour is selected at compile time:
//
//   - Without the gograph_crashinject build tag (every released binary)
//     it is a no-op the compiler elides — no environment lookup, no
//     signal. See crashpoint_disabled.go.
//   - With the gograph_crashinject build tag it reads GOGRAPH_CRASH_AT
//     and, when it equals name, sends SIGKILL to the current process to
//     simulate an abrupt crash at this exact execution point. See
//     crashpoint_enabled.go.
//
// name must be non-empty; an empty name is silently ignored so that
// callers cannot accidentally crash when the environment variable is
// unset.
//
// Breakpoint is safe for concurrent use: in the enabled build it reads
// an environment variable set once at process startup and takes no
// locks; in the disabled build it does nothing.
//
// The concrete implementations live in crashpoint_enabled.go and
// crashpoint_disabled.go.
