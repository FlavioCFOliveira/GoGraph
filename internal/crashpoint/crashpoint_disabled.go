//go:build !gograph_crashinject

package crashpoint

// Breakpoint is a no-op in every build that omits the
// gograph_crashinject tag — which is every released binary.
//
// It takes no action: it does not read GOGRAPH_CRASH_AT and links no
// syscall, so an inherited GOGRAPH_CRASH_AT value cannot make a
// production process kill itself, and there is no per-call overhead on
// the durability paths (store/wal.Writer.Truncate, store/checkpoint)
// that embed it. The compiler inlines this empty body to nothing.
//
// The active crash-injection implementation lives in
// crashpoint_enabled.go and is compiled only under the
// gograph_crashinject build tag.
func Breakpoint(string) {}
