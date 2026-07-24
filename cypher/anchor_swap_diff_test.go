package cypher

// anchor_swap_diff_test.go — differential tests for the single-edge anchor-swap
// peephole (#2090).
//
// Each representative query runs on an anchor-swap-ENABLED engine and a
// -DISABLED engine over the same graph, and the results are compared as a sorted
// bag (unordered) or an exact sequence (with a downstream total ORDER BY). A
// separate assertion on anchorSwapBuildCount confirms the swap fired for the
// cases that must re-root and did NOT fire for the veto cases (reverse-
// introducing direction, a dirtied D cell, a suppressed order), so the test
// cannot silently pass by never triggering.
//
// The graphs are built through a seed engine's write path so the count-store's
// exact D(label,relType,dir) cells are populated exactly as production maintains
// them; both the ON and OFF engines are then constructed fresh on the shared
// graph, each recomputing an identical, clean count-store at construction.

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// runWrite executes a write statement to completion via RunInTx.
func runWrite(t *testing.T, e *Engine, q string) {
	t.Helper()
	res, err := e.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", q, err)
	}
	for res.Next() { //nolint:revive // intentional full drain
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
}

// buildAnchorGraph creates an in-memory graph and populates it by running the
// given write statements through a seed engine (so the count-store is maintained
// exactly as production does), then returns the graph for the ON/OFF engines to
// share.
func buildAnchorGraph(t *testing.T, stmts ...string) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	seed := NewEngine(g)
	for _, s := range stmts {
		runWrite(t, seed, s)
	}
	return g
}

// assertAnchorIdentical runs q on an anchor-swap-ENABLED and a -DISABLED engine
// over g and asserts identical results (sorted bag when ordered==false, exact
// sequence when ordered==true). wantTrigger asserts whether the enabled run
// actually re-rooted a single-edge pattern.
func assertAnchorIdentical(t *testing.T, g *lpg.Graph[string, float64], q string, wantTrigger, ordered bool) {
	t.Helper()
	on := NewEngine(g)
	off := NewEngineWithOptions(g, EngineOptions{DisableAnchorSwap: true})

	before := anchorSwapBuildCount.Load()
	gotOn := drainRows(t, on, q)
	triggered := anchorSwapBuildCount.Load() > before
	gotOff := drainRows(t, off, q)

	if !ordered {
		sort.Strings(gotOn)
		sort.Strings(gotOff)
	}
	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch for %q: swap=%d default=%d", q, len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			kind := "bag"
			if ordered {
				kind = "sequence"
			}
			t.Fatalf("%s row %d differs for %q:\n  swap    = %s\n  default = %s", kind, i, q, gotOn[i], gotOff[i])
		}
	}
	if wantTrigger && !triggered {
		t.Fatalf("expected the anchor swap to fire for %q, but it did not", q)
	}
	if !wantTrigger && triggered {
		t.Fatalf("did NOT expect the anchor swap to fire for %q, but it did", q)
	}
}

// seedHubGraph builds the adversarial hub graph for the OUT-ward swap:
//
//   - one :Hub node;
//   - 1600 :Other nodes, each with an R edge Other→Hub (so Hub has a huge R
//     in-degree, D(Hub,R,IN) ≈ 1601);
//   - 50 :Leaf nodes, exactly one of which (i=1) has an R edge Leaf→Hub (so
//     D(Leaf,R,OUT) = 1, N(Leaf) = 50).
//
// The written pattern `(a:Hub)<-[:R]-(b:Leaf)` anchors Hub and walks its ~1601
// incoming R-edges (a DirIn expand). Re-rooting onto Leaf (a DirOut expand) walks
// a single out-edge. A naive "scan the smaller label" choice picks Hub (N=1) and
// examines ~1601 edges; the count-store cost model picks Leaf and examines 1 —
// ~1600× fewer examined edges. The single true match is (Leaf i=1) → Hub.
func seedHubGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	return buildAnchorGraph(t,
		"CREATE (:Hub {tag:0})",
		"MATCH (h:Hub) UNWIND range(1,1600) AS i CREATE (:Other {i:i})-[:R]->(h)",
		"UNWIND range(1,50) AS i CREATE (:Leaf {i:i})",
		"MATCH (h:Hub),(l:Leaf {i:1}) CREATE (l)-[:R]->(h)",
	)
}

func TestAnchorSwap_Differential_Hub_OutwardSwaps(t *testing.T) {
	// Written DirIn `(a:Hub)<-[:R]-(b:Leaf)`: anchor-Hub walks ~1601 in-edges;
	// re-rooting onto Leaf (DirOut) walks 1. The swap FIRES and the (a,b) bag is
	// identical to the written-order plan.
	g := seedHubGraph(t)
	assertAnchorIdentical(t, g,
		"MATCH (a:Hub)<-[:R]-(b:Leaf) RETURN a.tag AS at, b.i AS bi", true, false)
}

func TestAnchorSwap_Differential_Hub_TotalOrderBy_Safe(t *testing.T) {
	// A total ORDER BY is a RESET enabler: the swap is order-safe and FIRES, and
	// the sort masks the emission-order change, so the exact row SEQUENCE is
	// identical ON vs OFF. Totality requires EVERY flowing column to be pinned by
	// the sort key; the single-edge pattern binds a, b AND the relationship r, so
	// the order must key on id(a), id(b), id(r) (naming the rel) — id(a), id(b)
	// alone leaves r unpinned and would (correctly) suppress the swap.
	g := seedHubGraph(t)
	assertAnchorIdentical(t, g,
		"MATCH (a:Hub)<-[r:R]-(b:Leaf) RETURN a, b, r ORDER BY id(a), id(b), id(r)", true, true)
}

func TestAnchorSwap_Differential_ReverseIntroducing_Vetoed(t *testing.T) {
	// Written DirOut `(a:Hub)-[:R]->(b:Leaf)` — here the arrow points OUT of the
	// pattern's first node. Re-rooting onto Leaf would require a DirIn expand,
	// whose per-in-edge cost is Θ(source out-degree) and invisible to the counts
	// (docs/reordering-design.md §5.1). Even though Hub's OUT-degree here is small
	// and Leaf is a plausible cheaper anchor, the swap MUST be vetoed because it is
	// reverse-introducing. No trigger; result identity regardless.
	g := buildAnchorGraph(t,
		"CREATE (:Hub {tag:0})",
		"MATCH (h:Hub) UNWIND range(1,1600) AS i CREATE (h)-[:R]->(:Other {i:i})",
		"UNWIND range(1,50) AS i CREATE (:Leaf {i:i})",
		"MATCH (h:Hub),(l:Leaf {i:1}) CREATE (h)-[:R]->(l)",
	)
	assertAnchorIdentical(t, g,
		"MATCH (a:Hub)-[:R]->(b:Leaf) RETURN a.tag AS at, b.i AS bi", false, false)
}

func TestAnchorSwap_Differential_ToProperty_Swaps(t *testing.T) {
	// An OUT-ward swap under a to-endpoint property filter: the b.i=1 Selection
	// sits ABOVE the matched inner Selection[(b:Leaf)] and references b, which the
	// mirror still binds (as the re-rooted scan). The swap FIRES and the filtered
	// result is identical ON vs OFF — verifying the property filter is correctly
	// left in place above the re-rooted subtree.
	g := seedHubGraph(t)
	assertAnchorIdentical(t, g,
		"MATCH (a:Hub)<-[:R]-(b:Leaf {i:1}) RETURN a.tag AS at, b.i AS bi", true, false)
}

func TestAnchorSwap_Differential_FeedingCollect_Suppressed(t *testing.T) {
	// collect() builds a list in arrival order — a value trap under bag comparison
	// — so SuppressReorder removes the site from the candidate set at parse time
	// and the swap MUST NOT fire. The single collected-list row is identical.
	g := seedHubGraph(t)
	assertAnchorIdentical(t, g,
		"MATCH (a:Hub)<-[:R]-(b:Leaf) RETURN collect(b.i) AS bs", false, false)
}

func TestAnchorSwap_Differential_BareLimit_Suppressed(t *testing.T) {
	// A bare LIMIT without a dominating total sort selects WHICH rows survive, so
	// the swap MUST be suppressed. Both engines return the same first-k bag.
	g := seedHubGraph(t)
	assertAnchorIdentical(t, g,
		"MATCH (a:Hub)<-[:R]-(b:Leaf) RETURN a.tag AS at, b.i AS bi LIMIT 1", false, false)
}

func TestAnchorSwap_Differential_BalancedNoSwap(t *testing.T) {
	// Written DirIn but the anchors are balanced: X has 4 nodes each receiving one
	// R edge from a distinct Y node (D(X,R,IN)=4, N(X)=4), and Y has 4 nodes each
	// with one R out-edge (D(Y,R,OUT)=4, N(Y)=4). The modeled costs are equal, so
	// there is no strict win under the margin → no swap. Result identity regardless.
	g := buildAnchorGraph(t,
		"UNWIND range(1,4) AS i CREATE (:Y {i:i})-[:R]->(:X {i:i})",
	)
	assertAnchorIdentical(t, g,
		"MATCH (a:X)<-[:R]-(b:Y) RETURN a.i AS ai, b.i AS bi", false, false)
}

func TestAnchorSwap_Differential_DirtyD_Vetoed(t *testing.T) {
	// The dirty-D veto: after a relabel dirties D(Hub,*,IN), the written anchor's
	// cost input D(Hub,R,IN) becomes EstFallback, so the swap that would otherwise
	// fire (proven by the first run) is vetoed on the second run. The relabel adds
	// :Hub to an isolated node with no R in-edges, so it dirties the IN family
	// without adding any match — the result multiset is unchanged across the
	// relabel, isolating the PLAN change from any result change.
	g := seedHubGraph(t)
	runWrite(t, NewEngine(g), "CREATE (:Iso {tag:9})") // isolated, no edges

	e := NewEngine(g)
	const q = "MATCH (a:Hub)<-[:R]-(b:Leaf) RETURN a.tag AS at, b.i AS bi"

	// First run: the swap fires (establishes the site is normally swappable).
	before := anchorSwapBuildCount.Load()
	firstRows := drainRows(t, e, q)
	if anchorSwapBuildCount.Load() == before {
		t.Fatalf("expected the anchor swap to fire on the clean run, but it did not")
	}

	// Relabel: add :Hub to the isolated node → dirties D(Hub,*,IN). The Iso node
	// has no R in-edges, so `(a:Hub)<-[:R]-(b:Leaf)` gains no match.
	runWrite(t, e, "MATCH (n:Iso) SET n:Hub")

	// Second run on the same engine: the cost gate re-evaluates against the now
	// dirty D(Hub,R,IN) cell → EstFallback → veto. The swap MUST NOT fire, and the
	// result multiset is unchanged.
	before = anchorSwapBuildCount.Load()
	secondRows := drainRows(t, e, q)
	if anchorSwapBuildCount.Load() != before {
		t.Fatalf("expected the anchor swap to be VETOED after the relabel dirtied D(Hub,IN), but it fired")
	}
	sort.Strings(firstRows)
	sort.Strings(secondRows)
	if fmt.Sprint(firstRows) != fmt.Sprint(secondRows) {
		t.Fatalf("result changed across the (match-neutral) relabel:\n  before = %v\n  after  = %v", firstRows, secondRows)
	}
}
