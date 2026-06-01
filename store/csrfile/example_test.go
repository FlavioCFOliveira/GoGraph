package csrfile_test

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/store/csrfile"
)

// Example writes a CSR snapshot to an on-disk Tier 2 file and reads it
// back through the mmap-backed Reader, inspecting the header and the
// stored edge weights without re-parsing the body.
func Example() {
	dir, err := os.MkdirTemp("", "csrfile-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	// Build a small weighted directed graph and freeze it into an
	// immutable CSR snapshot.
	a := adjlist.New[string, int64](adjlist.Config{Directed: true})
	for _, e := range []struct {
		src, dst string
		w        int64
	}{
		{"a", "b", 10},
		{"a", "c", 20},
		{"b", "c", 30},
	} {
		if err := a.AddEdge(e.src, e.dst, e.w); err != nil {
			panic(err)
		}
	}
	c := csr.BuildFromAdjList(a)

	// Write the CSR to disk; publication is atomic (write .tmp, fsync,
	// rename).
	path := filepath.Join(dir, "graph.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		panic(err)
	}

	// Open mmaps the file read-only and verifies the header and tail
	// CRC. The returned slices alias the mapped region and stay valid
	// until Close.
	r, err := csrfile.Open(path)
	if err != nil {
		panic(err)
	}
	defer func() { _ = r.Close() }()

	// NEdges is the live edge count. NVertices is the length of the
	// dense vertex-offset (row-pointer) array, which is sized by the
	// largest interned NodeID rather than by the number of distinct
	// keys, so it is not asserted here; csr.CSR.Order reports the live
	// vertex count when that is what you need.
	h := r.Header()
	fmt.Printf("edges=%d\n", h.NEdges)

	weights, ok := r.WeightsUint64()
	fmt.Printf("weights present=%t count=%d\n", ok, len(weights))

	// The mmapped edge slice has one entry per stored edge.
	fmt.Printf("edge slice len=%d\n", len(r.Edges()))

	// Output:
	// edges=3
	// weights present=true count=3
	// edge slice len=3
}
