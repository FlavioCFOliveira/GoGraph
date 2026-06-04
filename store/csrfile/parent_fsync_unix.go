//go:build linux || darwin || freebsd || netbsd || openbsd

package csrfile

import (
	"os"
	"path/filepath"
)

// parentDirFsync opens the parent directory of childPath and issues an
// fsync(2) on the directory descriptor, so the directory's own inode —
// including the dirent that links childPath into it — becomes durable.
//
// It is called once by [WriteToFile], immediately after the publish
// [os.Rename] moves the temp file onto its final path, to make the new
// directory entry durable.
//
// On POSIX filesystems, fsyncing the temp file's data (via
// [os.File.Sync] before the rename) does NOT guarantee that the
// directory entry naming the published file is durable. A crash within
// the kernel's writeback window can therefore leave the renamed file
// invisible to the next mount even though [WriteToFile] returned
// success — and because the bulk-loader path bypasses the WAL entirely
// (no WAL replay, no later checkpoint of this artefact), that lost
// directory entry loses the WHOLE bulk load despite an acknowledged
// success. Fsyncing the parent directory after the rename closes that
// window: the inode→name link is forced to stable storage.
//
// childPath is the path of the file just renamed into place; the
// function fsyncs filepath.Dir(childPath). childPath itself is not
// opened here (its data was fsynced separately before the rename).
//
// The directory is opened with [os.O_RDONLY], which is portable across
// Linux and the BSD family. Errors from either the open or the sync are
// propagated verbatim so the caller can treat them as a hard failure:
// if the parent cannot be fsynced, the durability contract is not met.
func parentDirFsync(childPath string) error {
	parent := filepath.Dir(childPath)
	f, err := os.Open(parent) //nolint:gosec // caller-supplied csrfile path
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
