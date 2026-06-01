package csrfile

import (
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
)

// TestReinterpret_ZeroCopy asserts two properties of [Reinterpret]:
//
//  1. AllocsPerRun == 0: the function must not allocate heap memory.
//  2. The returned slice's data pointer lies within the source buffer —
//     confirming the conversion is a view, not a copy.
//
// The test uses [Reader.Edges] (already a zero-copy view of the mmap
// region) as its input so the pointer-range check verifies that
// Reinterpret's result still lives inside the mapped file. Content
// correctness and roundtrip semantics are covered by
// TestReinterpret_Uint64Roundtrip and TestReader_ZeroCopyRoundTrip.
func TestReinterpret_ZeroCopy(t *testing.T) {
	// AllocsPerRun requires a serial test — t.Parallel() is intentionally
	// absent because testing.AllocsPerRun panics when called inside a
	// parallel subtest.

	// Build a small grid graph so both vertex and edge sections are
	// populated in the written file.
	g, err := shapegen.Grid(5, 5, false).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Grid.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())
	if c.Size() == 0 {
		t.Fatal("csr Size = 0; need at least one edge for this test")
	}

	path := filepath.Join(t.TempDir(), "reinterpret.csr")
	if _, err := WriteToFile[int64](path, c); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}

	r, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = r.Close() }()

	h := r.Header()
	if h.NEdges == 0 {
		t.Fatal("NEdges = 0 in written file")
	}

	// 1. AllocsPerRun == 0 — Reinterpret must be allocation-free.
	edgeBytes := r.mm[h.EdgesOffset : h.EdgesOffset+8*h.NEdges]
	n := testing.AllocsPerRun(100, func() {
		_ = Reinterpret[uint64](edgeBytes, int(h.NEdges))
	})
	if n > 0 {
		t.Errorf("Reinterpret allocs = %v, want 0", n)
	}

	// 2. The resulting slice's data pointer must lie within the mmap region.
	got := Reinterpret[uint64](edgeBytes, int(h.NEdges))
	if len(got) == 0 {
		t.Fatal("Reinterpret returned empty slice")
	}
	gotPtr := uintptr(unsafe.Pointer(&got[0]))  //nolint:gosec // intentional: verifying zero-copy aliasing
	mmBase := uintptr(unsafe.Pointer(&r.mm[0])) //nolint:gosec // intentional: verifying zero-copy aliasing
	mmEnd := mmBase + uintptr(len(r.mm))
	if gotPtr < mmBase || gotPtr >= mmEnd {
		t.Errorf("Reinterpret result data pointer %x not in mmap [%x, %x)",
			gotPtr, mmBase, mmEnd)
	}
}
