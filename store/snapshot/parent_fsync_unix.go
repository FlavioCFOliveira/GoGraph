//go:build linux || darwin || freebsd || netbsd || openbsd

package snapshot

import (
	"os"
	"path/filepath"
)

// parentDirFsync opens the parent directory of childPath and issues
// an fsync(2) on the directory descriptor. The intent is to make a
// preceding rename(2) durable: on POSIX filesystems the rename's
// inode update is journalled, but the directory entry that points
// at the new snapshot directory only becomes durable after the
// parent directory itself is fsync'd. Without this step, a crash
// within the kernel's writeback window (~5s on Linux ext4 default,
// up to 30s under tuning) can leave the rename invisible to the
// next mount even though the snapshot's payload (csr.bin, manifest,
// etc.) is already durable on its own data blocks.
//
// childPath is the path that was just renamed into place (a file
// or directory); the function fsyncs filepath.Dir(childPath).
// childPath itself is not opened or modified.
//
// The directory is opened with [os.O_RDONLY], which is portable
// across Linux and BSD-family kernels. Linux additionally accepts
// the O_DIRECTORY flag, but O_RDONLY suffices because the caller
// guarantees that the path is a directory at this point in the
// snapshot publish protocol.
//
// Errors returned by either the open or the sync are propagated
// verbatim so the caller can decide whether to surface them or
// retry; the snapshot publish path treats any error here as a
// hard failure because the durability contract is no longer met.
func parentDirFsync(childPath string) error {
	parent := filepath.Dir(childPath)
	f, err := os.Open(parent) //nolint:gosec // caller-controlled snapshot directory
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
