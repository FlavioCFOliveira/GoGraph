package bulk_test

import (
	"fmt"
	"os"
	"path/filepath"

	"gograph/store/bulk"
	"gograph/store/csrfile"
)

// Example bulk-loads a small graph: edges are streamed through a Loader
// that bypasses the transactional WAL stack and writes a Tier 2 csrfile
// directly. Finalise returns the row count and the in-memory CSR; the
// file is then reopened to confirm it landed on disk.
func Example() {
	dir, err := os.MkdirTemp("", "bulk-example")
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	out := filepath.Join(dir, "graph.csr")
	l := bulk.New(bulk.Options{OutputPath: out, Directed: true})

	// Feed edges one at a time and in a batch; both paths funnel into
	// the same in-memory adjacency list.
	if err := l.Add(bulk.Edge{Src: "a", Dst: "b", Weight: 1}); err != nil {
		panic(err)
	}
	if err := l.AddBatch([]bulk.Edge{
		{Src: "b", Dst: "c", Weight: 2},
		{Src: "c", Dst: "a", Weight: 3},
	}); err != nil {
		panic(err)
	}

	// Finalise flushes the accumulated edges to the csrfile and returns
	// the row count plus the resulting CSR snapshot.
	rows, c, err := l.Finalise()
	if err != nil {
		panic(err)
	}
	fmt.Printf("rows=%d csr-order=%d csr-size=%d\n", rows, c.Order(), c.Size())

	// Reopen the written file to confirm the bulk load is durable.
	r, err := csrfile.Open(out)
	if err != nil {
		panic(err)
	}
	defer func() { _ = r.Close() }()
	fmt.Printf("on-disk edges=%d\n", r.Header().NEdges)

	// Output:
	// rows=3 csr-order=3 csr-size=3
	// on-disk edges=3
}
