package bulk

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// genEdges builds a deterministic, reproducible edge stream of n edges
// over a key space of nNodes nodes, with enough multiplicity that
// parallel edges between the same (src,dst) pair are common — this is
// what exercises the multigraph ordering invariant.
func genEdges(seed uint64, n, nNodes int) []Edge {
	rng := rand.New(rand.NewPCG(seed, seed*2654435761+1)) //nolint:gosec // deterministic test RNG
	es := make([]Edge, n)
	for i := range es {
		es[i] = Edge{
			Src:    fmt.Sprintf("n-%d", rng.IntN(nNodes)),
			Dst:    fmt.Sprintf("n-%d", rng.IntN(nNodes)),
			Weight: int64(i),
		}
	}
	return es
}

// csrEqual reports whether two CSRs are structurally identical: same
// offsets array, same flat edge array (so per-source adjacency order and
// parallel-edge multiplicity match exactly), same weights, same order
// and size. This is the strong form of "multigraph edge ordering
// preserved".
func csrEqual(a, b *csr.CSR[int64]) (bool, string) {
	if a.Order() != b.Order() {
		return false, fmt.Sprintf("order %d != %d", a.Order(), b.Order())
	}
	if a.Size() != b.Size() {
		return false, fmt.Sprintf("size %d != %d", a.Size(), b.Size())
	}
	av, bv := a.VerticesSlice(), b.VerticesSlice()
	if len(av) != len(bv) {
		return false, fmt.Sprintf("vertices len %d != %d", len(av), len(bv))
	}
	for i := range av {
		if av[i] != bv[i] {
			return false, fmt.Sprintf("vertices[%d] %d != %d", i, av[i], bv[i])
		}
	}
	ae, be := a.EdgesSlice(), b.EdgesSlice()
	if len(ae) != len(be) {
		return false, fmt.Sprintf("edges len %d != %d", len(ae), len(be))
	}
	for i := range ae {
		if ae[i] != be[i] {
			return false, fmt.Sprintf("edges[%d] %d != %d (per-source order/multiplicity diverged)", i, ae[i], be[i])
		}
	}
	aw, bw := a.WeightsSlice(), b.WeightsSlice()
	if len(aw) != len(bw) {
		return false, fmt.Sprintf("weights len %d != %d", len(aw), len(bw))
	}
	for i := range aw {
		if aw[i] != bw[i] {
			return false, fmt.Sprintf("weights[%d] %d != %d", i, aw[i], bw[i])
		}
	}
	return true, ""
}

// loadSeq and loadPar build the same edge stream through the sequential
// and parallel paths respectively and return the in-memory CSR plus the
// on-disk csrfile bytes.
func loadSeq(t *testing.T, edges []Edge) (*csr.CSR[int64], []byte) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "seq.csr")
	l := New(Options{OutputPath: out, Directed: true})
	if err := l.AddBatch(edges); err != nil {
		t.Fatalf("seq AddBatch: %v", err)
	}
	_, c, err := l.Finalise()
	if err != nil {
		t.Fatalf("seq Finalise: %v", err)
	}
	b, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read seq csrfile: %v", err)
	}
	return c, b
}

func loadPar(t *testing.T, edges []Edge) (*csr.CSR[int64], []byte) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "par.csr")
	l := New(Options{OutputPath: out, Directed: true, Parallel: true})
	if err := l.AddBatch(edges); err != nil {
		t.Fatalf("par AddBatch: %v", err)
	}
	_, c, err := l.Finalise()
	if err != nil {
		t.Fatalf("par Finalise: %v", err)
	}
	b, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read par csrfile: %v", err)
	}
	return c, b
}

// TestParallel_IdenticalToSequential is the determinism gate: across
// several seeds, the parallel build must yield a CSR that is
// structurally identical to the sequential build (offsets, flat edge
// array, weights, order, size) AND a byte-for-byte identical on-disk
// csrfile. It forces the parallel path on small inputs by lowering the
// size threshold, and runs with GOMAXPROCS > 1 so workers actually
// fan out.
func TestParallel_IdenticalToSequential(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS >= 2 to exercise the parallel fan-out")
	}
	// Force the parallel path regardless of input size for this test.
	orig := parallelMinEdges
	parallelMinEdges = 1
	t.Cleanup(func() { parallelMinEdges = orig })

	cases := []struct {
		name      string
		n, nNodes int
	}{
		{"dense-multi", 5000, 200},       // heavy parallel-edge multiplicity
		{"sparse", 5000, 4000},           // mostly distinct edges
		{"tiny-keyspace", 8000, 16},      // extreme multigraph collisions
		{"single-source-heavy", 3000, 1}, // every edge shares one source
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for seed := uint64(1); seed <= 5; seed++ {
				edges := genEdges(seed, tc.n, tc.nNodes)

				cSeq, bSeq := loadSeq(t, edges)
				cPar, bPar := loadPar(t, edges)

				if ok, why := csrEqual(cSeq, cPar); !ok {
					t.Fatalf("seed %d: CSR diverged: %s", seed, why)
				}
				if !bytes.Equal(bSeq, bPar) {
					t.Fatalf("seed %d: on-disk csrfile not byte-identical (len seq=%d par=%d)",
						seed, len(bSeq), len(bPar))
				}
			}
		})
	}
}

// TestParallel_StableAcrossRuns asserts the parallel build is
// deterministic with respect to itself: repeated parallel loads of the
// same input produce byte-identical csrfiles regardless of goroutine
// scheduling. This catches any residual scheduling dependence the
// sequential differential test could miss if the sequential build were
// itself nondeterministic.
func TestParallel_StableAcrossRuns(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS >= 2 to exercise the parallel fan-out")
	}
	orig := parallelMinEdges
	parallelMinEdges = 1
	t.Cleanup(func() { parallelMinEdges = orig })

	edges := genEdges(99, 10_000, 64)
	_, first := loadPar(t, edges)
	for run := 0; run < 8; run++ {
		_, b := loadPar(t, edges)
		if !bytes.Equal(first, b) {
			t.Fatalf("run %d: parallel csrfile not stable across runs", run)
		}
	}
}
