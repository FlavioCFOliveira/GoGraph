//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package snapshot

// parentDirFsync is a no-op on platforms that either do not expose a
// directory-fsync primitive at the syscall layer (Windows) or where
// the filesystem semantics make a directory fsync redundant.
//
// On Windows the underlying NTFS/ReFS rename of a directory entry is
// part of the metadata journal and becomes durable once the file
// system commits its log; there is no equivalent of POSIX fsync(2)
// against a directory handle, and FlushFileBuffers against a
// directory handle is undefined behaviour. The snapshot publish
// path therefore relies on the journal alone on Windows, which is
// consistent with how all major Windows databases (LMDB, SQLite,
// RocksDB) handle the same problem.
//
// Callers must not assume parentDirFsync provides any durability
// guarantee on these platforms: it exists only so the shared
// publish protocol can compile and run.
func parentDirFsync(_ string) error { return nil }
