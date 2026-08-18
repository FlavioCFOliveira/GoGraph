//go:build linux || darwin || freebsd || netbsd || openbsd

package tmphygiene

// freespace_unix_test.go — the statfs(2) binding behind
// TestTempArea_VolumeHasRoomForTheGate.
//
// It is isolated in a REMOVABLE second file, paired with freespace_other_test.go,
// so a platform without the binding degrades exactly one capability (the
// free-space figure) instead of deleting the whole temp-hygiene gate. The build
// constraint mirrors the pairs already used across store/wal, store/snapshot and
// store/csrfile.

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// availableBytes reports the bytes available to an unprivileged process on the
// filesystem holding path.
//
// Bavail is used rather than Bfree because the reserved blocks Bfree includes are
// not writable by the test suite, and it is what the suite can actually write
// that decides whether the WAL will hit ENOSPC.
func availableBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	// Bsize is uint32 on darwin and int64 on linux; both widen to uint64
	// losslessly for any real block size.
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // G115: a filesystem block size is small and non-negative
}
