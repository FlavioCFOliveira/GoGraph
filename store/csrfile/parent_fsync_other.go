//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package csrfile

// parentDirFsync is a no-op on platforms that do not expose a
// directory-fsync primitive at the syscall layer (Windows) or where the
// filesystem semantics make a directory fsync redundant.
//
// On Windows the creation of a directory entry (including the dirent
// produced by a rename) is part of the NTFS/ReFS metadata journal and
// becomes durable once the file system commits its log; there is no
// equivalent of POSIX fsync(2) against a directory handle. The
// bulk-loader csrfile publish therefore relies on the journal alone on
// Windows, consistent with how LMDB, SQLite, and RocksDB handle the same
// problem.
//
// Callers must not assume parentDirFsync provides any durability
// guarantee on these platforms: it exists only so the shared publish
// path can compile and run.
func parentDirFsync(_ string) error { return nil }
