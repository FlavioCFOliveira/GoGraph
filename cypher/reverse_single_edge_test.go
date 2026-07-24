package cypher

// reverse_single_edge_test.go — tests for the result-identical single-edge
// expand reversal (#2089, docs/reordering-design.md §6).
//
// The reversal (mirrorAnchorSite) re-roots a single-edge pattern onto its other
// endpoint, traversing the SAME relationship in the opposite direction. Two
// properties are proven:
//
//   - Structural: the mirror of a translated pattern is byte-for-byte the plan
//     the translator emits for the openCypher-equivalent MIRROR pattern (both
//     directions, Incoming⇄Outgoing). Since openCypher mandates a pattern and its
//     direction-flipped rewrite are equivalent, this establishes result-identity
//     by construction.
//   - Differential: a directed pattern and its hand-written mirror return the
//     identical result set (both directions), and — end to end — the anchor-swap
//     peephole that APPLIES the reversal returns a result identical to the
//     written-order plan.

import (
	"fmt"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/cypher/parser"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// translateForTest parses and translates q to a logical plan.
func translateForTest(t *testing.T, q string) ir.LogicalPlan {
	t.Helper()
	a, err := parser.Parse(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	p, err := ir.FromAST(a)
	if err != nil {
		t.Fatalf("translate %q: %v", q, err)
	}
	return p
}

// firstAnchorSite returns the first structurally-matched single-edge site in a
// plan (pre-order), ignoring order-safety (which is a separate gate).
func firstAnchorSite(t *testing.T, plan ir.LogicalPlan) (anchorSite, bool) {
	t.Helper()
	var found anchorSite
	ok := false
	var walk func(ir.LogicalPlan)
	walk = func(p ir.LogicalPlan) {
		if p == nil || ok {
			return
		}
		if sel, is := p.(*ir.Selection); is {
			if s, m := matchAnchorSite(sel); m {
				found, ok = s, true
				return
			}
		}
		for _, c := range p.Children() {
			walk(c)
		}
	}
	walk(plan)
	return found, ok
}

// siteFingerprint renders the identity-relevant fields of a site for comparison.
func siteFingerprint(s *anchorSite) string {
	return fmt.Sprintf("from=%s:%s|to=%s:%s|rel=%s|dir=%s",
		s.fromVar, s.fromLabel, s.toVar, s.toLabel, s.relType, dirName(s.exp.Direction))
}

func dirName(d ir.Direction) string {
	switch d {
	case ir.DirectionIncoming:
		return "IN"
	case ir.DirectionOutgoing:
		return "OUT"
	default:
		return "BOTH"
	}
}

func TestReverseSingleEdge_Structural_IncomingToOutgoing(t *testing.T) {
	// `(a:A)<-[r:R]-(b:B)` is a DirIn expand rooted at a. Its mirror must equal the
	// plan for the equivalent `(b:B)-[r:R]->(a:A)` (a DirOut expand rooted at b).
	src, ok := firstAnchorSite(t, translateForTest(t, "MATCH (a:A)<-[r:R]-(b:B) RETURN a,b,r"))
	if !ok {
		t.Fatal("expected a matchable single-edge site in the Incoming pattern")
	}
	if src.exp.Direction != ir.DirectionIncoming {
		t.Fatalf("expected written direction Incoming, got %s", dirName(src.exp.Direction))
	}
	mirrorPlan := mirrorAnchorSite(&src)
	mirrorSite, ok := firstAnchorSite(t, mirrorPlan)
	if !ok {
		t.Fatal("mirror plan has no matchable single-edge site")
	}
	want, ok := firstAnchorSite(t, translateForTest(t, "MATCH (b:B)-[r:R]->(a:A) RETURN a,b,r"))
	if !ok {
		t.Fatal("expected a matchable single-edge site in the equivalent Outgoing pattern")
	}
	if siteFingerprint(&mirrorSite) != siteFingerprint(&want) {
		t.Fatalf("mirror != translator's equivalent plan:\n  mirror = %s\n  want   = %s",
			siteFingerprint(&mirrorSite), siteFingerprint(&want))
	}
	if mirrorSite.exp.Direction != ir.DirectionOutgoing {
		t.Fatalf("mirror direction = %s, want OUT", dirName(mirrorSite.exp.Direction))
	}
	if mirrorSite.exp.RelVar != src.exp.RelVar {
		t.Fatalf("mirror lost the relationship variable: got %q want %q", mirrorSite.exp.RelVar, src.exp.RelVar)
	}
}

func TestReverseSingleEdge_Structural_OutgoingToIncoming(t *testing.T) {
	// `(a:A)-[r:R]->(b:B)` is a DirOut expand rooted at a. Its mirror must equal the
	// plan for the equivalent `(b:B)<-[r:R]-(a:A)` (a DirIn expand rooted at b).
	src, ok := firstAnchorSite(t, translateForTest(t, "MATCH (a:A)-[r:R]->(b:B) RETURN a,b,r"))
	if !ok {
		t.Fatal("expected a matchable single-edge site in the Outgoing pattern")
	}
	if src.exp.Direction != ir.DirectionOutgoing {
		t.Fatalf("expected written direction Outgoing, got %s", dirName(src.exp.Direction))
	}
	mirrorSite, ok := firstAnchorSite(t, mirrorAnchorSite(&src))
	if !ok {
		t.Fatal("mirror plan has no matchable single-edge site")
	}
	want, ok := firstAnchorSite(t, translateForTest(t, "MATCH (b:B)<-[r:R]-(a:A) RETURN a,b,r"))
	if !ok {
		t.Fatal("expected a matchable single-edge site in the equivalent Incoming pattern")
	}
	if siteFingerprint(&mirrorSite) != siteFingerprint(&want) {
		t.Fatalf("mirror != translator's equivalent plan:\n  mirror = %s\n  want   = %s",
			siteFingerprint(&mirrorSite), siteFingerprint(&want))
	}
	if mirrorSite.exp.Direction != ir.DirectionIncoming {
		t.Fatalf("mirror direction = %s, want IN", dirName(mirrorSite.exp.Direction))
	}
}

func TestReverseSingleEdge_Match_DeclinesUnreversibleShapes(t *testing.T) {
	// Shapes matchAnchorSite must decline: each lacks the clean
	// Selection[label] → Expand[single-type] → NodeByLabelScan structure the
	// reversal relocates, so the peephole never touches them (a missed
	// optimisation, never an unsafe rewrite).
	cases := []struct {
		name string
		q    string
	}{
		{"var-length", "MATCH (a:A)-[r:R*1..3]->(b:B) RETURN a,b"},
		{"multi-type", "MATCH (a:A)-[r:R|S]->(b:B) RETURN a,b"},
		{"any-type", "MATCH (a:A)-[r]->(b:B) RETURN a,b"},
		{"unlabelled-to", "MATCH (a:A)-[r:R]->(b) RETURN a,b"},
		{"unlabelled-from", "MATCH (a)-[r:R]->(b:B) RETURN a,b"},
		{"from-property", "MATCH (a:A {x:1})-[r:R]->(b:B) RETURN a,b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := firstAnchorSite(t, translateForTest(t, tc.q)); ok {
				t.Fatalf("matcher accepted %q, but the reversal is not admissible for this shape", tc.q)
			}
		})
	}
}

func TestReverseSingleEdge_Collector_DeclinesNamedPath(t *testing.T) {
	// A named path reconstructs its value from THIS pattern's traversal triplet, so
	// reversing the edge would reverse the path's node order. The inner
	// Selection[(b:B)] shape still matches at the structural level, but the
	// candidate COLLECTOR must decline it because of the NamedPath ancestor.
	plan := translateForTest(t, "MATCH p=(a:A)-[r:R]->(b:B) RETURN p")
	if _, ok := firstAnchorSite(t, plan); !ok {
		t.Fatal("precondition: the inner single-edge shape should still match structurally")
	}
	if got := collectAnchorSwapCandidates(plan); len(got) != 0 {
		t.Fatalf("collector kept %d candidate(s) under a NamedPath; want 0 (reversal changes the path value)", len(got))
	}
}

func TestReverseSingleEdge_Collector_AcceptsEndpointProperty(t *testing.T) {
	// A property filter on either endpoint sits as a Selection ABOVE the matched
	// inner Selection[(b:B)] and references a variable the reversal still binds, so
	// the swap is result-identical and the collector keeps the site. (A property
	// BELOW the expand — on the from-side scan — makes the expand's child a
	// Selection, which matchAnchorSite declines; that asymmetry is covered by the
	// from-property decline case above.)
	plan := translateForTest(t, "MATCH (a:A)-[r:R]->(b:B {y:2}) RETURN a,b")
	if got := collectAnchorSwapCandidates(plan); len(got) != 1 {
		t.Fatalf("collector kept %d candidate(s) for a to-endpoint property filter; want 1", len(got))
	}
}

// seedTypedTriples builds a small A-[:R]->B graph: n distinct (A_i)-[:R]->(B_i)
// edges, so a reversal that mixed up endpoints or edges would diverge.
func seedTypedTriples(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	return buildAnchorGraph(t,
		fmt.Sprintf("UNWIND range(1,%d) AS i CREATE (:A {i:i})-[:R]->(:B {i:i})", n),
	)
}

func TestReverseSingleEdge_Differential_EquivalentQueries(t *testing.T) {
	// The reversal's semantic contract: a directed pattern and its hand-written
	// mirror return the identical result set. Proven for both written directions on
	// the same graph with a plain read engine (no peephole involved), so this
	// isolates reversal SEMANTICS from the swap POLICY.
	g := seedTypedTriples(t, 20)
	e := NewEngine(g)
	forward := drainRows(t, e, "MATCH (a:A)-[r:R]->(b:B) RETURN a.i AS ai, b.i AS bi")
	mirror := drainRows(t, e, "MATCH (b:B)<-[r:R]-(a:A) RETURN a.i AS ai, b.i AS bi")
	sort.Strings(forward)
	sort.Strings(mirror)
	if len(forward) != len(mirror) {
		t.Fatalf("row-count mismatch: forward=%d mirror=%d", len(forward), len(mirror))
	}
	for i := range forward {
		if forward[i] != mirror[i] {
			t.Fatalf("reversal changed the result at row %d:\n  forward = %s\n  mirror  = %s", i, forward[i], mirror[i])
		}
	}
}

func TestReverseSingleEdge_Differential_AppliedByPeephole(t *testing.T) {
	// End to end: the peephole APPLIES the reversal for an OUT-ward swap. On a graph
	// where `(t:B)<-[:R]-(s:A)` re-roots onto A, the swap-ON engine RUNS the mirror
	// while the swap-OFF engine runs the written plan; both must produce the
	// identical bag. B is the high-in-degree side (200 Filler→B edges), A the
	// low-out-degree side re-rooted onto. Projecting type(r), startNode(r).i and
	// endNode(r).i asserts DATA-DIRECTION FIDELITY (§6): the re-root flips the START
	// endpoint of traversal but must NOT flip the semantic edge direction, so the
	// relationship's start/end are identical whether the plan traversed it forward
	// (OFF) or in reverse (ON).
	g := buildAnchorGraph(t,
		"CREATE (:B {tag:0})",
		"MATCH (x:B) UNWIND range(1,200) AS i CREATE (:Filler {i:i})-[:R]->(x)",
		"UNWIND range(1,30) AS i CREATE (:A {i:i})",
		"MATCH (x:B),(a:A {i:7}) CREATE (a)-[:R]->(x)",
	)
	// `(t:B)<-[r:R]-(s:A)`: anchor-B walks ~201 in-edges (DirIn); re-root onto A
	// (DirOut) walks 1. OUT-ward → swap fires. The single true match is (A i=7)→B.
	assertAnchorIdentical(t, g,
		"MATCH (t:B)<-[r:R]-(s:A) RETURN t.tag AS tt, s.i AS si, type(r) AS rt, "+
			"startNode(r).i AS sn, endNode(r).tag AS en", true, false)
}
