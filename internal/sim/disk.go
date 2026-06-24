package sim

import (
	"errors"
	"io"
	"io/fs"
	"os"
	pathpkg "path"
	"strings"
	"sync"
	"syscall"
	"time"
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
	// dirs tracks the dirent durability of DIRECTORIES that were published by
	// a directory rename (the snapshot publish renames a whole staging
	// directory onto the live name). The value is whether the directory's own
	// name in its parent is durable; a directory absent from this map is
	// implicitly durable (it was created in place via MkdirAll, never via a
	// publish rename). A [SimDisk.Crash] drops every directory whose dirent is
	// not yet durable, taking its entire subtree with it.
	dirs map[string]bool
	// capacityBytes, when > 0, bounds the total number of bytes the in-memory
	// filesystem may hold across all files. It models a finite disk so the DST
	// can drive the engine through a disk-full (ENOSPC) condition. Zero (the
	// default) means unbounded, so every pre-existing caller is byte-for-byte
	// unaffected. The budget check is a PURE function of the current byte total
	// and the requested growth — it draws NOTHING from the [Seed] — so turning a
	// capacity on never perturbs the reproducible torn-write/Sync fault stream.
	capacityBytes int64
	// enospcOnSync selects WHERE the out-of-space condition surfaces:
	//
	//   - false (eager mode, the default): a Write / Truncate / TruncatePath
	//     that would grow the total past capacityBytes returns an ENOSPC
	//     [os.PathError] and grows nothing — modelling allocate-on-write.
	//   - true (delayed-allocation mode): growth is buffered in memory and
	//     succeeds, but Sync returns ENOSPC whenever the total exceeds
	//     capacityBytes — modelling the common case where a full disk only
	//     surfaces at fsync. This is the harder path for the WAL commit contract.
	//
	// It has no effect when capacityBytes is 0.
	enospcOnSync bool
}

// simFile is the in-memory backing store for one path. data holds the file
// bytes; faulted marks, per sector index, whether writes into that sector are
// corrupted.
//
// direntDurable models POSIX directory-entry durability: it is the in-memory
// analogue of whether the name->inode link of this path is on stable storage.
// A create or rename links a name with direntDurable=false; only a
// [SimDisk.DirSync] of the containing directory (the fsync(2)-on-a-directory
// primitive the snapshot/csrfile publish protocol relies on) flips it true. A
// [SimDisk.Crash] drops every dirent that is not yet durable, exactly as a
// real crash within the kernel writeback window loses a rename whose parent
// directory was never fsync'd. The file's DATA durability is modelled
// separately by the per-Sync fault and the torn-write sector bitmap; this flag
// is only about the name's link surviving a crash.
type simFile struct {
	data          []byte
	faulted       map[int]bool
	direntDurable bool
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
		dirs:      make(map[string]bool),
	}
}

// SetCapacity bounds the disk to capacityBytes total bytes across all files,
// modelling a finite disk. When enospcOnSync is false the out-of-space
// condition surfaces eagerly at the growing Write/Truncate; when true it
// surfaces at Sync (delayed allocation). A capacityBytes of 0 removes the bound
// (the default). It must be called before the store is driven, from the single
// simulation goroutine; it draws nothing from the seed, so it never perturbs
// the reproducible fault stream. See the field docs on [SimDisk] for the model.
func (d *SimDisk) SetCapacity(capacityBytes int64, enospcOnSync bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if capacityBytes < 0 {
		capacityBytes = 0
	}
	d.capacityBytes = capacityBytes
	d.enospcOnSync = enospcOnSync
}

// totalBytesLocked returns the sum of every file's data length. The caller must
// hold d.mu. It is O(files); the simulated store holds only a handful of files
// (the WAL plus the checkpoint components), so the linear scan is negligible and
// avoids the bookkeeping-drift risk of a running counter.
func (d *SimDisk) totalBytesLocked() int64 {
	var total int64
	for _, f := range d.files {
		total += int64(len(f.data))
	}
	return total
}

// enospc builds the ENOSPC [os.PathError] the eager and delayed paths return.
// It matches the shape internal/testfs uses, so both errors.Is(err,
// syscall.ENOSPC) and the WAL's errno classifier recognise it.
func enospc(op, path string) error {
	return &os.PathError{Op: op, Path: path, Err: syscall.ENOSPC}
}

// wouldExceedLocked reports whether growing a file from oldLen to newLen bytes
// would push the disk's total past its capacity. It is a pure function of the
// current byte total (no seed draw). With no capacity, or in delayed-allocation
// mode (where growth is always buffered and Sync enforces the budget), it never
// blocks a write. The caller must hold d.mu.
func (d *SimDisk) wouldExceedLocked(oldLen, newLen int64) bool {
	if d.capacityBytes <= 0 || d.enospcOnSync {
		return false
	}
	if newLen <= oldLen {
		return false
	}
	return d.totalBytesLocked()-oldLen+newLen > d.capacityBytes
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
		// A freshly linked name's dirent is not yet durable: it survives a
		// crash only after a DirSync of its parent directory. The exception is
		// a root-level file (parent ".") such as the WAL: those are governed by
		// the long-standing data-durability model (per-Sync fault), not the
		// dirent model the snapshot/csrfile publish protocol exercises, so they
		// are treated as durably linked on creation to keep WAL-only crash
		// recovery byte-compatible with Phase 2.
		f = &simFile{faulted: make(map[int]bool), direntDurable: isRootLevel(path)}
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

// Rename atomically moves oldPath to newPath, replacing any existing
// destination. It handles both a single file (oldPath is a file key) and a
// directory (oldPath has child keys prefixed oldPath+"/"): the snapshot
// publish protocol renames a whole staging directory onto the live name, so a
// directory rename must move every child key, re-rooting its prefix. The moved
// dirent(s) become NOT durable — only a [SimDisk.DirSync] of the new parent
// makes the new name crash-survivable — which is what lets the simulator crash
// in the publish window between the rename and the parent-dir fsync. It returns
// an error wrapping fs.ErrNotExist when the source is absent.
func (d *SimDisk) Rename(oldPath, newPath string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if f, ok := d.files[oldPath]; ok {
		// Single-file rename: replace any destination, link the new name with
		// a not-yet-durable dirent (unless root-level — see [isRootLevel] and
		// the rationale in OpenFile — so the WAL's own renames stay governed by
		// the data-durability model, not the dirent model).
		delete(d.files, oldPath)
		f.direntDurable = isRootLevel(newPath)
		d.files[newPath] = f
		return nil
	}

	// Directory rename: move every child key under oldPath/ to newPath/,
	// preserving each child's own dirent durability — children travel inside
	// the moved directory inode, so a prior DirSync(oldPath) that made them
	// durable still holds. What is NOT yet durable is the directory's own name
	// in its parent: tracked in d.dirs, set false here and made durable only
	// by a later ParentDirSync(newPath). A crash before that parent fsync
	// drops the whole subtree (the rename is lost), exactly the window the
	// publish protocol's post-rename parent fsync closes.
	prefix := oldPath + "/"
	moved := make(map[string]*simFile)
	for p, f := range d.files {
		if strings.HasPrefix(p, prefix) {
			moved[newPath+"/"+p[len(prefix):]] = f
		}
	}
	if len(moved) == 0 {
		return &fs.PathError{Op: "rename", Path: oldPath, Err: fs.ErrNotExist}
	}
	// Drop any pre-existing destination subtree (replace semantics).
	d.removeSubtreeLocked(newPath)
	delete(d.dirs, oldPath)
	for p := range d.files {
		if strings.HasPrefix(p, prefix) {
			delete(d.files, p)
		}
	}
	for np, f := range moved {
		d.files[np] = f
	}
	// The directory's own dirent under its parent is not durable until a
	// ParentDirSync(newPath); root-level directories are durably linked on
	// creation, mirroring root-level files (see [isRootLevel]).
	d.dirs[newPath] = isRootLevel(newPath)
	return nil
}

// removeSubtreeLocked deletes path and every key under path/. The caller holds
// d.mu.
func (d *SimDisk) removeSubtreeLocked(path string) {
	delete(d.files, path)
	prefix := path + "/"
	for p := range d.files {
		if strings.HasPrefix(p, prefix) {
			delete(d.files, p)
		}
	}
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

// RemoveAll deletes path and every file under path/. Removing an absent path is
// a no-op, matching os.RemoveAll and the staging/backup cleanup the snapshot
// writer relies on.
func (d *SimDisk) RemoveAll(path string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removeSubtreeLocked(path)
	return nil
}

// Stat reports a minimal [fs.FileInfo] for a file at path. It returns an error
// wrapping fs.ErrNotExist when no file is present, which is exactly the probe
// the snapshot and recovery paths rely on (testing for manifest.json / wal
// presence). Directories have no standalone entry in the opaque-key model;
// Stat reports a directory when any child key is prefixed path+"/".
func (d *SimDisk) Stat(path string) (fs.FileInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f, ok := d.files[path]; ok {
		return simFileInfo{name: baseName(path), size: int64(len(f.data))}, nil
	}
	prefix := path + "/"
	for p := range d.files {
		if strings.HasPrefix(p, prefix) {
			return simFileInfo{name: baseName(path), dir: true}, nil
		}
	}
	return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrNotExist}
}

// ReadFile returns a copy of the whole contents of the file at path, or an
// error wrapping fs.ErrNotExist when absent. The copy keeps the returned slice
// independent of later writes, mirroring os.ReadFile.
func (d *SimDisk) ReadFile(path string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	out := make([]byte, len(f.data))
	copy(out, f.data)
	return out, nil
}

// TruncatePath resizes the file at path to size bytes (zero-filling on grow),
// the path-based analogue of os.Truncate used by the csrfile writer. It returns
// an error wrapping fs.ErrNotExist when the file is absent.
func (d *SimDisk) TruncatePath(path string, size int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	f, ok := d.files[path]
	if !ok {
		return &fs.PathError{Op: "truncate", Path: path, Err: fs.ErrNotExist}
	}
	if size < 0 {
		return errors.New("sim: negative truncate size")
	}
	if size <= int64(len(f.data)) {
		f.data = f.data[:size]
		return nil
	}
	if d.wouldExceedLocked(int64(len(f.data)), size) {
		return enospc("truncate", path)
	}
	grown := make([]byte, size)
	copy(grown, f.data)
	f.data = grown
	return nil
}

// DirSync makes every dirent in directory dir durable: it is the in-memory
// analogue of fsync(2) on a directory descriptor. The snapshot/csrfile publish
// protocol calls it on the staging directory before the publish rename and on
// the parent directory after it; only after DirSync does a freshly created or
// renamed name survive a [SimDisk.Crash]. A DirSync of a directory with no
// entries is a harmless no-op (it models fsync of an empty directory).
func (d *SimDisk) DirSync(dir string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for p, f := range d.files {
		if pathpkg.Dir(p) == dir {
			f.direntDurable = true
		}
	}
	// A directory whose parent is dir has its own name made durable too: this
	// is how the post-publish ParentDirSync(live) durabilises the live
	// snapshot directory's name.
	for dp := range d.dirs {
		if pathpkg.Dir(dp) == dir {
			d.dirs[dp] = true
		}
	}
	return nil
}

// ParentDirSync makes the dirent of childPath durable by DirSyncing its parent
// directory. It is the analogue of the post-rename parent-directory fsync the
// publish protocols issue.
func (d *SimDisk) ParentDirSync(childPath string) error {
	return d.DirSync(pathpkg.Dir(childPath))
}

// Crash models a host crash / kill -9: it drops every dirent that is not yet
// durable, exactly as a real crash within the kernel writeback window loses a
// create or rename whose parent directory was never fsync'd. A name becomes
// durable only after a [SimDisk.DirSync] of its parent; until then it is the
// load-bearing job of the publish protocol's directory fsyncs to make the name
// survive, and removing one of those fsyncs makes the corresponding name vanish
// here — which is what the non-vacuity guard test asserts. File DATA that was
// never Sync'd is handled separately by the per-Sync fault and the torn-write
// sectors; Crash only revokes the not-yet-durable name links.
//
// Crash mutates the SimDisk in place and is driven from the single simulation
// goroutine; it must not run concurrently with disk I/O.
func (d *SimDisk) Crash() {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Drop directories whose own dirent never became durable, taking their
	// whole subtree with them (a lost directory rename loses every child).
	for dp, durable := range d.dirs {
		if !durable {
			d.removeSubtreeLocked(dp)
			delete(d.dirs, dp)
		}
	}
	// Drop individual files whose dirent never became durable.
	for p, f := range d.files {
		if !f.direntDurable {
			delete(d.files, p)
		}
	}
}

// baseName returns the final path element, for the synthetic [fs.FileInfo].
func baseName(p string) string { return pathpkg.Base(p) }

// isRootLevel reports whether p sits at the filesystem root (its parent is "."
// or "/" or itself). Root-level names — notably the WAL at [simWALPath] — are
// treated as durably linked on creation, so the long-standing WAL data-
// durability model (per-Sync fault) is unaffected by the dirent model the
// snapshot/csrfile publish protocol exercises in subdirectories.
func isRootLevel(p string) bool {
	dir := pathpkg.Dir(p)
	return dir == "." || dir == "/" || dir == p
}

// simFileInfo is the minimal [fs.FileInfo] SimDisk.Stat returns: callers only
// consult existence, Size, and IsDir.
type simFileInfo struct {
	name string
	size int64
	dir  bool
}

func (fi simFileInfo) Name() string { return fi.name }
func (fi simFileInfo) Size() int64  { return fi.size }
func (fi simFileInfo) Mode() fs.FileMode {
	if fi.dir {
		return fs.ModeDir | 0o755
	}
	return 0o600
}
func (fi simFileInfo) ModTime() time.Time { return time.Time{} }
func (fi simFileInfo) IsDir() bool        { return fi.dir }
func (fi simFileInfo) Sys() any           { return nil }

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

// Stat returns a minimal [fs.FileInfo] for the open handle, reporting the
// current file size. The snapshot index writer calls it to record the
// component's on-disk size in the manifest.
func (h *SimFileHandle) Stat() (fs.FileInfo, error) {
	if h.closed {
		return nil, fs.ErrClosed
	}
	h.disk.mu.Lock()
	defer h.disk.mu.Unlock()
	return simFileInfo{name: baseName(h.path), size: int64(len(h.file.data))}, nil
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
	oldLen := int64(len(h.file.data))
	if end > oldLen {
		// Disk full: in eager mode a growing write that would breach the budget
		// returns ENOSPC and grows nothing (no partial write), matching real
		// allocate-on-write and the internal/testfs ReturnENOSPC contract.
		if h.disk.wouldExceedLocked(oldLen, end) {
			return 0, enospc("write", h.path)
		}
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
		if h.disk.wouldExceedLocked(int64(len(h.file.data)), size) {
			return enospc("truncate", h.path)
		}
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
	// Delayed-allocation disk-full: the bytes were buffered by Write but the
	// backing blocks cannot be allocated, so the out-of-space condition only
	// surfaces here at fsync. Checked before the seed draw and gated on a
	// non-zero capacity, so the default (capacity 0) Sync fault stream is
	// unchanged.
	if h.disk.enospcOnSync && h.disk.capacityBytes > 0 && h.disk.totalBytesLocked() > h.disk.capacityBytes {
		return enospc("fsync", h.path)
	}
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
