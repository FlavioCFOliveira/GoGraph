//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package csrfile

// parentDirFsync is a no-op on platforms outside the unix build set, which do
// not expose a directory-fsync primitive at the syscall layer: there is no
// equivalent of POSIX fsync(2) against a directory handle on Windows.
//
// WHAT BECOMES OF THE DIRECTORY ENTRY THERE IS NOT STATED HERE, deliberately
// (rmp #2582). This doc used to say that on Windows the dirent produced by a
// rename "becomes durable once the file system commits its log", citing LMDB,
// SQLite and RocksDB as doing the same. That is an inference from the absent
// barrier rather than a measurement, and the storage-engine audit that raised it
// found no normative Microsoft documentation on NTFS journal-commit ordering
// with respect to MoveFileEx. Under the project's evidence-over-assumption rule
// an unmeasured reassurance is worse than silence, because a reader takes it for
// a guarantee.
//
// What IS true and is all that is claimed: this function performs no barrier, so
// GoGraph establishes nothing about the entry's durability on these platforms.
// Whatever holds there is a property of the filesystem, and a caller who needs
// the guarantee must obtain it from the platform rather than from here.
//
// Callers must not assume parentDirFsync provides any durability guarantee on
// these platforms: it exists only so the shared publish path can compile and
// run.
func parentDirFsync(_ string) error { return nil }
