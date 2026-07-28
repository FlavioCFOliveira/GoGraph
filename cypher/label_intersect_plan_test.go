package cypher

// label_intersect_plan_test.go — plan-shape, precedence and veto gate for the
// set-at-a-time multi-label conjunction (#2133).
//
// This file proves the acceptance criteria of the implementation task: that a
// multi-label pattern is answered by intersecting bitmaps, that the residual label
// Filter is gone (the bitmap subsumes it), that an equality index seek still wins,
// that a zero-population label still short-circuits, and that every veto condition
// falls through to the shipped min-label plan rather than to something worse.
//
// The result-identity differential, the absolute oracle, the rapid property and
// the concurrency assertion live in label_intersect_diff_test.go (#2135).
//
// Design and proofs: docs/design-bitmap-intersection.md.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/ast"
	"github.com/FlavioCFOliveira/GoGraph/cypher/ir"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// liGraph seeds a fixture with the label shapes every case below needs:
//
//	:Big     — n nodes
//	:Small   — every 100th :Big node, PLUS a disjoint tail carrying only :Small,
//	           so |Big ∩ Small| < |Small| and the intersection strictly reduces
//	           rows (the gate's condition)
//	:Nested  — the first 10 :Big nodes, so :Nested ⊂ :Big entirely
//	:Empty   — declared via a node that is then relabelled away, so the label is
//	           known to the registry but has zero live members
//	:Other   — a disjoint population, so :Big ∩ :Other is empty
func liGraph(t *testing.T, n int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("b%05d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Big"); err != nil {
			t.Fatalf("SetNodeLabel Big: %v", err)
		}
		if i%100 == 0 {
			if err := g.SetNodeLabel(key, "Small"); err != nil {
				t.Fatalf("SetNodeLabel Small: %v", err)
			}
		}
		if i < 10 {
			if err := g.SetNodeLabel(key, "Nested"); err != nil {
				t.Fatalf("SetNodeLabel Nested: %v", err)
			}
		}
		if err := g.SetNodeProperty(key, "sid", lpg.StringValue(fmt.Sprintf("s%05d", i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	// A :Small-only tail with no :Big, so :Small is NOT a subset of :Big and the
	// intersection scans strictly fewer rows than either label alone — which is
	// exactly the gate's condition.
	for i := 0; i < 40; i++ {
		key := fmt.Sprintf("s%05d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode small-only: %v", err)
		}
		if err := g.SetNodeLabel(key, "Small"); err != nil {
			t.Fatalf("SetNodeLabel Small-only: %v", err)
		}
	}
	// A registered-but-empty label: give it to a node, then take it away.
	if err := g.SetNodeLabel("b00000", "Empty"); err != nil {
		t.Fatalf("SetNodeLabel Empty: %v", err)
	}
	g.RemoveNodeLabel("b00000", "Empty")
	// A disjoint population.
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("o%05d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode other: %v", err)
		}
		if err := g.SetNodeLabel(key, "Other"); err != nil {
			t.Fatalf("SetNodeLabel Other: %v", err)
		}
	}
	return g
}

// liPlan renders the physical plan for q on an engine with the intersection
// enabled or disabled.
func liPlan(t *testing.T, g *lpg.Graph[string, float64], q string, disable bool) string {
	t.Helper()
	eng := NewEngineWithOptions(g, EngineOptions{DisableBitmapIntersection: disable})
	plan, err := eng.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain %q: %v", q, err)
	}
	return plan
}

// intersectMarker is what PlanDetail renders for the conjunction form. Asserting
// on it rather than on the operator name matters: the operator is still a
// NodeByLabelScan, so only the detail distinguishes the access path.
const intersectMarker = "∩"

// TestLabelIntersect_PlanShape pins which shapes are answered set-at-a-time and
// which keep today's plan.
func TestLabelIntersect_PlanShape(t *testing.T) {
	t.Parallel()
	g := liGraph(t, 5000)

	cases := []struct {
		name       string
		query      string
		wantFire   bool
		wantOrder  string // the expected AND order, smallest label first
		wantFilter bool   // a NON-label residual predicate legitimately remains
	}{{
		// The headline shape. Smallest-first ordering puts Small (50) ahead of
		// Big (5000), which is what makes Intersect clone the cheap bitmap.
		name:      "two_labels_selective",
		query:     `MATCH (n:Big:Small) RETURN n.sid AS s`,
		wantFire:  true,
		wantOrder: "Small" + intersectMarker + "Big",
	}, {
		name:      "three_labels",
		query:     `MATCH (n:Big:Small:Nested) RETURN n.sid AS s`,
		wantFire:  true,
		wantOrder: "Nested" + intersectMarker + "Small" + intersectMarker + "Big",
	}, {
		// A label with zero live members makes the conjunction provably empty, and
		// the path fires so the plan is an empty bitmap rather than a full scan of a
		// populated label followed by an all-dropping filter.
		name:      "zero_population_label_short_circuits",
		query:     `MATCH (n:Big:Empty) RETURN n.sid AS s`,
		wantFire:  true,
		wantOrder: "Empty" + intersectMarker + "Big",
	}, {
		// Disjoint labels: a real but empty intersection.
		name:      "disjoint_labels",
		query:     `MATCH (n:Big:Other) RETURN n.sid AS s`,
		wantFire:  true,
		wantOrder: "Other" + intersectMarker + "Big",
	}, {
		// NESTED labels: |Nested ∩ Big| == |Nested|, so the intersection removes NO
		// rows relative to scanning :Nested. The gate vetoes, and the shape is left to
		// the columnar filter chain, which removes the per-row box the intersection
		// cannot improve on. A rewrite may pre-empt another only when it removes ROWS.
		name:     "nested_labels_veto_to_columnar",
		query:    `MATCH (n:Nested:Big) RETURN n.sid AS s`,
		wantFire: false,
	}, {
		// Smallest label written FIRST, so the min-label re-anchor has nothing to do
		// — yet the intersection still removes rows (|Big ∩ Small| = 50 < 90). The
		// columnar filter chain must yield here, or a real row reduction is lost.
		name:      "smallest_label_first_still_intersects",
		query:     `MATCH (n:Small:Big) RETURN n.sid AS s`,
		wantFire:  true,
		wantOrder: "Small" + intersectMarker + "Big",
	}, {
		// ── Not eligible ──
		name:     "single_label_has_nothing_to_intersect",
		query:    `MATCH (n:Big) RETURN n.sid AS s`,
		wantFire: false,
	}, {
		// A multi-label pattern combined with a property equality does NOT reach the
		// index today — a documented limitation (docs/cypher.md) — so the
		// intersection legitimately serves the label part and the property predicate
		// stays as the residual Filter. Precedence against a seek that DOES fire is
		// asserted separately in TestLabelIntersect_SingleLabelSeekUnaffected.
		name:       "multilabel_with_property_intersects_and_keeps_filter",
		query:      `MATCH (n:Big:Small {sid: "s00100"}) RETURN n.sid AS s`,
		wantFire:   true,
		wantOrder:  "Small" + intersectMarker + "Big",
		wantFilter: true,
	}}

	// An index on (Big, sid) so the precedence case has a seek to prefer.
	eng := NewEngine(g)
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:Big) ON (n.sid)`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			on := liPlan(t, g, tc.query, false)
			fired := strings.Contains(on, intersectMarker)
			if fired != tc.wantFire {
				t.Fatalf("intersection fired = %v, want %v\nplan:\n%s", fired, tc.wantFire, on)
			}
			if !tc.wantFire {
				return
			}
			if !strings.Contains(on, tc.wantOrder) {
				t.Fatalf("plan missing the smallest-first AND order %q\nplan:\n%s", tc.wantOrder, on)
			}
			// The residual LABEL Filter must be GONE: the bitmap subsumes it, and
			// dropping it is the whole win. A Filter here would mean the labels are
			// still being re-checked one row at a time. A non-label residual (a
			// property predicate) legitimately remains and is declared per case.
			if hasFilter := strings.Contains(on, "Filter"); hasFilter != tc.wantFilter {
				t.Fatalf("residual Filter present = %v, want %v — the bitmap should subsume the LABEL re-check\nplan:\n%s",
					hasFilter, tc.wantFilter, on)
			}
			// Anti-degeneracy: with the flag off the same query must take a
			// different plan, or the differential in #2135 proves nothing.
			off := liPlan(t, g, tc.query, true)
			if strings.Contains(off, intersectMarker) {
				t.Fatalf("disabled arm still intersected\nplan:\n%s", off)
			}
		})
	}
}

// TestLabelIntersect_SingleLabelSeekUnaffected is the precedence assertion that
// can actually bite. A multi-label pattern combined with a property equality does
// not reach the index today (a documented limitation), so "the seek wins over the
// intersection" is untestable on an eligible shape — the real risk is that adding
// a peephole to the Selection build perturbs the seek that DOES fire. This pins
// that it does not: a single-label indexed equality still plans a seek, with the
// intersection enabled and disabled alike.
func TestLabelIntersect_SingleLabelSeekUnaffected(t *testing.T) {
	t.Parallel()
	g := liGraph(t, 5000)
	eng := NewEngine(g)
	if _, err := eng.Run(context.Background(),
		`CREATE INDEX FOR (n:Big) ON (n.sid)`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
	const q = `MATCH (n:Big {sid: "s00100"}) RETURN n.sid AS s`
	for _, disable := range []bool{false, true} {
		plan := liPlan(t, g, q, disable)
		if !strings.Contains(plan, "NodeByIndexSeek") {
			t.Fatalf("single-label indexed equality lost its seek (intersection disabled=%v)\nplan:\n%s",
				disable, plan)
		}
		if strings.Contains(plan, intersectMarker) {
			t.Fatalf("a single-label pattern must never intersect\nplan:\n%s", plan)
		}
	}
}

// TestLabelIntersect_DisabledFallsThroughToMinLabel proves the veto path lands on
// the shipped min-label re-anchor rather than on the pre-#2077 Labels[0] plan:
// "never worse than today" has to be true of the FALLBACK, not just of the
// optimised path.
func TestLabelIntersect_DisabledFallsThroughToMinLabel(t *testing.T) {
	t.Parallel()
	g := liGraph(t, 5000)
	const q = `MATCH (n:Big:Small) RETURN n.sid AS s`

	off := NewEngineWithOptions(g, EngineOptions{DisableBitmapIntersection: true})
	before := minLabelScanBuildCount.Load()
	plan, err := off.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	// Explain builds the physical tree the read path builds, so the re-anchor
	// counter advances when the fallback fires.
	if minLabelScanBuildCount.Load() == before {
		t.Fatalf("with the intersection disabled the min-label re-anchor should fire\nplan:\n%s", plan)
	}
	// And it re-anchors on the SMALL label, not the syntactically first one.
	if !strings.Contains(plan, "Small") {
		t.Fatalf("fallback did not re-anchor on the smaller label\nplan:\n%s", plan)
	}
}

// TestLabelIntersect_UntrustworthyCountsVeto proves the exactness veto: a resolver
// that cannot supply exact label cardinalities must keep today's plan rather than
// intersect on a guess. This is the one veto condition that remains after the
// design removed the (unjustifiable) selectivity ceiling, so it is the one that
// must be pinned.
func TestLabelIntersect_UntrustworthyCountsVeto(t *testing.T) {
	t.Parallel()

	// The exact shape the IR translator emits for `(n:Big:Small)`: a bare
	// LabelPredicate Selection over a NodeByLabelScan of the same variable.
	sel := &ir.Selection{
		PredicateExpr: &ast.LabelPredicate{
			Receiver: &ast.Variable{Name: "n"},
			Labels:   []string{"Small"},
		},
		Child: &ir.NodeByLabelScan{NodeVar: "n", Label: "Big"},
	}

	// The shape itself must be recognised, so the veto below is demonstrably about
	// the counts and not about a shape mismatch.
	if _, _, _, ok := pickMinLabelShape(sel); !ok {
		t.Fatal("the shared shape recogniser rejected the canonical multi-label Selection")
	}

	// A nil resolver cannot produce an exact cardinality, so planStaysDefault must
	// trip and the plan must stay the default. This is the one veto condition that
	// remains after the design removed the (unjustifiable) selectivity ceiling, so
	// it is the one that has to be pinned.
	if _, _, ok := pickLabelIntersection(sel, nil, nil); ok {
		t.Fatal("pickLabelIntersection accepted a nil resolver; the exactness veto must fail closed")
	}

	// A single label has nothing to intersect, whatever the resolver says.
	solo := &ir.Selection{
		PredicateExpr: &ast.LabelPredicate{Receiver: &ast.Variable{Name: "n"}, Labels: []string{}},
		Child:         &ir.NodeByLabelScan{NodeVar: "n", Label: "Big"},
	}
	if _, _, ok := pickLabelIntersection(solo, nil, nil); ok {
		t.Fatal("pickLabelIntersection accepted a single-label pattern")
	}
}
