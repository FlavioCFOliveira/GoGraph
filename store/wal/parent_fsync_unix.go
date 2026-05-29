//go:build linux || darwin || freebsd || netbsd || openbsd

package wal

import (
	"os"
	"path/filepath"
)

// parentDirFsync opens the parent directory of childPath and issues an
// fsync(2) on the directory descriptor. It is called once, immediately
// after a WAL file is freshly created, to make the new directory entry
// durable.
//
// On POSIX filesystems, fsyncing a newly-created file's data (via
// [os.File.Sync] at commit time) does NOT guarantee that the directory
// entry naming that file is durable. A crash within the kernel's
// writeback window can therefore leave the entire WAL file invisible to
// the next mount even though a transaction was acknowledged as committed
// — a Durability violation on the very first commit (audit gap F4, see
// docs/acid-audit.md). Fsyncing the parent directory after creation
// closes that window: the inode→name link is forced to stable storage.
//
// childPath is the path of the WAL file just created; the function
// fsyncs filepath.Dir(childPath). childPath itself is not opened here
// (its data is fsynced separately by [Writer.Sync]).
//
// The directory is opened with [os.O_RDONLY], which is portable across
// Linux and the BSD family. Errors from either the open or the sync are
// propagated verbatim so the caller can treat them as a hard failure:
// if the parent cannot be fsynced, the durability contract is not met.
func parentDirFsync(childPath string) error {
	parent := filepath.Dir(childPath)
	f, err := os.Open(parent) //nolint:gosec // caller-controlled WAL directory
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
