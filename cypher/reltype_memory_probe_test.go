package cypher_test

// reltype_memory_probe_test.go — the RESIDENT-MEMORY measurement for rmp #2251,
// written so the identical file compiles and runs in a pristine 35990293 worktree
// as well as in the changed tree. It uses nothing but the public API, so the two
// arms measure the same thing by construction.
//
// # What is measured, and why this way
//
// The quantity is RETAINED heap: what an Engine still holds after a typed
// workload has run and the garbage collector has run twice. That is the honest
// question — the retired filter map and the type column are both structures a warm
// Engine keeps alive for the lifetime of a graph state, not transients.
//
// The reading is BRACKETED: the graph is built, GC'd and measured FIRST, then the
// Engine is created and driven, then GC'd and measured again. The graph itself is
// identical in both arms, so the difference of the two readings isolates what the
// Engine added — the CSR pair (also identical in both arms) plus the type
// structures, which is the only thing that differs.
//
// Two type-set counts are reported, deliberately. One type set is the case that
// flatters the old structure most; three is a realistic mixed workload and is
// where the old per-type-set cache had to hold three maps against the column's
// one. Reporting only the second would be choosing the answer.
//
// It is a LOGGING probe with one structural assertion, not a threshold test: an
// absolute byte count is a property of this machine and this Go version, so
// pinning one would be pinning noise. The comparison lives in the report.

import (
	"context"
	"fmt"
	"runtime"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// heapAfterGC returns HeapAlloc after two collections, so finalised-but-unswept
// objects from the previous phase are not counted into this one.
func heapAfterGC() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// buildTypedFixture builds a directed multigraph of n nodes where every node has
// out-degree 2 — one :K arc and one :M arc — so the graph has 2n relationships and
// ONE dominant type family, the shape on which the retired filter map was Θ(E).
func buildTypedFixture(tb testing.TB, n int) *lpg.Graph[string, float64] {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
	}
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("n%d", i)
		for j, typ := range []string{"K", "M"} {
			dst := fmt.Sprintf("n%d", (i+j+1)%n)
			if err := g.AddEdge(src, dst, 1); err != nil {
				tb.Fatalf("AddEdge: %v", err)
			}
			g.SetEdgeLabel(src, dst, typ)
		}
	}
	return g
}

// TestRelTypeResidentMemory reports the retained-heap delta an Engine adds over a
// typed workload, for one and for three distinct relationship-type sets.
func TestRelTypeResidentMemory(t *testing.T) {
	ctx := context.Background()
	for _, n := range []int{25_000, 100_000} {
		for _, tc := range []struct {
			name    string
			queries []string
		}{
			{"1 type set", []string{`MATCH ()-[:K]->() RETURN count(*) AS c`}},
			{"3 type sets", []string{
				`MATCH ()-[:K]->() RETURN count(*) AS c`,
				`MATCH ()-[:M]->() RETURN count(*) AS c`,
				`MATCH ()-[:K|M]->() RETURN count(*) AS c`,
			}},
		} {
			t.Run(fmt.Sprintf("n=%d/%s", n, tc.name), func(t *testing.T) {
				g := buildTypedFixture(t, n)
				before := heapAfterGC()

				eng := cypher.NewEngine(g)
				rows := 0
				for _, q := range tc.queries {
					res, err := eng.Run(ctx, q, nil)
					if err != nil {
						t.Fatalf("Run %q: %v", q, err)
					}
					for res.Next() {
						rows++
					}
					if err := res.Err(); err != nil {
						t.Fatalf("Err %q: %v", q, err)
					}
					if err := res.Close(); err != nil {
						t.Fatalf("Close %q: %v", q, err)
					}
				}
				after := heapAfterGC()

				// The Engine and the graph must still be reachable, or the reading
				// measures a collected structure and is meaningless.
				runtime.KeepAlive(eng)
				runtime.KeepAlive(g)

				if rows != len(tc.queries) {
					t.Fatalf("workload shipped %d rows over %d queries; it did not run", rows, len(tc.queries))
				}
				if after <= before {
					t.Fatalf("retained heap did not grow (%d -> %d): the Engine kept nothing, "+
						"so this reading cannot be compared with anything", before, after)
				}
				delta := after - before
				arcs := 2 * n
				t.Logf("RETAINED n=%d arcs=%d %-12s graph=%d B  warm=%d B  delta=%d B  (%.2f B/arc)",
					n, arcs, tc.name, before, after, delta, float64(delta)/float64(arcs))
			})
		}
	}
}
