package cypher_test

// degree_oracle_test.go — every spelling of "how many out-edges" answered
// against an ABSOLUTE oracle, over randomised multigraphs (rmp #2273).
//
// Layer: short.
//
// # Why an absolute oracle and not a differential
//
// A degree question has at least five spellings in this engine, and they are
// served by different machinery: COUNT { } reaches the degree rewrite,
// size([ … ]) reaches it only since rmp #2264, an EXISTS { } short-circuits, a
// pattern predicate in WHERE takes yet another route, and a plain MATCH …
// count(*) enumerates. Comparing them to EACH OTHER is what a differential does,
// and this project has twice watched a differential go green over a real defect
// because both arms shared the broken code.
//
// So the oracle here is not another query. The fixture GENERATOR records the
// truth as it builds: every edge it adds is tallied by source and by type, and
// every node it later removes retracts the tallies of the edges incident to it.
// The expected answer is therefore known by construction, independent of every
// read path in the module — including the Go API, which is itself under test.
//
// # The shapes are the ones that have actually broken
//
// Parallel edges (rmp #2241, #2258), self-loops (#2258 answered 12 where 1 was
// correct), a mix of typed and untyped slots on one pair (#2258: two build
// orders left byte-identical state needing different answers), and tombstoned
// far nodes (#2265: a cap applied to the result of an uncapped walk). A uniform
// simple graph exercises none of them.

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// degreeFixture is a randomised multigraph together with the tallies its
// generator recorded while building it.
type degreeFixture struct {
	g *lpg.Graph[string, float64]
	// typedOut[src][type] and anyOut[src] count LIVE out-edge slots. A slot is
	// live when neither endpoint has been removed.
	typedOut map[int]map[string]int64
	anyOut   map[int]int64
	live     []int
}

// buildDegreeFixture generates nodes, edges and removals from seed, tallying as
// it goes. Nothing here reads the graph back: the tallies are the oracle.
func buildDegreeFixture(t *testing.T, seed int64, nodes, edges int) *degreeFixture {
	t.Helper()
	rng := rand.New(rand.NewSource(seed)) //nolint:gosec // deterministic fixture, not security
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	key := func(i int) string { return fmt.Sprintf("n%d", i) }
	for i := 0; i < nodes; i++ {
		if err := g.AddNode(key(i)); err != nil {
			t.Fatalf("AddNode(%d): %v", i, err)
		}
		if err := g.SetNodeProperty(key(i), "k", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty(%d): %v", i, err)
		}
		if err := g.SetNodeLabel(key(i), "P"); err != nil {
			t.Fatalf("SetNodeLabel(%d): %v", i, err)
		}
	}

	// Record every edge so removals can retract exactly the right tallies.
	type edge struct {
		src, dst int
		typ      string
	}
	added := make([]edge, 0, edges)
	types := []string{"K", "M", ""}
	for i := 0; i < edges; i++ {
		src := rng.Intn(nodes)
		// A quarter of the edges are self-loops: the shape that produced a
		// wrong typed count of 12 against a correct 1 in rmp #2258.
		dst := src
		if rng.Intn(4) != 0 {
			dst = rng.Intn(nodes)
		}
		typ := types[rng.Intn(len(types))]
		var err error
		if typ == "" {
			err = g.AddEdge(key(src), key(dst), 1)
		} else {
			err = g.AddEdgeLabeled(key(src), key(dst), 1, typ)
		}
		if err != nil {
			t.Fatalf("add edge %d->%d (%q): %v", src, dst, typ, err)
		}
		added = append(added, edge{src, dst, typ})
	}

	// Remove a tenth of the nodes, so tombstoned endpoints are in play.
	removed := make(map[int]bool, nodes/10)
	for i := 0; i < nodes/10; i++ {
		n := rng.Intn(nodes)
		if removed[n] {
			continue
		}
		removed[n] = true
		g.RemoveNode(key(n))
	}

	f := &degreeFixture{
		g:        g,
		typedOut: make(map[int]map[string]int64, nodes),
		anyOut:   make(map[int]int64, nodes),
		live:     make([]int, 0, nodes),
	}
	for i := 0; i < nodes; i++ {
		if !removed[i] {
			f.live = append(f.live, i)
		}
		f.typedOut[i] = make(map[string]int64, 2)
	}

	// Non-degeneracy tallies, asserted below: a fixture that failed to produce
	// the awkward shapes would let every spelling agree vacuously. This project
	// has previously drawn a conclusion from a fixture whose far endpoints all
	// had out-degree zero.
	var selfLoops, dropped, parallel int
	seenPair := make(map[[2]int]int, len(added))
	for _, e := range added {
		if removed[e.src] || removed[e.dst] {
			dropped++
			continue // the slot went with its endpoint
		}
		if e.src == e.dst {
			selfLoops++
		}
		seenPair[[2]int{e.src, e.dst}]++
		if seenPair[[2]int{e.src, e.dst}] == 2 {
			parallel++
		}
		f.anyOut[e.src]++
		if e.typ != "" {
			f.typedOut[e.src][e.typ]++
		}
	}
	var mixedPairs int
	for i := range f.typedOut {
		if f.typedOut[i]["K"] > 0 && f.anyOut[i] > f.typedOut[i]["K"]+f.typedOut[i]["M"] {
			mixedPairs++
		}
	}
	t.Logf("fixture seed=%d: %d live nodes, %d live slots, %d self-loops, %d parallel pairs, "+
		"%d slots dropped with a removed endpoint, %d anchors mixing typed and untyped slots",
		seed, len(f.live), totalSlots(f), selfLoops, parallel, dropped, mixedPairs)
	if selfLoops == 0 || parallel == 0 || dropped == 0 || mixedPairs == 0 {
		t.Fatalf("degenerate fixture at seed %d (self-loops %d, parallel %d, dropped %d, mixed %d): "+
			"it does not contain the shapes that have actually broken this engine, so agreement "+
			"between the spellings would prove nothing", seed, selfLoops, parallel, dropped, mixedPairs)
	}
	return f
}

// totalSlots sums the live out-degree tallies, used only for the fixture log.
func totalSlots(f *degreeFixture) int64 {
	var n int64
	for _, v := range f.anyOut {
		n += v
	}
	return n
}

// scalarInt runs q with the given key parameter and returns the single integer
// column n. Every query below returns exactly one row.
func scalarInt(t *testing.T, eng *cypher.Engine, q string, k int64) string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, map[string]expr.Value{"k": expr.IntegerValue(k)})
	if err != nil {
		t.Fatalf("Run(%s, k=%d): %v", q, k, err)
	}
	got := "<no row>"
	for res.Next() {
		got = fmt.Sprint(res.Record()["n"])
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%s, k=%d): %v", q, k, err)
	}
	_ = res.Close()
	return got
}

// TestDegree_AllSpellings_MatchAbsoluteOracle is the gate. Every spelling of a
// degree question must equal the tally the generator recorded, for every live
// anchor, across several seeds.
func TestDegree_AllSpellings_MatchAbsoluteOracle(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 1337, 20260731, 99991, 31337, 4242} {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			f := buildDegreeFixture(t, seed, 40, 200)
			eng := cypher.NewEngine(f.g)

			// Each entry pairs a Cypher spelling with the tally it must equal.
			// The typed spellings answer typedOut[a]["K"]; the untyped ones
			// answer anyOut[a], which includes the typed slots.
			spellings := []struct {
				name  string
				query string
				want  func(a int) int64
			}{
				{
					"typed COUNT subquery",
					`MATCH (a:P) WHERE a.k = $k RETURN COUNT { (a)-[:K]->() } AS n`,
					func(a int) int64 { return f.typedOut[a]["K"] },
				},
				{
					"typed size(pattern comprehension)",
					`MATCH (a:P) WHERE a.k = $k RETURN size([ (a)-[:K]->(x) | 1 ]) AS n`,
					func(a int) int64 { return f.typedOut[a]["K"] },
				},
				{
					"typed enumeration",
					`MATCH (a:P)-[:K]->(x) WHERE a.k = $k RETURN count(*) AS n`,
					func(a int) int64 { return f.typedOut[a]["K"] },
				},
				{
					"typed comprehension length after WITH",
					`MATCH (a:P) WHERE a.k = $k WITH a, [ (a)-[:K]->(x) | 1 ] AS l RETURN size(l) AS n`,
					func(a int) int64 { return f.typedOut[a]["K"] },
				},
				{
					"untyped COUNT subquery",
					`MATCH (a:P) WHERE a.k = $k RETURN COUNT { (a)-->() } AS n`,
					func(a int) int64 { return f.anyOut[a] },
				},
				{
					"untyped size(pattern comprehension)",
					`MATCH (a:P) WHERE a.k = $k RETURN size([ (a)-->(x) | 1 ]) AS n`,
					func(a int) int64 { return f.anyOut[a] },
				},
				{
					"untyped enumeration",
					`MATCH (a:P)-->(x) WHERE a.k = $k RETURN count(*) AS n`,
					func(a int) int64 { return f.anyOut[a] },
				},
			}

			for _, sp := range spellings {
				bad := 0
				for _, a := range f.live {
					want := fmt.Sprint(sp.want(a))
					got := scalarInt(t, eng, sp.query, int64(a))
					if got != want {
						bad++
						if bad <= 3 { // cap the noise; the count below is the verdict
							t.Errorf("%s: anchor n%d answered %s, oracle says %s",
								sp.name, a, got, want)
						}
					}
				}
				if bad != 0 {
					t.Errorf("%s: %d of %d live anchors disagree with the oracle",
						sp.name, bad, len(f.live))
				}
			}

			// EXISTS must agree with "the tally is non-zero" — a different
			// operator with a short-circuit, so it can be wrong on its own.
			for _, a := range f.live {
				want := "false"
				if f.typedOut[a]["K"] > 0 {
					want = "true"
				}
				q := `MATCH (a:P) WHERE a.k = $k RETURN EXISTS { (a)-[:K]->() } AS n`
				if got := scalarInt(t, eng, q, int64(a)); got != want {
					t.Errorf("EXISTS: anchor n%d answered %s, oracle says %s (tally %d)",
						a, got, want, f.typedOut[a]["K"])
					break
				}
			}
		})
	}
}
