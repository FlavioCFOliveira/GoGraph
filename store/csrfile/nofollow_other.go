//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package csrfile

// csrNoFollow is zero on platforms without an O_NOFOLLOW open flag (notably
// Windows). Unix symlinks are not a portable concept there and reparse-point
// traversal is governed by separate OS-level controls; the symlink-escape
// regression tests skip where symlink creation is unavailable. OR-ing a zero
// flag is a no-op, so every csrfile open site compiles and runs unchanged
// everywhere while the Unix build applies the guard. This mirrors
// store/wal/nofollow_other.go and store/snapshot/safe_open_other.go.
const csrNoFollow = 0
