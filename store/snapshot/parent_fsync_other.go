//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package snapshot

// dirFsync is a no-op on platforms that either do not expose a
// directory-fsync primitive at the syscall layer (Windows) or where the
// filesystem semantics make a directory fsync redundant.
//
// On Windows the underlying NTFS/ReFS metadata journal records both the
// creation of a file's directory entry and a directory rename, and those
// become durable once the file system commits its log; there is no
// equivalent of POSIX fsync(2) against a directory handle, and
// FlushFileBuffers against a directory handle is undefined behaviour. The
// snapshot publish path therefore relies on the journal alone on Windows
// for BOTH the staging-directory dirents and the published directory
// name, which is consistent with how all major Windows databases (LMDB,
// SQLite, RocksDB) handle the same problem.
//
// Callers must not assume dirFsync provides any durability guarantee on
// these platforms: it exists only so the shared publish protocol can
// compile and run.
func dirFsync(_ string) error { return nil }

// parentDirFsync is a no-op on these platforms for the same reasons as
// [dirFsync], to which it would otherwise delegate. It exists only so the
// shared publish protocol can compile and run; see [dirFsync] for the
// Windows journal rationale.
func parentDirFsync(_ string) error { return nil }
