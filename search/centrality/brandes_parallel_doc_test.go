package centrality

import (
	"bytes"
	"math"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildParallelTestGraph returns a small undirected CSR graph with
// enough paths to produce non-trivial betweenness scores and force
// observable floating-point accumulation differences between the
// serial and parallel implementations:
//
//	0-1-2-3-4-5 (path spine) plus cross-edges 0-3, 1-4, 2-5.
func buildParallelTestGraph() *csr.CSR[struct{}] {
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	spine := [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 4}, {4, 5}}
	cross := [][2]int{{0, 3}, {1, 4}, {2, 5}}
	for _, e := range append(spine, cross...) {
		if err := a.AddEdge(e[0], e[1], struct{}{}); err != nil {
			panic(err)
		}
	}
	return csr.BuildFromAdjList(a)
}

// TestBetweennessParallel_AgreesWithSerial_WithinTolerance verifies
// that the parallel result agrees with the serial Betweenness within
// 1e-10 for every node, matching the corrected godoc which states
// results "agree within this numerical tolerance". A fixed numWorkers
// value ensures the parallel output is itself deterministic.
func TestBetweennessParallel_AgreesWithSerial_WithinTolerance(t *testing.T) {
	t.Parallel()
	c := buildParallelTestGraph()

	serial := Betweenness(c)
	parallel := BetweennessParallel(c, 2) // fixed workers for determinism

	const tol = 1e-10
	for i := range serial {
		diff := math.Abs(parallel[i] - serial[i])
		if diff > tol {
			t.Errorf("node %d: serial=%v parallel=%v diff=%v (want <= %e)",
				i, serial[i], parallel[i], diff, tol)
		}
	}
}

// TestBetweennessParallel_GodocNoBitIdentityClaim enforces that the
// godoc of BetweennessParallel does not claim bit-identity with the
// serial result. Bit-identity is false for numWorkers > 1 due to
// non-associative floating-point addition.
//
// This test fails when the source file still contains the old claim
// and passes once "bit-identical" is removed.
func TestBetweennessParallel_GodocNoBitIdentityClaim(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("brandes_parallel.go")
	if err != nil {
		t.Fatalf("cannot read brandes_parallel.go: %v", err)
	}
	if bytes.Contains(src, []byte("bit-identical")) {
		t.Fatal("brandes_parallel.go godoc still claims 'bit-identical' — " +
			"remove or replace with tolerance-based language")
	}
}
