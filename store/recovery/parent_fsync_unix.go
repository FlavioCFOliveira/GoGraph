//go:build linux || darwin || freebsd || netbsd || openbsd

package recovery

import (
	"os"
	"path/filepath"
)

// parentDirFsync opens the parent directory of childPath and issues an
// fsync(2) on the directory descriptor. Recovery calls it once,
// immediately after it promotes a stranded snapshot backup
// (snapshot.bak -> snapshot) during interrupted-publish repair, to make
// the promoted directory's NAME durable.
//
// The interrupted-publish repair (see openCodec) renames snapshot.bak
// onto the live snapshot name when a crash inside the publish window left
// the previous snapshot stranded at the backup. On POSIX filesystems the
// rename's inode update is journalled, but the directory entry that links
// the promoted snapshot into its parent directory only becomes durable
// once the parent directory itself is fsync'd. Without this step, a crash
// within the kernel's writeback window (~5s on Linux ext4 default, up to
// 30s under tuning) can leave the promotion invisible to the next mount.
// Because the checkpoint that produced the stranded backup has ALREADY
// truncated the WAL prefix, the snapshot is the only durable copy of the
// pre-checkpoint state: losing the promoted dirent silently discards
// every checkpointed transaction. This mirrors the parent-dir fsync the
// snapshot publish path issues after its own publish rename
// (store/snapshot, [store/snapshot] writer.go / full.go) precisely to
// close the same window.
//
// childPath is the snapshot path just renamed into place; the function
// fsyncs filepath.Dir(childPath). childPath itself is not opened or
// modified here.
//
// The directory is opened with [os.O_RDONLY], which is portable across
// Linux and the BSD family. Errors from either the open or the sync are
// propagated verbatim so the caller can treat them as a hard failure: if
// the parent cannot be fsynced, the promotion's durability contract is
// not met and recovery must fail-stop rather than continue.
func parentDirFsync(childPath string) error {
	parent := filepath.Dir(childPath)
	f, err := os.Open(parent) //nolint:gosec // caller-controlled recovery snapshot directory
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
