//go:build !linux

package wal

// dataSync makes the file's appended data durable on every platform other
// than Linux by delegating to [os.File.Sync] (fsync(2) / F_FULLFSYNC on
// macOS / FlushFileBuffers on Windows).
//
// fdatasync(2) is a Linux-specific optimisation that skips the redundant
// inode-metadata flush on the WAL commit hot path (see data_sync_linux.go).
// Other platforms either do not expose an equivalent at the Go syscall layer
// or do not benefit, so the WAL keeps the full, always-correct fsync here:
// behaviour and durability are byte-for-byte identical to the pre-#1510 code
// on these platforms.
//
// Delegating to the WALFile's own Sync (rather than a type-asserted
// *os.File) also preserves the fault-injection seam: a *testfs.FaultFile's
// Sync still fires its injected faults unchanged.
func dataSync(f WALFile) error {
	return f.Sync()
}
