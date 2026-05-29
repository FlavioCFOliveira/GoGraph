package extern_test

import (
	"fmt"
	"os"
	"path/filepath"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/extern"
	"gograph/store/csrfile"
)

// writeDiamond builds the directed diamond 0->{1,2}->3, serialises it
// to a Tier 2 csrfile in a fresh temp directory, and returns an open
// mmap-backed reader, the mapper, and a cleanup func the caller must
// defer. The reader streams adjacency from the mapped file; extern's
// algorithms never materialise the CSR in memory.
func writeDiamond() (*csrfile.Reader, *graph.Mapper[int], func()) {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	for _, e := range [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}} {
		_ = a.AddEdge(e[0], e[1], struct{}{})
	}
	c := csr.BuildFromAdjList(a)

	dir, err := os.MkdirTemp("", "extern-example-")
	if err != nil {
		panic(err)
	}
	path := filepath.Join(dir, "diamond.csr")
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		_ = os.RemoveAll(dir)
		panic(err)
	}
	r, err := csrfile.Open(path)
	if err != nil {
		_ = os.RemoveAll(dir)
		panic(err)
	}
	cleanup := func() {
		_ = r.Close()
		_ = os.RemoveAll(dir)
	}
	return r, a.Mapper(), cleanup
}

// ExampleBFS runs a semi-external breadth-first traversal directly over
// an mmap-backed csrfile.Reader, reporting the depth at which each node
// is reached. Adjacency is streamed from the file; only the visited
// set and frontiers live in RAM.
func ExampleBFS() {
	r, m, cleanup := writeDiamond()
	defer cleanup()

	src, _ := m.Lookup(0)
	depth := map[int]int{}
	if err := extern.BFS(r, src, func(node graph.NodeID, d int) bool {
		v, _ := m.Resolve(node)
		depth[v] = d
		return true // keep traversing
	}); err != nil {
		fmt.Println("error:", err)
		return
	}

	for v := 0; v < 4; v++ {
		fmt.Printf("node %d at depth %d\n", v, depth[v])
	}
	// Output:
	// node 0 at depth 0
	// node 1 at depth 1
	// node 2 at depth 1
	// node 3 at depth 2
}

// ExamplePageRank runs PageRank over the same mmap-backed reader. The
// rank vector lives in RAM while edges stream from the file each
// iteration; the ranks sum to 1.0, and the sink (node 3, the only
// destination with no out-edges) carries more mass than the source.
func ExamplePageRank() {
	r, m, cleanup := writeDiamond()
	defer cleanup()

	ranks, _, err := extern.PageRank(r, extern.DefaultPageRankOptions())
	if err != nil {
		fmt.Println("error:", err)
		return
	}

	var total float64
	for _, x := range ranks {
		total += x
	}
	src, _ := m.Lookup(0)
	sink, _ := m.Lookup(3)

	fmt.Printf("ranks sum to 1.0: %v\n", total > 0.999 && total < 1.001)
	fmt.Printf("sink outranks source: %v\n", ranks[sink] > ranks[src])
	// Output:
	// ranks sum to 1.0: true
	// sink outranks source: true
}
