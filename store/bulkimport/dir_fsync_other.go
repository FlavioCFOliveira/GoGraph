//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package bulkimport

// dirFsync is a no-op on platforms without a directory-fsync primitive
// (Windows), exactly as store/snapshot's is: there a directory entry becomes
// durable through the filesystem's metadata journal, and FlushFileBuffers on a
// directory handle is undefined. It exists only so [mkdirAllDurable] compiles.
func dirFsync(_ string) error { return nil }
