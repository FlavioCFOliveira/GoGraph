package csrfile

import (
	"path/filepath"
	"sort"
	"testing"
	"unsafe"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/internal/shapegen"
)

// TestReader_ZeroCopyRoundTrip verifies that:
//  1. The edges slice returned by Reader.Edges() aliases the mmap region
//     directly (no copy).
//  2. The neighbour list for every node matches the in-memory CSR.
//  3. A second Open produces identical results.
//  4. Reader.Close returns no error.
func TestReader_ZeroCopyRoundTrip(t *testing.T) {
	t.Parallel()

	// Build Grid(10, 10, false): undirected, 100 nodes.
	g, err := shapegen.Grid(10, 10, false).Build(adjlist.Config{})
	if err != nil {
		t.Fatalf("Grid.Build: %v", err)
	}
	c := csr.BuildFromAdjList(g.AdjList())

	path := filepath.Join(t.TempDir(), "grid.csr")
	if _, err := WriteToFile[int64](path, c); err != nil {
		t.Fatalf("WriteToFile: %v", err)
	}

	openAndVerify := func(label string) {
		r, err := Open(path)
		if err != nil {
			t.Fatalf("[%s] Open: %v", label, err)
		}
		defer func() {
			if err := r.Close(); err != nil {
				t.Errorf("[%s] Close: %v", label, err)
			}
		}()

		// 1. Check that the edges data pointer lies within the mmap region.
		edgesSlice := r.Edges()
		if len(edgesSlice) > 0 && len(r.mm) > 0 {
			edgePtr := uintptr(unsafe.Pointer(&edgesSlice[0])) //nolint:gosec // intentional: verifying mmap aliasing
			mmBase := uintptr(unsafe.Pointer(&r.mm[0]))        //nolint:gosec // intentional: verifying mmap aliasing
			mmEnd := mmBase + uintptr(len(r.mm))
			if edgePtr < mmBase || edgePtr >= mmEnd {
				t.Errorf("[%s] edges slice data pointer %x not in mmap [%x, %x)", label, edgePtr, mmBase, mmEnd)
			}
		}

		// 2. Verify neighbour lists match in-memory CSR for every node.
		fileVerts := r.Vertices()
		fileEdges := r.Edges()
		csrVerts := c.VerticesSlice()

		for id := graph.NodeID(0); id+1 < graph.NodeID(len(csrVerts)); id++ {
			// Collect neighbours from file reader.
			fileStart := fileVerts[id]
			var fileEnd uint64
			if int(id)+1 < len(fileVerts) {
				fileEnd = fileVerts[id+1]
			} else {
				fileEnd = fileStart
			}
			fileNbrs := make([]int, 0, fileEnd-fileStart)
			for i := fileStart; i < fileEnd; i++ {
				fileNbrs = append(fileNbrs, int(fileEdges[i]))
			}

			// Collect neighbours from in-memory CSR.
			memStart := csrVerts[id]
			var memEnd uint64
			if int(id)+1 < len(csrVerts) {
				memEnd = csrVerts[id+1]
			} else {
				memEnd = memStart
			}
			csrEdges := c.EdgesSlice()
			memNbrs := make([]int, 0, memEnd-memStart)
			for i := memStart; i < memEnd; i++ {
				memNbrs = append(memNbrs, int(csrEdges[i]))
			}

			sort.Ints(fileNbrs)
			sort.Ints(memNbrs)
			if len(fileNbrs) != len(memNbrs) {
				t.Errorf("[%s] node %d: file neighbours count=%d, csr count=%d",
					label, id, len(fileNbrs), len(memNbrs))
				continue
			}
			for k := range fileNbrs {
				if fileNbrs[k] != memNbrs[k] {
					t.Errorf("[%s] node %d: neighbour[%d] file=%d csr=%d",
						label, id, k, fileNbrs[k], memNbrs[k])
				}
			}
		}
	}

	openAndVerify("first-open")
	openAndVerify("second-open")
}
