//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package snapshot

import "os"

// openSnapshotComponent opens a snapshot component file for reading.
//
// On platforms without an O_NOFOLLOW open flag (notably Windows) this is
// a plain read-only open. Unix symlinks are not a portable concept on
// these filesystems, and Windows reparse-point traversal is governed by
// separate OS-level controls; the dedicated symlink-escape regression
// test skips on platforms where symlink creation is unavailable. The
// shared snapshot read path calls this helper so it compiles and runs
// everywhere while the Unix build applies the O_NOFOLLOW guard.
func openSnapshotComponent(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // path is a fixed component name or validated index name under the snapshot dir
}
