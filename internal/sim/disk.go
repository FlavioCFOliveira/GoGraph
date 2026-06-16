package sim

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"sync"
)

// sectorSize is the granularity of the fault bitmap. Each 512-byte sector can
// be independently marked faulted; a write into a faulted sector is corrupted
// deterministically.
const sectorSize = 512

// ErrSimFault is the sentinel returned by a simulated file operation that the
// seed-driven fault injector chose to fail. Callers match it with
// [errors.Is]. It models a durability fault: data the caller believed it was
// flushing did not reach stable storage.
var ErrSimFault = errors.New("sim: injected disk fault")

// walFile mirrors the (unexported) file-system interface that the WAL
// [github.com/FlavioCFOliveira/GoGraph/store/wal.Writer] requires of its
// underlying handle (see store/wal/writer_vfs.go). It cannot be imported by
// name because the WAL declares it unexported, so it is restated here verbatim;
// Go's structural typing means a [SimFileHandle] that satisfies this local copy
// also satisfies the WAL's own interface, which is what lets Phase 2 pass a
// SimFileHandle to wal.OpenWith. The compile-time assertion below guards the
// shape so a drift in either interface surfaces as a build failure here.
type walFile interface {
	io.Writer
	io.Reader
	io.Seeker
	Sync() error
	Truncate(size int64) error
	Close() error
}

// Compile-time assertion that a simulated file handle satisfies the WAL file
// interface, so SimDisk can stand in for *os.File once the engine is wired to
// it in Phase 2.
var _ walFile = (*SimFileHandle)(nil)

// SimDisk is an in-memory filesystem with seed-driven fault injection. It backs
// the durability layer of the simulation: files live entirely in memory, and a
// per-sector fault bitmap plus a per-Sync fault probability let the simulator
// reproduce torn writes and failed flushes deterministically.
//
// SimDisk is built in Phase 1 but is not yet wired into the engine (that is
// Phase 2 work); it must compile, implement the WAL file interface, and be
// unit-tested standalone.
//
// # Concurrency contract
//
// SimDisk's directory operations are guarded by an internal mutex so the file
// table cannot be corrupted, but the simulation drives it from a single
// goroutine and the fault decisions draw from the shared single-goroutine
// [Seed]; it must not be used concurrently.
//
// The "Sim" prefix is part of the DST harness's deliberate naming scheme
// (SimDisk / SimFileHandle / SimReport), which reads clearly at call sites and
// matches the design specification; the apparent stutter is intentional.
//
//nolint:revive // intentional SimXxx naming scheme (see comment above).
type SimDisk struct {
	mu        sync.Mutex
	files     map[string]*simFile
	faultRate float64
	seed      *Seed
}

// simFile is the in-memory backing store for one path. data holds the file
// bytes; faulted marks, per sector index, whether writes into that sector are
// corrupted.
type simFile struct {
	data    []byte
	faulted map[int]bool
}

// NewSimDisk returns an empty in-memory filesystem. faultRate is the
// probability (clamped to [0,1]) that any individual Sync fails with
// [ErrSimFault] and that a freshly written sector is marked faulted. seed
// drives every fault decision so the fault sequence is reproducible.
func NewSimDisk(seed *Seed, faultRate float64) *SimDisk {
	if faultRate < 0 {
		faultRate = 0
	}
	if faultRate > 1 {
		faultRate = 1
	}
	return &SimDisk{
		files:     make(map[string]*simFile),
		faultRate: faultRate,
		seed:      seed,
	}
}

// OpenFile opens (creating when os.O_CREATE is set) the file at path and
// returns a handle positioned per the flags: at end when os.O_APPEND is set, at
// zero otherwise. When os.O_TRUNC is set the file's contents are discarded. It
// returns an error wrapping fs.ErrNotExist when the file is absent and
// os.O_CREATE is not set.
func (d *SimDisk) OpenFile(path string, flag int) (*SimFileHandle, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	f, ok := d.files[path]
	if !ok {
		if flag&os.O_CREATE == 0 {
			return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
		}
		f = &simFile{faulted: make(map[int]bool)}
		d.files[path] = f
	}
	if flag&os.O_TRUNC != 0 {
		f.data = f.data[:0]
		f.faulted = make(map[int]bool)
	}
	h := &SimFileHandle{disk: d, file: f, path: path}
	if flag&os.O_APPEND != 0 {
		h.pos = int64(len(f.data))
	}
	return h, nil
}

// Rename atomically moves the file at oldPath to newPath, replacing any
// existing destination. It returns an error wrapping fs.ErrNotExist when the
// source is absent.
func (d *SimDisk) Rename(oldPath, newPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	f, ok := d.files[oldPath]
	if !ok {
		return &fs.PathError{Op: "rename", Path: oldPath, Err: fs.ErrNotExist}
	}
	delete(d.files, oldPath)
	d.files[newPath] = f
	return nil
}

// MkdirAll is a no-op for the in-memory filesystem: there is no directory tree,
// paths are opaque keys. It exists to satisfy the filesystem surface the WAL
// and snapshot writers expect. perm is ignored.
func (d *SimDisk) MkdirAll(_ string, _ fs.FileMode) error { return nil }

// Remove deletes the file at path. Removing an absent path is a no-op, matching
// the tolerant cleanup the snapshot writer relies on.
func (d *SimDisk) Remove(path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.files, path)
	return nil
}

// Exists reports whether a file is present at path.
func (d *SimDisk) Exists(path string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.files[path]
	return ok
}

// Snapshot returns an independent deep copy of every file's contents keyed by
// path. Mutating the returned maps or slices never affects the live
// filesystem, so a caller can capture disk state for comparison after a crash.
func (d *SimDisk) Snapshot() map[string][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string][]byte, len(d.files))
	for path, f := range d.files {
		cp := make([]byte, len(f.data))
		copy(cp, f.data)
		out[path] = cp
	}
	return out
}

// SimFileHandle is an open handle onto a [SimDisk] file. It implements the WAL
// file interface (io.Reader, io.Writer, io.Seeker, Sync, Truncate, Close) so it
// can substitute for *os.File.
//
// # Concurrency contract
//
// SimFileHandle is NOT safe for concurrent use; it is driven from the single
// simulation goroutine.
//
//nolint:revive // "Sim" prefix is intentional (see SimDisk).
type SimFileHandle struct {
	disk   *SimDisk
	file   *simFile
	path   string
	pos    int64
	closed bool
}

// Read copies up to len(p) bytes from the current position into p, advancing
// the position. It returns io.EOF when the position is at or past end of file.
func (h *SimFileHandle) Read(p []byte) (int, error) {
	if h.closed {
		return 0, fs.ErrClosed
	}
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	if h.pos >= int64(len(h.file.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.file.data[h.pos:])
	h.pos += int64(n)
	return n, nil
}

// Write copies p to the file at the current position, growing the file as
// needed, and advances the position. Any byte written into a sector that the
// fault injector has marked faulted is corrupted deterministically (a single
// byte in that sector is flipped), modelling a torn or mis-directed write.
func (h *SimFileHandle) Write(p []byte) (int, error) {
	if h.closed {
		return 0, fs.ErrClosed
	}
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()

	end := h.pos + int64(len(p))
	if end > int64(len(h.file.data)) {
		grown := make([]byte, end)
		copy(grown, h.file.data)
		h.file.data = grown
	}
	copy(h.file.data[h.pos:end], p)

	// Decide, per touched sector, whether the write lands in a faulted sector
	// and corrupt it. Iterating sectors in ascending index keeps the draw
	// order deterministic.
	first := int(h.pos / sectorSize)
	last := int((end - 1) / sectorSize)
	for sec := first; sec <= last; sec++ {
		if !h.file.faulted[sec] && h.disk.seed.Bool(h.disk.faultRate) {
			h.file.faulted[sec] = true
		}
		if h.file.faulted[sec] {
			h.corruptSector(sec)
		}
	}
	h.pos = end
	return len(p), nil
}

// corruptSector flips the first byte of the given sector deterministically.
// The caller holds disk.mu.
func (h *SimFileHandle) corruptSector(sec int) {
	off := sec * sectorSize
	if off < len(h.file.data) {
		h.file.data[off] ^= 0xFF
	}
}

// Seek repositions the handle per the standard io.Seeker whence values and
// returns the resulting absolute offset. A negative resulting offset is an
// error.
func (h *SimFileHandle) Seek(offset int64, whence int) (int64, error) {
	if h.closed {
		return 0, fs.ErrClosed
	}
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = h.pos + offset
	case io.SeekEnd:
		abs = int64(len(h.file.data)) + offset
	default:
		return 0, errors.New("sim: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("sim: negative seek offset")
	}
	h.pos = abs
	return abs, nil
}

// Truncate resizes the file to size bytes, zero-filling when growing and
// dropping fault marks for sectors that no longer exist.
func (h *SimFileHandle) Truncate(size int64) error {
	if h.closed {
		return fs.ErrClosed
	}
	if size < 0 {
		return errors.New("sim: negative truncate size")
	}
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	if size <= int64(len(h.file.data)) {
		h.file.data = h.file.data[:size]
	} else {
		grown := make([]byte, size)
		copy(grown, h.file.data)
		h.file.data = grown
	}
	lastSector := int((size - 1) / sectorSize)
	for sec := range h.file.faulted {
		if int64(size) == 0 || sec > lastSector {
			delete(h.file.faulted, sec)
		}
	}
	return nil
}

// Sync models flushing OS buffers to stable storage. With probability
// faultRate (drawn from the disk's seed) it fails with [ErrSimFault], modelling
// a durability fault. Each call consumes exactly one draw from the seed so the
// fault sequence is reproducible.
func (h *SimFileHandle) Sync() error {
	if h.closed {
		return fs.ErrClosed
	}
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	if h.disk.seed.Bool(h.disk.faultRate) {
		return ErrSimFault
	}
	return nil
}

// Close releases the handle. It is idempotent: a second Close is a no-op.
func (h *SimFileHandle) Close() error {
	h.closed = true
	return nil
}
