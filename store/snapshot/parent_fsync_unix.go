//go:build linux || darwin || freebsd || netbsd || openbsd

package snapshot

import (
	"os"
	"path/filepath"
)

// dirFsync opens the directory at path and issues an fsync(2) on the
// directory descriptor, so the directory's own inode — including the
// dirents that link its child files into it — becomes durable.
//
// It is the primitive behind both publish-protocol fsyncs:
//
//   - The STAGING-directory fsync issued after every component file has
//     been written and fsync'd but BEFORE the publish rename. fsync(2) on
//     a file flushes that file's data and its inode, but it does NOT
//     guarantee that the dirent linking the file into its parent
//     directory is durable. Without an explicit fsync of the staging
//     directory, a crash on a filesystem that does not flush a renamed
//     directory's child dirents as part of the rename can leave the
//     published snapshot directory present yet its components missing or
//     zero-length — and if the checkpointer has already truncated the
//     WAL, every committed transaction folded into that checkpoint is
//     lost. This is the durability defect this primitive closes.
//   - The PARENT-directory fsync issued after the rename (via
//     [parentDirFsync]) so the new directory NAME survives the crash
//     window.
//
// The directory is opened with [os.O_RDONLY], which is portable across
// Linux and BSD-family kernels. Linux additionally accepts O_DIRECTORY,
// but O_RDONLY suffices because every caller in the snapshot publish
// protocol guarantees that path is a directory at the call point.
//
// Errors from either the open or the sync are propagated verbatim so the
// caller can decide whether to surface or retry; the publish path treats
// any error here as a hard failure because the durability contract is no
// longer met.
func dirFsync(path string) error {
	f, err := os.Open(path) //nolint:gosec // caller-controlled snapshot directory
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

// parentDirFsync makes a preceding rename(2) durable by fsync'ing the
// PARENT directory of childPath: on POSIX filesystems the rename's inode
// update is journalled, but the directory entry that points at the new
// snapshot directory only becomes durable after the parent directory
// itself is fsync'd. Without this step, a crash within the kernel's
// writeback window (~5s on Linux ext4 default, up to 30s under tuning)
// can leave the rename invisible to the next mount even though the
// snapshot's payload (csr.bin, manifest, etc.) is already durable on its
// own data blocks.
//
// childPath is the path that was just renamed into place (a file or
// directory); the function fsyncs filepath.Dir(childPath). childPath
// itself is not opened or modified. It delegates to [dirFsync] so the
// two publish-protocol fsyncs share a single syscall implementation.
func parentDirFsync(childPath string) error {
	return dirFsync(filepath.Dir(childPath))
}
