//go:build linux

package wal

import (
	"os"

	"golang.org/x/sys/unix"
)

// dataSync makes the file's appended data — and the file-size growth needed
// to read that data back — durable, using the cheapest call that still
// satisfies WAL durability on Linux.
//
// # Why fdatasync and not fsync on the commit hot path
//
// [os.File.Sync] issues fsync(2), which flushes the file's data AND all of
// its inode metadata (mtime, atime, and so on). The WAL commit path only
// appends bytes to a growing file and then needs them durable; the
// non-essential inode-metadata (timestamp) flush is wasted work on every
// commit. fdatasync(2) skips exactly that: it flushes the data plus only the
// metadata required to retrieve it — crucially, the file size — and is the
// standard WAL sync on Linux (PostgreSQL wal_sync_method=fdatasync, the
// RocksDB default). It is typically ~10–30% cheaper per commit because it
// avoids a second metadata write to the inode.
//
// # Durability contract (the load-bearing point)
//
// A WAL commit is durable only if both the appended bytes AND the new file
// length are on stable storage — otherwise recovery cannot read the bytes
// back even though they reached the platter. POSIX fdatasync(2) is specified
// to flush "file data, and [...] file system metadata required to allow a
// subsequent data retrieval to be successfully completed"; for an append the
// increased file size is precisely such metadata, so a mainstream Linux
// filesystem (ext4, xfs) makes the grown size durable under fdatasync. This
// is the same guarantee PostgreSQL relies on for its default sync method.
// The deterministic crash-injection battery (internal/crashinject,
// store/recovery, store/wal under -tags gograph_crashinject) certifies that
// no acknowledged commit is lost across a crash on this path.
//
// fdatasync is used ONLY for the per-commit data sync. File creation, the
// parent-directory fsync, and every truncate/rename metadata sync keep the
// full [os.File.Sync] (fsync) because they must make inode/directory
// metadata — not just the data and size — durable.
//
// dataSync requires the concrete *os.File handle to obtain the file
// descriptor for fdatasync(2). For any other WALFile implementation (the
// fault-injection *testfs.FaultFile, the benchmark discardFile) it falls
// back to the interface's Sync method, so those test seams keep their exact
// behaviour. A nil-fd or otherwise unusable *os.File falls back to Sync too.
func dataSync(f WALFile) error {
	osf, ok := f.(*os.File)
	if !ok {
		// Synthetic test file (FaultFile / discardFile): preserve its own
		// Sync semantics, including injected faults.
		return f.Sync()
	}
	fd := int(osf.Fd())
	if fd < 0 {
		// Defensive: a closed/invalid handle. Fall back to the standard
		// Sync, which surfaces the same error os.File would report.
		return osf.Sync()
	}
	return unix.Fdatasync(fd)
}
