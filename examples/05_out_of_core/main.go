// Example 05_out_of_core — writes a Tier 2 csrfile, opens it via mmap,
// applies a SEQUENTIAL access hint, and runs PageRank directly over the
// mapped region, then verifies the result against the graph's symmetry.
//
// Tier 2 is GoGraph's external-memory storage tier: the CSR adjacency
// lives on disk in the csrfile binary format and is read back by mmap'ing
// the file and reinterpreting its aligned sections as typed slices in
// place — no parse, no copy into the heap. This contrasts with Tier 1,
// the fully in-memory CSR snapshot built by csr.BuildFromAdjList. The
// semi-external PageRank keeps only the rank vector in RAM (size =
// number of vertices) while streaming the adjacency sequentially from the
// mapped file on every iteration, so the working set is bounded by the
// vertex count rather than the edge count.
//
// The graph is a uniform 1000-node directed ring (i -> (i+1) mod 1000).
// Because every node has exactly one in-edge and one out-edge, the
// PageRank stationary distribution is perfectly uniform: every live node
// holds rank 1/1000 = 0.001, so the example verifies the computation by
// checking that the minimum and maximum live ranks are equal and match a
// sampled node's rank.
//
// Sample output: run `go run ./examples/05_out_of_core` and capture the
// stdout — the output is deterministic for the inputs hard-coded above and
// serves as the regression baseline a future change should preserve. The
// csrfile is written to an os.MkdirTemp directory whose absolute path
// varies per run, so the report never prints that path.
package main

import (
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"

	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/csr"
	"gograph/search/extern"
	"gograph/store/csrfile"
)

func main() {
	if err := run(os.Stdout); err != nil {
		log.Fatal(err)
	}
}

// run builds a uniform directed ring, freezes it into a CSR snapshot,
// persists it as a Tier 2 csrfile, re-opens the file via mmap, runs
// semi-external PageRank over the mapped region, and verifies the result.
// All output goes to w so a test can capture and assert it; run returns
// wrapped errors rather than terminating the process.
//
// The csrfile is written under an os.MkdirTemp directory whose absolute
// path differs on every run, so run never prints that path; every line it
// emits is byte-stable.
func run(w io.Writer) error {
	const ringSize = 1000

	dir, err := os.MkdirTemp("", "gograph-ex05-")
	if err != nil {
		return fmt.Errorf("MkdirTemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "graph.csr")

	// Build the in-memory adjacency: a uniform directed ring where every
	// node points to its successor modulo ringSize.
	a := adjlist.New[uint32, struct{}](adjlist.Config{Directed: true})
	for i := uint32(0); i < ringSize; i++ {
		if err := a.AddEdge(i, (i+1)%ringSize, struct{}{}); err != nil {
			return fmt.Errorf("AddEdge %d->%d: %w", i, (i+1)%ringSize, err)
		}
	}

	// Tier 1 — freeze the builder into an immutable in-memory CSR snapshot.
	c := csr.BuildFromAdjList(a)

	// Tier 2 — write the CSR to disk in the csrfile binary format. The
	// write is atomic (write .tmp, fsync, rename).
	if _, err := csrfile.WriteToFile(path, c); err != nil {
		return fmt.Errorf("csrfile.WriteToFile: %w", err)
	}
	fmt.Fprintf(w, "Tier 2: wrote %s (%d vertices, %d edges).\n",
		filepath.Base(path), c.Order(), c.Size())

	// Re-open the file via mmap. The returned slices alias the mapped
	// region directly; no body is parsed and nothing is copied onto the
	// heap. Hint SEQUENTIAL access so the OS reads ahead while PageRank
	// streams the adjacency.
	r, err := csrfile.Open(path)
	if err != nil {
		return fmt.Errorf("csrfile.Open: %w", err)
	}
	defer func() { _ = r.Close() }()
	if err := r.SetHint(csrfile.AccessSequential); err != nil {
		return fmt.Errorf("SetHint: %w", err)
	}

	// Semi-external PageRank: only the rank vector lives in RAM; the
	// adjacency streams from the mapped file each iteration.
	ranks, iters, err := extern.PageRank(r, extern.DefaultPageRankOptions())
	if err != nil {
		return fmt.Errorf("extern.PageRank: %w", err)
	}

	// Verify the computation. The rank slice is indexed by NodeID and its
	// length is the sharded, MaxNodeID-rounded vertex count, so it carries
	// ghost slots (zero rank) beyond the live nodes. Count only the live
	// ranks (non-zero) and track their min and max. On a uniform ring the
	// stationary distribution is uniform, so min and max must be equal.
	minRank, maxRank := math.Inf(1), math.Inf(-1)
	live := 0
	for _, x := range ranks {
		if x == 0 {
			continue
		}
		live++
		minRank = math.Min(minRank, x)
		maxRank = math.Max(maxRank, x)
	}
	if live == 0 {
		return fmt.Errorf("PageRank produced no live ranks")
	}

	fmt.Fprintf(w, "PageRank: converged in %d iteration(s), %d live ranks.\n", iters, live)

	// Sample one live node's rank (NodeID 0) and confirm it equals the
	// theoretical uniform value 1/live.
	sample, err := liveRankAt(ranks, 0)
	if err != nil {
		return err
	}
	expected := 1.0 / float64(live)
	uniform := math.Abs(maxRank-minRank) < 1e-12
	fmt.Fprintf(w, "Verify: uniform=%t, min=max=%.6f, node 0 rank=%.6f (expected %.6f).\n",
		uniform, minRank, sample, expected)
	return nil
}

// liveRankAt returns the rank stored at NodeID id, or an error if that
// slot is out of range or carries no live rank.
func liveRankAt(ranks []float64, id graph.NodeID) (float64, error) {
	if int(id) >= len(ranks) {
		return 0, fmt.Errorf("node %d out of range (len %d)", id, len(ranks))
	}
	r := ranks[id]
	if r == 0 {
		return 0, fmt.Errorf("node %d is not a live rank", id)
	}
	return r, nil
}
