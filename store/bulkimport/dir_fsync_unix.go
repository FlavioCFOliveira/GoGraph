//go:build linux || darwin || freebsd || netbsd || openbsd

package bulkimport

import "os"

// dirFsync opens the directory at path and issues an fsync(2) on the
// directory descriptor, making the entries that link its children into it
// durable. It is the operating-system primitive behind [mkdirAllDurable], and
// mirrors the directory fsync of store/snapshot and store/wal.
//
// Errors from the open, the sync or the close are returned verbatim; the
// publish treats any of them as a failure, because the durability contract is
// then not met.
func dirFsync(path string) error {
	f, err := os.Open(path) //nolint:gosec // caller-controlled store directory
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
