//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package wal

// walNoFollow is zero on platforms without an O_NOFOLLOW open flag (notably
// Windows). Unix symlinks are not a portable concept on these filesystems, and
// Windows reparse-point traversal is governed by separate OS-level controls;
// the WAL symlink-escape regression test skips where symlink creation is
// unavailable. OR-ing a zero flag is a no-op, so the WAL open sites compile and
// run unchanged everywhere while the Unix build applies the guard. This mirrors
// the snapshot layer's safe_open_other.go fallback.
const walNoFollow = 0
