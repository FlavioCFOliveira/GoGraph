// Package testfs provides a fault-injection wrapper around *os.File
// for use in crash-safety and durability tests of WAL, snapshot, and
// checkpoint paths.
//
// A [FaultFile] is created via [New] and honours the fault modes
// configured in [Faults]:
//
//   - [Faults.FailWritesAfterBytes] — returns an error once the
//     cumulative bytes written reaches the threshold (simulates a
//     partial write or disk failure mid-frame).
//   - [Faults.ReturnENOSPC] — makes every Write call return
//     [syscall.ENOSPC] regardless of bytes written.
//   - [Faults.FsyncDelay] — sleeps for the given duration before
//     each Sync call (simulates a slow or stalled fsync).
//   - [Faults.CorruptOnRead] — when non-nil, is called with the
//     current file offset and the number of bytes about to be read;
//     returning true flips the first byte of the result (simulates
//     a bit-flip or CRC corruption at the given position).
//
// [FaultFile] implements [File], the minimal filesystem interface
// accepted by store/wal.OpenWith and store/snapshot write paths.
// [File] is purposely narrow so it can be satisfied by both
// *os.File and *FaultFile without importing "os" in tests.
//
// Concurrency: [FaultFile] is safe for concurrent Read/Write/Seek/
// Sync/Truncate/Close calls; all mutations serialise on an internal
// mutex. The underlying *os.File's own locking therefore plays no
// role; the wrapper is the serialising layer.
package testfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// File is the minimal filesystem interface used by store/wal and
// store/snapshot for write paths. *os.File and *FaultFile both
// implement this interface so production code and tests can accept
// either without conditional compilation.
//
// File is safe for concurrent use by multiple goroutines (both
// *os.File and *FaultFile serialise their operations internally).
type File interface {
	io.ReadWriter
	io.Seeker
	// Sync flushes the OS write buffer to durable storage (equivalent
	// to fsync(2)).
	Sync() error
	// Truncate resizes the file to size bytes. If size < current
	// length, the suffix is discarded; if size > current length, the
	// file is extended with zero bytes (implementation-defined).
	Truncate(size int64) error
	// Close releases any associated OS resources.
	Close() error
}

// Faults configures the injected failure modes for a [FaultFile].
// The zero value disables all fault injection (the wrapper is a
// transparent pass-through).
type Faults struct {
	// FailWritesAfterBytes causes Write to fail once the cumulative
	// bytes written to the file reaches this value. The partial write
	// up to the threshold is permitted; subsequent writes return
	// [ErrPartialWrite]. Zero disables this mode.
	FailWritesAfterBytes int64

	// ReturnENOSPC causes every Write call to return [syscall.ENOSPC]
	// regardless of the current write budget.
	ReturnENOSPC bool

	// FsyncDelay inserts a sleep of this duration before each Sync
	// call. Zero disables the delay.
	FsyncDelay time.Duration

	// CorruptOnRead, when non-nil, is called with the current file
	// offset and the number of bytes about to be read. Returning true
	// flips the MSB of the first byte in the result buffer to simulate
	// a bit-flip or CRC-corrupting storage error.
	CorruptOnRead func(offset, n int64) bool
}

// ErrPartialWrite is returned by Write once [Faults.FailWritesAfterBytes]
// has been reached. It wraps [io.ErrShortWrite] so callers that
// already handle short writes behave correctly.
var ErrPartialWrite = fmt.Errorf("testfs: write budget exhausted: %w", io.ErrShortWrite)

// FaultFile wraps an *os.File with configurable fault injection.
// Zero-value is invalid; always create via [New].
//
// FaultFile is safe for concurrent use; all operations are
// serialised on an internal mutex.
type FaultFile struct {
	mu     sync.Mutex
	f      *os.File
	faults Faults
	// written is the cumulative bytes committed to the underlying
	// file (including partial writes up to the budget limit).
	written int64
	// offset mirrors the logical position tracked for CorruptOnRead.
	offset int64
}

// New opens or creates the file at path (flags: O_RDWR|O_CREATE)
// with the given Faults configuration.
func New(path string, faults Faults) (*FaultFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- path is caller-supplied test fixture.
	if err != nil {
		return nil, fmt.Errorf("testfs: open %q: %w", path, err)
	}
	return &FaultFile{f: f, faults: faults}, nil
}

// Wrap creates a FaultFile over an already-open *os.File. The
// caller must not use f directly after this call; FaultFile takes
// exclusive ownership.
func Wrap(f *os.File, faults Faults) *FaultFile {
	return &FaultFile{f: f, faults: faults}
}

// Write implements io.Writer. It respects Faults.ReturnENOSPC and
// Faults.FailWritesAfterBytes; a partial write is returned when the
// budget is crossed mid-call so the caller observes a
// (n < len(p), ErrPartialWrite) pair that mirrors a real OS short
// write.
func (ff *FaultFile) Write(p []byte) (int, error) {
	ff.mu.Lock()
	defer ff.mu.Unlock()

	if ff.faults.ReturnENOSPC {
		return 0, &os.PathError{Op: "write", Path: ff.f.Name(), Err: syscall.ENOSPC}
	}

	if ff.faults.FailWritesAfterBytes > 0 {
		remaining := ff.faults.FailWritesAfterBytes - ff.written
		if remaining <= 0 {
			return 0, ErrPartialWrite
		}
		if int64(len(p)) > remaining {
			// Allow the partial slice up to the budget, then stop.
			n, err := ff.f.Write(p[:remaining])
			ff.written += int64(n)
			ff.offset += int64(n)
			if err != nil {
				return n, err
			}
			return n, ErrPartialWrite
		}
	}

	n, err := ff.f.Write(p)
	ff.written += int64(n)
	ff.offset += int64(n)
	return n, err
}

// Read implements io.Reader. It honours Faults.CorruptOnRead by
// flipping the MSB of the first byte when the callback returns true
// for the current offset and read length.
func (ff *FaultFile) Read(p []byte) (int, error) {
	ff.mu.Lock()
	defer ff.mu.Unlock()

	n, err := ff.f.Read(p)
	if n > 0 && ff.faults.CorruptOnRead != nil {
		if ff.faults.CorruptOnRead(ff.offset, int64(n)) {
			p[0] ^= 0xFF
		}
	}
	ff.offset += int64(n)
	return n, err
}

// Seek implements io.Seeker and keeps the internal offset in sync.
func (ff *FaultFile) Seek(offset int64, whence int) (int64, error) {
	ff.mu.Lock()
	defer ff.mu.Unlock()

	pos, err := ff.f.Seek(offset, whence)
	if err == nil {
		ff.offset = pos
	}
	return pos, err
}

// Sync flushes to durable storage. It sleeps for Faults.FsyncDelay
// before delegating to the OS.
func (ff *FaultFile) Sync() error {
	ff.mu.Lock()
	defer ff.mu.Unlock()

	if ff.faults.FsyncDelay > 0 {
		// Unlock while sleeping so concurrent Reads/Writes are not
		// blocked for the duration of the delay.
		ff.mu.Unlock()
		time.Sleep(ff.faults.FsyncDelay)
		ff.mu.Lock()
	}
	return ff.f.Sync()
}

// Truncate resizes the file to size bytes.
func (ff *FaultFile) Truncate(size int64) error {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.f.Truncate(size)
}

// Close releases the underlying OS file.
func (ff *FaultFile) Close() error {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.f.Close()
}

// Written returns the cumulative bytes committed to the underlying
// file since the FaultFile was created.
func (ff *FaultFile) Written() int64 {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.written
}

// BudgetExhausted reports whether the FailWritesAfterBytes budget
// has been reached.
func (ff *FaultFile) BudgetExhausted() bool {
	if ff.faults.FailWritesAfterBytes == 0 {
		return false
	}
	ff.mu.Lock()
	defer ff.mu.Unlock()
	return ff.written >= ff.faults.FailWritesAfterBytes
}

// ResetWritten resets the cumulative-bytes counter and re-enables
// writes after a previous FailWritesAfterBytes fault. This allows
// a test to confirm a partial-write scenario and then continue
// writing to the same file for a second phase.
func (ff *FaultFile) ResetWritten() {
	ff.mu.Lock()
	defer ff.mu.Unlock()
	ff.written = 0
}

// Unwrap returns the underlying *os.File. Callers must not use the
// raw file concurrently with FaultFile methods.
func (ff *FaultFile) Unwrap() *os.File {
	return ff.f
}

// IsENOSPC reports whether err is (or wraps) a ENOSPC error.
func IsENOSPC(err error) bool {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return errors.Is(pe.Err, syscall.ENOSPC)
	}
	return errors.Is(err, syscall.ENOSPC)
}
