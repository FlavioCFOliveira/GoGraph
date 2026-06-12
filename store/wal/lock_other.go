//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package wal

import (
	"fmt"
	"os"
)

// acquireLock creates a LOCK file at path using O_CREATE|O_EXCL as a
// best-effort mutual-exclusion mechanism on platforms that do not
// expose flock(2).
//
// On Windows, CreateFile with exclusive sharing is the canonical
// advisory lock primitive, but that requires the Win32 API which is
// not directly reachable from pure Go without cgo or the x/sys/windows
// package. The O_EXCL fallback used here provides a weaker guarantee
// (a crashed writer leaves the file behind and blocks a clean reopen
// until the file is manually removed), but it is sufficient for the
// most common race — two concurrent openers — and avoids adding a
// platform-specific dependency.
//
// A future version may upgrade to LockFileEx on Windows.
func acquireLock(path string) (*os.File, error) {
	//nolint:gosec // caller-supplied WAL directory path
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, ErrWALLocked
		}
		return nil, fmt.Errorf("wal: open lock file %q: %w", path, err)
	}
	return f, nil
}

// releaseLock removes and closes the O_EXCL lock file created by
// [acquireLock]. Both operations are best-effort.
func releaseLock(f *os.File) {
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
}
