package csrfile

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildSmallCSRFile writes a deterministic 3-vertex, 2-edge (unweighted)
// CSR file to dir and returns the raw bytes together with the parsed
// header. The topology is: 0→1, 0→2.
//
// VerticesSlice has length 4 (NVertices = 4 in the sentinel-inclusive
// header): [0, 2, 2, 2], where the last entry is the sentinel = NEdges.
// EdgesSlice has length 2: [1, 2].
func buildSmallCSRFile(t *testing.T) (path string, data []byte, h Header) {
	t.Helper()
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for _, src := range []int{0, 1, 2} {
		if err := a.AddNode(src); err != nil {
			t.Fatalf("AddNode(%d): %v", src, err)
		}
	}
	if err := a.AddEdge(0, 1, struct{}{}); err != nil {
		t.Fatalf("AddEdge(0,1): %v", err)
	}
	if err := a.AddEdge(0, 2, struct{}{}); err != nil {
		t.Fatalf("AddEdge(0,2): %v", err)
	}
	c := csr.BuildFromAdjList(a)
	path = filepath.Join(t.TempDir(), "small.csr")
	hdr, err := WriteToFile(path, c)
	if err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	return path, raw, hdr
}

// patchAndRecomputeCRC patches data[off:off+8] with val (little-endian
// uint64) and rewrites the trailing CRC32c so the file still passes the
// CRC check in Open. This lets the semantic-invariant tests exercise
// specifically the new validateCSRSemantics gate, not the CRC gate.
func patchAndRecomputeCRC(t *testing.T, data []byte, h Header, off, val uint64) []byte {
	t.Helper()
	patched := make([]byte, len(data))
	copy(patched, data)
	binary.LittleEndian.PutUint64(patched[off:off+8], val)
	crc := crc32.Update(0, crc32.MakeTable(crc32.Castagnoli), patched[:h.TailCRCOffset])
	binary.LittleEndian.PutUint32(patched[h.TailCRCOffset:], crc)
	return patched
}

// writePatched saves patched bytes to a temp file and returns the path.
func writePatched(t *testing.T, patched []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "patched.csr")
	if err := os.WriteFile(p, patched, 0o600); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
	return p
}

// TestOpen_NonMonotoneOffsets writes a 3-vertex, 2-edge CSR file, then
// patches vertices[1] < vertices[0] so the offset array is no longer
// monotone non-decreasing. Open must return ErrFileCorrupted.
//
// Before the fix, Open succeeded and downstream algorithms would read
// past the edge array or underflow a uint64 subtraction.
func TestOpen_NonMonotoneOffsets(t *testing.T) {
	t.Parallel()
	_, data, h := buildSmallCSRFile(t)

	// vertices slice starts at h.VerticesOffset.
	// Layout: [verts[0], verts[1], verts[2], verts[3]=sentinel].
	// We want verts[1] < verts[0]. verts[0] is currently 0; set it to 2
	// (the out-degree of vertex 0) and then set verts[1] to 1, which is
	// less than 2 — breaking monotonicity.
	//
	// Patch verts[0] = 2 so it is non-zero.
	v0off := h.VerticesOffset
	patched := patchAndRecomputeCRC(t, data, h, v0off, 2)
	// Now patch verts[1] = 1 < verts[0]=2.
	v1off := h.VerticesOffset + 8
	patched = patchAndRecomputeCRC(t, patched, h, v1off, 1)

	p := writePatched(t, patched)
	_, err := Open(p)
	if !errors.Is(err, ErrFileCorrupted) {
		t.Fatalf("expected errors.Is(err, ErrFileCorrupted), got %v", err)
	}
}

// TestOpen_FinalOffsetExceedsEdgeCount writes a 3-vertex, 2-edge CSR
// file, then patches the sentinel vertex entry (vertices[NVertices-1])
// to a value greater than NEdges. Open must return ErrFileCorrupted.
//
// Before the fix, Open succeeded; algorithms would access edges[k] for
// k >= NEdges, reading past the allocated slice.
func TestOpen_FinalOffsetExceedsEdgeCount(t *testing.T) {
	t.Parallel()
	_, data, h := buildSmallCSRFile(t)

	// Sentinel is at index NVertices-1; its byte offset is:
	// VerticesOffset + 8*(NVertices-1).
	sentinelOff := h.VerticesOffset + 8*(h.NVertices-1)
	// Set sentinel to NEdges+1, which is strictly greater than NEdges.
	patched := patchAndRecomputeCRC(t, data, h, sentinelOff, h.NEdges+1)

	p := writePatched(t, patched)
	_, err := Open(p)
	if !errors.Is(err, ErrFileCorrupted) {
		t.Fatalf("expected errors.Is(err, ErrFileCorrupted), got %v", err)
	}
}

// TestOpen_EdgeTargetOutOfRange writes a 3-vertex, 2-edge CSR file,
// then patches edges[0] to a value >= the actual vertex count
// (NVertices-1). Open must return ErrFileCorrupted.
//
// Before the fix, Open succeeded; algorithms would index out-of-bounds
// arrays such as the rank or out-degree vectors in PageRank.
func TestOpen_EdgeTargetOutOfRange(t *testing.T) {
	t.Parallel()
	_, data, h := buildSmallCSRFile(t)

	// Patch edges[0] to NVertices (which equals the actual vertex count
	// because NVertices is sentinel-inclusive, so valid targets are
	// [0, NVertices-1)). NVertices itself is out of range.
	e0off := h.EdgesOffset
	patched := patchAndRecomputeCRC(t, data, h, e0off, h.NVertices)

	p := writePatched(t, patched)
	_, err := Open(p)
	if !errors.Is(err, ErrFileCorrupted) {
		t.Fatalf("expected errors.Is(err, ErrFileCorrupted), got %v", err)
	}
}
