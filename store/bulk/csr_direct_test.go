package bulk

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// buildAdjCSR replays edges through the mutable adjacency list and builds
// the CSR with csr.BuildFromAdjList — the byte-identity ground truth the
// CSR-direct counting sort must reproduce.
func buildAdjCSR(t *testing.T, edges []Edge, multigraph bool) *csr.CSR[int64] {
	t.Helper()
	adj := adjlist.New[string, int64](adjlist.Config{Directed: true, Multigraph: multigraph})
	for _, e := range edges {
		if err := adj.AddEdge(e.Src, e.Dst, e.Weight); err != nil {
			t.Fatalf("AddEdge(%s->%s): %v", e.Src, e.Dst, err)
		}
	}
	return csr.BuildFromAdjList(adj)
}

// loadCSRDirect drives a Parallel directed load (which now takes the
// CSR-direct counting-sort path for the uncapped, production configuration)
// and returns the in-memory CSR plus the on-disk csrfile bytes.
func loadCSRDirect(t *testing.T, edges []Edge, multigraph bool) (*csr.CSR[int64], []byte) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "direct.csr")
	l := New(Options{OutputPath: out, Directed: true, Multigraph: multigraph, Parallel: true})
	if err := l.AddBatch(edges); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	if !l.csrDirectEligible() {
		t.Fatalf("expected CSR-direct eligibility for directed uncapped load (multigraph=%v)", multigraph)
	}
	_, c, err := l.Finalise()
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	b, err := os.ReadFile(out) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read csrfile: %v", err)
	}
	return c, b
}

// TestCSRDirect_IdenticalToBuildFromAdjList is the byte-identity gate for
// the CSR-direct counting-sort build. For both directed simple graphs
// (first-occurrence dedup) and directed multigraphs (keep every parallel
// edge), the counting-sort CSR — and the csrfile it writes — must match
// csr.BuildFromAdjList exactly: same offsets, same flat edge array, same
// weights, same order and size. The dense-multi / tiny-keyspace cases pack
// many parallel edges between the same (src, dst) pair, which is precisely
// where the simple-vs-multigraph dedup behaviour and within-row ordering
// diverge if reproduced incorrectly.
func TestCSRDirect_IdenticalToBuildFromAdjList(t *testing.T) {
	cases := []struct {
		name      string
		n, nNodes int
	}{
		{"dense-multi", 5000, 200},
		{"sparse", 5000, 4000},
		{"tiny-keyspace", 8000, 16},
		{"single-source-heavy", 3000, 1},
		{"self-loops", 4000, 32}, // many (n, n) edges land on the self-loop forward append
	}
	for _, multigraph := range []bool{false, true} {
		multigraph := multigraph
		mode := "simple"
		if multigraph {
			mode = "multigraph"
		}
		t.Run(mode, func(t *testing.T) {
			for _, tc := range cases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					for seed := uint64(1); seed <= 5; seed++ {
						edges := genEdges(seed, tc.n, tc.nNodes)
						want := buildAdjCSR(t, edges, multigraph)
						got, _ := loadCSRDirect(t, edges, multigraph)
						if ok, why := csrEqual(want, got); !ok {
							t.Fatalf("seed %d: CSR-direct diverged from BuildFromAdjList: %s", seed, why)
						}
					}
				})
			}
		})
	}
}

// TestCSRDirect_CsrfileByteIdentical asserts the stronger property that the
// on-disk csrfile bytes produced through the CSR-direct path are identical
// to those produced by the sequential adjacency path, for both multigraph
// modes. The csrfile is CRC32C-protected, so any single-byte divergence in
// offsets, edges, or weights would surface here.
func TestCSRDirect_CsrfileByteIdentical(t *testing.T) {
	for _, multigraph := range []bool{false, true} {
		multigraph := multigraph
		mode := "simple"
		if multigraph {
			mode = "multigraph"
		}
		t.Run(mode, func(t *testing.T) {
			edges := genEdges(7, 6000, 64)

			// Sequential adjacency path: force it by disabling Parallel.
			seqOut := filepath.Join(t.TempDir(), "seq.csr")
			ls := New(Options{OutputPath: seqOut, Directed: true, Multigraph: multigraph})
			if err := ls.AddBatch(edges); err != nil {
				t.Fatalf("seq AddBatch: %v", err)
			}
			if _, _, err := ls.Finalise(); err != nil {
				t.Fatalf("seq Finalise: %v", err)
			}
			seqBytes, err := os.ReadFile(seqOut) //nolint:gosec // test-controlled path
			if err != nil {
				t.Fatalf("read seq csrfile: %v", err)
			}

			_, directBytes := loadCSRDirect(t, edges, multigraph)
			if !bytes.Equal(seqBytes, directBytes) {
				t.Fatalf("csrfile not byte-identical (len seq=%d direct=%d)", len(seqBytes), len(directBytes))
			}
		})
	}
}

// TestCSRDirect_EmptyInput verifies the CSR-direct build reproduces
// BuildFromAdjList's empty-graph shape (vertices == {0}, order 0, size 0,
// nil edges/weights) when no edges are loaded, so the published csrfile is
// byte-identical for a degenerate load.
func TestCSRDirect_EmptyInput(t *testing.T) {
	want := buildAdjCSR(t, nil, false)

	out := filepath.Join(t.TempDir(), "empty.csr")
	l := New(Options{OutputPath: out, Directed: true, Parallel: true})
	_, got, err := l.Finalise()
	if err != nil {
		t.Fatalf("Finalise: %v", err)
	}
	if ok, why := csrEqual(want, got); !ok {
		t.Fatalf("empty CSR-direct diverged: %s", why)
	}
	if got.Order() != 0 || got.Size() != 0 {
		t.Fatalf("empty CSR: order=%d size=%d, want 0/0", got.Order(), got.Size())
	}
	if len(got.VerticesSlice()) != 1 || got.VerticesSlice()[0] != 0 {
		t.Fatalf("empty CSR vertices = %v, want [0]", got.VerticesSlice())
	}
	if got.EdgesSlice() != nil || got.WeightsSlice() != nil {
		t.Fatalf("empty CSR edges/weights not nil")
	}
}

// TestCSRDirect_CappedFallsBack confirms that a capacity-capped adjacency
// (MaxShardCapacity > 0) is NOT eligible for the CSR-direct path and
// therefore retains the adjacency build that enforces ErrShardFull before
// any csrfile is published. This guards the all-or-nothing publication
// invariant the parallel atomicity test relies on.
func TestCSRDirect_CappedFallsBack(t *testing.T) {
	if runtime.GOMAXPROCS(0) < 2 {
		t.Skip("needs GOMAXPROCS >= 2 to exercise the parallel fan-out")
	}
	orig := parallelMinEdges
	parallelMinEdges = 1
	t.Cleanup(func() { parallelMinEdges = orig })

	dir := t.TempDir()
	out := filepath.Join(dir, "graph.csr")
	l := &Loader{
		opts: Options{OutputPath: out, Directed: true, Parallel: true},
		adj: adjlist.New[string, int64](adjlist.Config{
			Directed:         true,
			MaxShardCapacity: 1,
		}),
	}
	if l.csrDirectEligible() {
		t.Fatal("capped adjacency must not be CSR-direct eligible")
	}
	edges := genEdges(7, 4000, 2000)
	if err := l.AddBatch(edges); err != nil {
		t.Fatalf("AddBatch: %v", err)
	}
	if _, _, err := l.Finalise(); err == nil {
		t.Fatal("expected Finalise to fail on a capped overflowing build")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("partial csrfile present after failed capped build: stat err = %v", statErr)
	}
}
