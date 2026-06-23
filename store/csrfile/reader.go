package csrfile

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"sync"
	"unsafe"

	"github.com/edsrzf/mmap-go"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// AccessPattern is the advisory hint given to the OS about the
// expected memory-access pattern of a mapped section.
type AccessPattern uint8

// Supported access patterns.
const (
	AccessDefault AccessPattern = iota
	AccessSequential
	AccessRandom
	AccessWillNeed
	AccessDontNeed
)

// Reader is a read-only, mmap-backed view of a csrfile.
//
// All slices returned by [Reader.Vertices] / [Reader.Edges] /
// [Reader.WeightsRaw] alias the underlying mmap region — they must
// not be mutated and remain valid only for as long as the mapping is
// live. [Reader.Close] unmaps the region, after which any retained
// slice is a dangling view into freed memory; reading it is a
// use-after-unmap fault, not a recoverable Go panic.
//
// Concurrency. A Reader is safe for concurrent use, but a long-lived
// reader that binds the slices once and iterates them MUST do so
// inside [Reader.Read]: Read holds an internal read lock for the
// whole duration of the callback, and Close blocks until every
// in-flight Read has returned before it unmaps. Calling the bare
// accessors ([Reader.Vertices] etc.) and then iterating outside Read
// races a concurrent Close and may fault the process; the accessors
// are retained only for short, non-concurrent inspection.
type Reader struct {
	// mu guards mm and the bound slices against a concurrent Close.
	// Read takes it for reading (callers may run in parallel); Close
	// takes it for writing, so the Unmap cannot run while any Read is
	// in flight and no reader can observe a half-released mapping.
	mu     sync.RWMutex
	f      *os.File
	mm     mmap.MMap
	header Header
	// buf is the backing region. For an mmap-backed Reader (the
	// production [Open] path) it aliases mm. For a byte-backed Reader
	// (the in-memory DST backend, via [openBytes]) it is a heap buffer
	// whose base address is 8-byte aligned so the zero-copy
	// reinterpretation in bindSlices stays sound. mm is then nil and
	// Close releases nothing but the slices.
	buf         []byte
	vertices    []uint64
	edges       []graph.NodeID
	weightBytes []byte
}

// Open mmaps path read-only, verifies the header and the tail CRC,
// and returns a Reader pointing into the mapped region.
func Open(path string) (*Reader, error) {
	f, err := os.Open(path) //nolint:gosec // caller-supplied path
	if err != nil {
		return nil, err
	}
	mm, err := mmap.Map(f, mmap.RDONLY, 0)
	if err != nil {
		_ = f.Close() // best-effort: already on error path, mmap err preserved
		return nil, fmt.Errorf("csrfile: mmap: %w", err)
	}
	if len(mm) < HeaderSize+4 {
		_ = mm.Unmap() // best-effort: already on error path, header-size err preserved
		_ = f.Close()  // best-effort: already on error path, header-size err preserved
		return nil, ErrHeaderTooShort
	}
	h, err := DecodeHeader(mm)
	if err != nil {
		_ = mm.Unmap() // best-effort: already on error path, decode err preserved
		_ = f.Close()  // best-effort: already on error path, decode err preserved
		return nil, err
	}
	// Structural validation MUST precede any use of the header's
	// offsets. It proves every section lies wholly within the mapped
	// region and that the offsets match the one canonical layout, so
	// the CRC slice below and the zero-copy reinterpretation in
	// bindSlices are both in-bounds by construction. Without this, a
	// hostile TailCRCOffset would panic on mm[h.TailCRCOffset:], and a
	// hostile count/offset (including the 8*NEdges integer-overflow
	// wrap) would make bindSlices read past the mmap region.
	if err := h.validate(len(mm)); err != nil {
		_ = mm.Unmap() // best-effort: already on error path, validate err preserved
		_ = f.Close()  // best-effort: already on error path, validate err preserved
		return nil, err
	}
	gotCRC := binary.LittleEndian.Uint32(mm[h.TailCRCOffset:])
	wantCRC := crc32.Update(0, castagnoli, mm[:h.TailCRCOffset])
	if gotCRC != wantCRC {
		_ = mm.Unmap() // best-effort: already on error path, CRC-mismatch err preserved
		_ = f.Close()  // best-effort: already on error path, CRC-mismatch err preserved
		return nil, fmt.Errorf("%w: crc32c", ErrFileCorrupted)
	}

	r := &Reader{f: f, mm: mm, buf: mm, header: h}
	r.bindSlices()
	if err := r.validateCSRSemantics(); err != nil {
		_ = mm.Unmap() // best-effort: already on error path, semantic err preserved
		_ = f.Close()  // best-effort: already on error path, semantic err preserved
		return nil, err
	}
	return r, nil
}

// OpenWith opens a csrfile through a caller-supplied filesystem backend.
// For the production OS backend it is byte-for-byte equivalent to [Open]
// (it mmaps the file). For an alternate (in-memory) backend — supplied by
// the deterministic-simulation harness (internal/sim) — it reads the whole
// file into an 8-byte-aligned buffer and binds the typed slices off it,
// because there is no real file to mmap. The backend parameter type is
// unexported, mirroring
// [github.com/FlavioCFOliveira/GoGraph/store/wal.OpenWith]; production code
// calls [Open].
func OpenWith(fsys fs, path string) (*Reader, error) {
	if isOS(fsys) {
		return Open(path)
	}
	raw, err := fsys.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Copy into an 8-byte-aligned buffer: the backend's ReadFile makes no
	// alignment promise, and bindSlices reinterprets the buffer in place.
	buf := allocAligned8(len(raw))
	copy(buf, raw)
	return openBytes(buf)
}

// openBytes builds a Reader over an already-read, 8-byte-aligned byte
// buffer instead of an mmap. It runs the identical header/CRC/semantic
// validation [Open] runs over the mmap region, then binds the typed
// slices off buf. It backs the in-memory DST filesystem, where there is
// no real file to mmap: the simulator reads the whole csrfile image out
// of its [github.com/FlavioCFOliveira/GoGraph/internal/sim.SimDisk] and
// hands the bytes here.
//
// Precondition: buf's base address is 8-byte aligned (the only callers
// allocate it via [allocAligned8]). The bind below reuses the same
// unsafe reinterpretation as the mmap path, so an unaligned buffer would
// trip the alignment assertion the production layout already guarantees.
func openBytes(buf []byte) (*Reader, error) {
	if len(buf) < HeaderSize+4 {
		return nil, ErrHeaderTooShort
	}
	h, err := DecodeHeader(buf)
	if err != nil {
		return nil, err
	}
	if err := h.validate(len(buf)); err != nil {
		return nil, err
	}
	gotCRC := binary.LittleEndian.Uint32(buf[h.TailCRCOffset:])
	wantCRC := crc32.Update(0, castagnoli, buf[:h.TailCRCOffset])
	if gotCRC != wantCRC {
		return nil, fmt.Errorf("%w: crc32c", ErrFileCorrupted)
	}
	r := &Reader{buf: buf, header: h}
	r.bindSlices()
	if err := r.validateCSRSemantics(); err != nil {
		return nil, err
	}
	return r, nil
}

// allocAligned8 returns a zeroed byte slice of length n whose base
// address is guaranteed 8-byte aligned, by allocating it as a []uint64
// and reslicing. A plain make([]byte, n) carries no alignment guarantee,
// which would make the zero-copy reinterpretation in bindSlices trip the
// alignment assertion in [Reinterpret] on some allocations; routing the
// in-memory read through this helper keeps the byte-backed Reader sound.
func allocAligned8(n int) []byte {
	words := (n + 7) / 8
	backing := make([]uint64, words)
	return unsafe.Slice((*byte)(unsafe.Pointer(&backing[0])), words*8)[:n] //nolint:gosec // 8-byte-aligned reinterpret of []uint64 backing
}

// bindSlices reinterprets the mmap'd byte regions as typed slices
// without copying. The aliasing is sound because the mmap region
// is read-only and lives at least as long as the Reader.
//
// Precondition: r.header has passed [Header.validate] against
// len(r.mm) (enforced in Open). That guarantees every offset+length
// computed below lies wholly within r.mm, so each slice expression
// and unsafe.Slice view is in-bounds by construction — no slice
// bound here can panic and no view can read past the mapped region.
func (r *Reader) bindSlices() {
	if r.header.NVertices > 0 {
		off := r.header.VerticesOffset
		bytes := r.buf[off : off+8*r.header.NVertices]
		r.vertices = unsafe.Slice((*uint64)(unsafe.Pointer(&bytes[0])), r.header.NVertices) //nolint:gosec // intentional zero-copy reinterpretation of backing region
	}
	if r.header.NEdges > 0 {
		off := r.header.EdgesOffset
		bytes := r.buf[off : off+8*r.header.NEdges]
		r.edges = unsafe.Slice((*graph.NodeID)(unsafe.Pointer(&bytes[0])), r.header.NEdges) //nolint:gosec // intentional zero-copy reinterpretation of backing region
	}
	if r.header.Weight != WeightAbsent && r.header.NEdges > 0 {
		off := r.header.WeightsOffset
		size := uint64(r.header.Weight.Size()) * r.header.NEdges
		r.weightBytes = r.buf[off : off+size]
	}
}

// validateCSRSemantics checks the three semantic invariants every CSR
// consumer assumes after bindSlices has succeeded:
//
//  1. The vertex offset array is monotone non-decreasing.
//  2. The sentinel — vertices[NVertices-1], which records the total edge
//     count — does not exceed NEdges (the edge array's actual length).
//  3. Every edge target is a valid vertex index (< NVertices-1).
//
// The vertex array stored on disk uses the sentinel-inclusive form: it
// has NVertices entries (not NVertices+1), where the last entry equals
// the total edge count. Equivalently, NVertices = V+1, with V being the
// actual number of vertices; edge targets are in [0, V) = [0, NVertices-1).
//
// Called once in Open, immediately after bindSlices; O(V+E).
func (r *Reader) validateCSRSemantics() error {
	nv := r.header.NVertices // sentinel-inclusive length: actual vertices = nv-1
	ne := r.header.NEdges
	verts := r.vertices
	edges := r.edges
	if nv == 0 {
		return nil
	}
	// Invariant 1 + 2: monotone offsets and sentinel bound.
	// verts has length nv; verts[nv-1] is the sentinel == total edge count.
	for i := uint64(1); i < nv; i++ {
		if verts[i] < verts[i-1] {
			return fmt.Errorf("%w: vertex offset [%d]=%d < [%d]=%d (non-monotone)",
				ErrFileCorrupted, i, verts[i], i-1, verts[i-1])
		}
	}
	if verts[nv-1] > ne {
		return fmt.Errorf("%w: sentinel offset [%d]=%d exceeds edge count %d",
			ErrFileCorrupted, nv-1, verts[nv-1], ne)
	}
	// Invariant 3: every edge target is a valid vertex index.
	// Actual vertices occupy indices [0, nv-1); targets must be < nv-1.
	if nv < 2 {
		// nv == 1 means zero actual vertices; no edge can have a valid target.
		// (NEdges == 0 is guaranteed by Invariant 2 when nv == 1, so this
		// loop body is unreachable — but check explicitly for safety.)
		if len(edges) > 0 {
			return fmt.Errorf("%w: edge[0]=%d: no valid vertices (NVertices=%d)",
				ErrFileCorrupted, edges[0], nv)
		}
		return nil
	}
	actualV := nv - 1 // number of real vertices
	for k, dst := range edges {
		if uint64(dst) >= actualV {
			return fmt.Errorf("%w: edge[%d]=%d >= vertex count %d",
				ErrFileCorrupted, k, dst, actualV)
		}
	}
	return nil
}

// Header returns the parsed file header.
func (r *Reader) Header() Header { return r.header }

// Vertices returns the offsets slice. Each entry is the start index
// in [Reader.Edges] of that vertex's out-neighbours.
//
// The returned slice aliases the mmap region and is valid only until
// [Reader.Close]. A consumer that iterates it concurrently with a
// possible Close MUST use [Reader.Read] instead; see the Reader type
// documentation.
func (r *Reader) Vertices() []uint64 { return r.vertices }

// Edges returns the edges slice. The returned slice aliases the mmap
// region and is valid only until [Reader.Close]; long-lived or
// concurrent iteration MUST use [Reader.Read].
func (r *Reader) Edges() []graph.NodeID { return r.edges }

// WeightsRaw returns the raw bytes of the weights section. Use
// [Reader.WeightsUint64] / [Reader.WeightsFloat64] for typed views.
// Returns nil when the file is unweighted. The returned slice aliases
// the mmap region and is valid only until [Reader.Close]; long-lived
// or concurrent iteration MUST use [Reader.Read].
func (r *Reader) WeightsRaw() []byte { return r.weightBytes }

// Read invokes fn with the mmap-aliased vertices, edges and raw
// weights slices while holding an internal read lock for the whole
// duration of the call. The lock keeps the mapping live across the
// entire callback, so fn may safely bind the slices once and iterate
// them: a concurrent [Reader.Close] blocks until fn returns before it
// unmaps, closing the use-after-unmap window that bare accessors plus
// an external iteration would leave open.
//
// The slices passed to fn alias the read-only mmap region; fn must
// not mutate them and must not retain them beyond its own return —
// they are invalid once Read returns. weights is nil when the file is
// unweighted.
//
// Read returns [ErrReaderClosed] if the Reader has already been
// closed; otherwise it returns whatever fn returns. The read lock is
// released even if fn panics. Read is safe for concurrent use; any
// number of Read calls run in parallel.
func (r *Reader) Read(fn func(vertices []uint64, edges []graph.NodeID, weights []byte) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.buf == nil {
		return ErrReaderClosed
	}
	return fn(r.vertices, r.edges, r.weightBytes)
}

// WeightsUint64 returns the weights section as a []uint64 when
// possible. Returns nil, false when the weight kind is not 8-byte
// integer.
func (r *Reader) WeightsUint64() ([]uint64, bool) {
	if r.header.Weight != WeightUint64 || len(r.weightBytes) == 0 {
		return nil, false
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&r.weightBytes[0])), r.header.NEdges), true //nolint:gosec // intentional zero-copy reinterpretation of mmap region
}

// WeightsFloat64 returns the weights section as a []float64 when
// possible.
func (r *Reader) WeightsFloat64() ([]float64, bool) {
	if r.header.Weight != WeightFloat64 || len(r.weightBytes) == 0 {
		return nil, false
	}
	return unsafe.Slice((*float64)(unsafe.Pointer(&r.weightBytes[0])), r.header.NEdges), true //nolint:gosec // intentional zero-copy reinterpretation of mmap region
}

// SetHint applies an OS-level advisory hint to the mapped region
// describing the expected access pattern. On Linux it issues
// madvise; on other platforms the call returns nil and is a no-op
// at the OS level (Go's mmap-go does not expose madvise on every
// platform).
//
// SetHint holds the read lock across the OS call, so it cannot race
// a concurrent [Reader.Close] (which would otherwise unmap the region
// the madvise syscall is about to touch). It returns [ErrReaderClosed]
// if the Reader is already closed. Safe for concurrent use.
func (r *Reader) SetHint(pattern AccessPattern) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.buf == nil {
		return ErrReaderClosed
	}
	// A byte-backed Reader (the in-memory DST backend) has no mmap to
	// advise: madvise is meaningless on a heap buffer, so the hint is a
	// no-op there, exactly as it already is on platforms without madvise.
	if r.mm == nil {
		return nil
	}
	return r.setHint(pattern)
}

// Close releases the mmap and underlying file. Any slice returned by
// the Reader becomes invalid.
//
// Close acquires the Reader's write lock, so it blocks until every
// in-flight [Reader.Read] has returned before unmapping — no reader
// can be iterating the mapping at the moment it is released. Close is
// idempotent: the second and later calls observe the already-released
// mapping and return nil without unmapping twice. Close is safe to
// call concurrently from multiple goroutines.
func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.buf == nil {
		return nil
	}
	// Byte-backed Reader (in-memory DST backend): no mmap, no file. Drop
	// the buffer reference so the slices become unreachable and a second
	// Close is the documented idempotent no-op.
	if r.mm == nil {
		r.buf = nil
		r.vertices = nil
		r.edges = nil
		r.weightBytes = nil
		return nil
	}
	err := r.mm.Unmap()
	r.mm = nil
	r.buf = nil
	if cerr := r.f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
