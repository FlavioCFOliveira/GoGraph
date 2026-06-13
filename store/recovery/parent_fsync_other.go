//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package recovery

// parentDirFsync is a no-op on platforms that do not expose a
// directory-fsync primitive at the syscall layer (Windows) or where the
// filesystem semantics make a directory fsync redundant.
//
// On Windows the rename of a directory entry is part of the NTFS/ReFS
// metadata journal and becomes durable once the file system commits its
// log; there is no equivalent of POSIX fsync(2) against a directory
// handle. The snapshot-backup promotion in openCodec therefore relies on
// the journal alone on Windows, consistent with how the snapshot publish
// path and how LMDB, SQLite, and RocksDB handle the same problem.
//
// Callers must not assume parentDirFsync provides any durability
// guarantee on these platforms: it exists only so the promotion path can
// compile and run.
func parentDirFsync(_ string) error { return nil }
