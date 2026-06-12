//go:build linux || darwin || freebsd || netbsd || openbsd

package wal

import (
	"fmt"
	"os"
	"syscall"
)

// acquireLock creates (or opens) a LOCK file at path and acquires an
// exclusive flock(2) on it in non-blocking mode. It returns the open
// file handle, which the caller must retain until [releaseLock] is
// called.
//
// flock(2) is used rather than fcntl(2) / OFD locks because:
//   - It is available on every POSIX target the module supports.
//   - Unlike advisory locks, it is released automatically by the
//     kernel when the process dies, so a crashed writer cannot
//     permanently strand the lock.
//   - LOCK_NB makes the call fail immediately rather than block,
//     so a second process sees [ErrWALLocked] without hanging.
//
// The lock file is not the WAL data file: creating a dedicated LOCK
// sentinel avoids any interaction between the flock held here and the
// O_APPEND writes the [Writer] makes to the WAL data file.
func acquireLock(path string) (*os.File, error) {
	//nolint:gosec // caller-supplied WAL directory path
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("wal: open lock file %q: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return nil, ErrWALLocked
		}
		return nil, fmt.Errorf("wal: flock %q: %w", path, err)
	}
	return f, nil
}

// releaseLock releases the exclusive flock held by f and closes the
// file. Both operations are best-effort: a failure here means the OS
// will release the lock when the process exits anyway.
func releaseLock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
