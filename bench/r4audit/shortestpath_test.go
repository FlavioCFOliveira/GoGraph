//go:build r4audit

package r4audit

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// spSeed builds a directed graph of n :P nodes with the given average
// out-degree, wired deterministically so a run is reproducible. This is the
// shape the round-3 head-to-head measured shortestPath on: average degree 10.
func spSeed(t *testing.T, n, degree int) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("n%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(k, "id", lpg.Int64Value(int64(i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("n%d", i)
		for j := 1; j <= degree; j++ {
			dst := fmt.Sprintf("n%d", (i*7+j*13)%n)
			if err := g.AddEdge(src, dst, 1); err != nil {
				t.Fatal(err)
			}
			g.SetEdgeLabel(src, dst, "K")
		}
	}
	eng := cypher.NewEngine(g)
	// Index :P(id) so binding the fixed destination is a seek, not a scan. Without
	// it the destination lookup re-scans the whole label per source row and costs
	// two orders of magnitude more than the search being measured.
	if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_id FOR (x:P) ON (x.id)`, nil); err != nil {
		t.Fatal(err)
	}
	return eng
}

// TestShortestPathBounded is the acceptance measurement for rmp #2220.
//
// Round 3 measured the bounded 6-hop shape at 23.797 ms against Neo4j's
// 1.089 ms and Memgraph's 395 µs — 60×, the worst non-triangle ratio in the
// matrix — on a graph of average out-degree 10.
func TestShortestPathBounded(t *testing.T) {
	const degree = 10
	sizes := []int{5000, 20000}
	// MANY pairs per query, deliberately. A single-pair query is dominated by the
	// fixed per-query setup — the CSR snapshot and the two endpoint lookups — which
	// swamped the search entirely in the first version of this benchmark: the
	// two-sided search measured 12.382 ms against the forward-only walk's
	// 13.268 ms at N=20000, a difference indistinguishable from noise, because
	// almost none of that time was the search. Driving 200 pairs through one
	// operator amortises the setup and lets the per-search cost show.
	cases := []struct{ name, q string }{
		{"shortestPath bounded 6", `MATCH (b:P {id: 4321}) WITH b MATCH (a:P) WHERE a.id < 200 MATCH p = shortestPath((a)-[:K*..6]->(b)) RETURN count(p)`},
		{"shortestPath unbounded", `MATCH (b:P {id: 4321}) WITH b MATCH (a:P) WHERE a.id < 200 MATCH p = shortestPath((a)-[:K*]->(b)) RETURN count(p)`},
		{"shortestPath untyped", `MATCH (b:P {id: 4321}) WITH b MATCH (a:P) WHERE a.id < 200 MATCH p = shortestPath((a)-[*..6]->(b)) RETURN count(p)`},
		{"allShortestPaths (control, untouched)", `MATCH (b:P {id: 4321}) WITH b MATCH (a:P) WHERE a.id < 200 MATCH p = allShortestPaths((a)-[:K*..6]->(b)) RETURN count(p)`},
		{"single pair (setup-dominated, kept as a caution)", `MATCH (a:P {id: 0}), (b:P {id: 4321}) MATCH p = shortestPath((a)-[:K*..6]->(b)) RETURN length(p)`},
	}

	fmt.Printf("%-40s", "case")
	for _, n := range sizes {
		fmt.Printf("%14s", fmt.Sprintf("N=%d", n))
	}
	fmt.Println()

	for _, c := range cases {
		fmt.Printf("%-40s", c.name)
		for _, n := range sizes {
			eng := spSeed(t, n, degree)
			var rows int
			if res, err := eng.RunAny(context.Background(), c.q, nil); err == nil {
				for res.Next() {
					rows++
				}
				_ = res.Close()
			} else {
				fmt.Printf("  ERROR %v", err)
				break
			}
			best := time.Hour
			for k := 0; k < 5; k++ {
				st := time.Now()
				res, err := eng.RunAny(context.Background(), c.q, nil)
				if err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				for res.Next() {
				}
				_ = res.Close()
				if d := time.Since(st); d < best {
					best = d
				}
			}
			fmt.Printf("%14s", best.Round(time.Microsecond))
		}
		fmt.Println()
	}
}
