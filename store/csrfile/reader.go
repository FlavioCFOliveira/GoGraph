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
	mu          sync.RWMutex
	f           *os.File
	mm          mmap.MMap
	header      Header
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

	r := &Reader{f: f, mm: mm, header: h}
	r.bindSlices()
	return r, nil
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
		bytes := r.mm[off : off+8*r.header.NVertices]
		r.vertices = unsafe.Slice((*uint64)(unsafe.Pointer(&bytes[0])), r.header.NVertices) //nolint:gosec // intentional zero-copy reinterpretation of mmap region
	}
	if r.header.NEdges > 0 {
		off := r.header.EdgesOffset
		bytes := r.mm[off : off+8*r.header.NEdges]
		r.edges = unsafe.Slice((*graph.NodeID)(unsafe.Pointer(&bytes[0])), r.header.NEdges) //nolint:gosec // intentional zero-copy reinterpretation of mmap region
	}
	if r.header.Weight != WeightAbsent && r.header.NEdges > 0 {
		off := r.header.WeightsOffset
		size := uint64(r.header.Weight.Size()) * r.header.NEdges
		r.weightBytes = r.mm[off : off+size]
	}
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
	if r.mm == nil {
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
	if r.mm == nil {
		return ErrReaderClosed
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
	if r.mm == nil {
		return nil
	}
	err := r.mm.Unmap()
	r.mm = nil
	if cerr := r.f.Close(); cerr != nil && err == nil {
		err = cerr
	}
	return err
}
