package cypher

// index_intersect_engagement_test.go — the ENGAGEMENT gate for the conjunctive
// index-intersection recogniser (#2134, budgeted by #2266).
//
// Layer: short.
//
// # Why this file exists separately from the correctness gate
//
// #2266 made the recogniser's decision cheaper. The failure mode a cheaper gate
// invites is not a wrong answer — it is a gate that quietly stops firing. That
// failure is INVISIBLE to every other check in this package: the residual Filter
// is mandatory for a composed probe, so a composition that silently stops
// composing returns byte-identical rows, keeps the differential green, keeps the
// absolute oracle green, AND makes the plan-time benchmark faster. It would read
// as a clean win in every instrument the sprint has.
//
// So this file pins the VERDICT — composed or not composed — for every shape the
// plan-time benchmarks measure, plus the shapes the correctness gate already
// covers, using two independent white-box signals:
//
//   - EXPLAIN, which renders the composed marker only when the intersected access
//     path was actually built; and
//   - indexIntersectBuildCount, the planner's own build counter.
//
// Neither is derivable from the result rows, which is the whole point.
//
// The test is deliberately NOT parallel: indexIntersectBuildCount is
// process-global, and Go resumes t.Parallel() tests only after the sequential
// ones finish, so staying sequential is what keeps the counter delta attributable.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// iiSmallPop is BELOW rangeSeekMinLabelPopulation, so every conjunct over
// the :Small label is refused by the population floor no matter how selective its
// range is. Such a label is the purest statement of the #2266 defect: not one
// cardinality probe can influence the outcome, so every probe it pays is waste by
// construction.
// It was 512, which sat under the floor when the floor was 1024. #2367 lowered
// the floor to 64 on measured evidence, which put 512 ABOVE it — at which point
// this fixture would have kept passing while proving nothing about the floor.
const iiSmallPop = 32

// iiSmallLabelFixture builds a :Small graph whose population is below the seek
// floor, with btree indexes on BOTH a and b — so a shape over it is declined by
// the floor and not merely by a missing index.
func iiSmallLabelFixture(tb testing.TB) (*lpg.Graph[string, float64], *Engine) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < iiSmallPop; i++ {
		key := fmt.Sprintf("m%06d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Small"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "a", lpg.Int64Value(int64(i%64))); err != nil {
			tb.Fatalf("SetNodeProperty a: %v", err)
		}
		if err := g.SetNodeProperty(key, "b", lpg.Int64Value(int64(i/8))); err != nil {
			tb.Fatalf("SetNodeProperty b: %v", err)
		}
	}
	eng := NewEngineWithOptions(g, EngineOptions{MaxResultRows: MaxResultRowsUnlimited})
	for _, ddl := range []string{
		`CREATE INDEX FOR (n:Small) ON (n.a) OPTIONS {indexType:'btree'}`,
		`CREATE INDEX FOR (n:Small) ON (n.b) OPTIONS {indexType:'btree'}`,
	} {
		if _, err := eng.Run(context.Background(), ddl, nil); err != nil {
			tb.Fatalf("%s: %v", ddl, err)
		}
	}
	return g, eng
}

// iiBuildsPerExplain is how many times ONE plan build composes the intersection
// for a shape that accepts it.
//
// It is TWO, not one, and that is worth pinning rather than papering over: the
// planner runs the whole range-seek peephole a second time as a throwaway probe
// inside indexSeekWouldFire, which builds a real operator purely to answer "would
// a seek fire here?" and then closes it. So every cost inside the recogniser —
// including the range counts #2266 budgets — is paid TWICE per query build, once
// by the probe and once by the build that keeps its result.
//
// A change to this number is a change to how many times the planner walks that
// path. It should fail here and be read, not silently absorbed.
const iiBuildsPerExplain = 2

// iiEngagementCase is one shape and the verdict the planner must reach for it.
type iiEngagementCase struct {
	name        string
	query       string
	why         string
	wantCompose bool
}

// iiEngagementCases covers every shape BenchmarkIndexIntersectPlan_* measures.
// A plan-time win is only a win if these verdicts are unchanged by it.
var iiEngagementCases = []iiEngagementCase{{
	name:        "accept_two_numeric",
	query:       `MATCH (n:Doc) WHERE n.a < 10 AND n.b < 30 RETURN n.s AS s`,
	wantCompose: true,
	why:         "two selective conjuncts on different indexed numeric properties",
}, {
	name:        "accept_numeric_and_string",
	query:       `MATCH (n:Doc) WHERE n.a < 10 AND n.s < "s000300" RETURN n.s AS s`,
	wantCompose: true,
	why:         "the numeric companion composes with a string btree",
}, {
	name:        "accept_two_of_three",
	query:       `MATCH (n:Doc) WHERE n.a < 10 AND n.b < 30 AND n.s < "s019000" RETURN n.s AS s`,
	wantCompose: true,
	why:         "two conjuncts compose; the third is too broad and is left to the residual Filter",
}, {
	name:        "decline_broad_numeric",
	query:       `MATCH (n:Doc) WHERE n.a < 900 AND n.b < 900 RETURN n.s AS s`,
	wantCompose: false,
	why:         "each conjunct covers ~90% of the label, so neither passes the selectivity ceiling",
}, {
	name:        "decline_broad_string",
	query:       `MATCH (n:Doc) WHERE n.s > "s000000" AND n.a < 900 RETURN n.s AS s`,
	wantCompose: false,
	why:         "an open-ended string range covers the whole index; the numeric side is broad too",
}, {
	name:        "decline_one_indexed",
	query:       `MATCH (n:Doc) WHERE n.a < 900 AND n.missing = 1 RETURN n.s AS s`,
	wantCompose: false,
	why:         "only one side is indexed, so fewer than two parts survive",
}, {
	name:        "control_single_conjunct",
	query:       `MATCH (n:Doc) WHERE n.a < 10 RETURN n.s AS s`,
	wantCompose: false,
	why:         "not a conjunction at all — the recogniser bails at its first check",
}, {
	name:        "control_no_index",
	query:       `MATCH (n:Doc) WHERE n.missing1 < 10 AND n.missing2 < 30 RETURN n.s AS s`,
	wantCompose: false,
	why:         "no covering index on either property, so no count is ever reached",
}}

// TestIndexIntersect_EngagementVerdictsAreStable pins which shapes compose. It is
// the acceptance evidence for #2266 AC 2: a recogniser that stopped firing would
// leave every result identical and every benchmark faster, so only a white-box
// verdict can tell a real speed-up from a silent capitulation.
func TestIndexIntersect_EngagementVerdictsAreStable(t *testing.T) {
	g, _ := iiFixture(t)
	eng := iiEngine(t, g, false)

	for _, tc := range iiEngagementCases {
		t.Run(tc.name, func(t *testing.T) {
			before := indexIntersectBuildCount.Load()
			plan, err := eng.Explain(tc.query, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			built := indexIntersectBuildCount.Load() - before

			composed := strings.Contains(plan, composedMarker)
			if composed != tc.wantCompose {
				t.Fatalf("EXPLAIN says composed=%v, want %v (%s)\nplan:\n%s",
					composed, tc.wantCompose, tc.why, plan)
			}
			// The counter must agree with the rendered plan. They are independent
			// signals — one is read off the built operator tree, the other is
			// incremented inside the planner — so a disagreement means the plan
			// text no longer reflects what the planner did.
			wantBuilt := uint64(0)
			if tc.wantCompose {
				wantBuilt = iiBuildsPerExplain
			}
			if built != wantBuilt {
				t.Fatalf("indexIntersectBuildCount advanced by %d, want %d (%s); "+
					"the build counter disagrees with EXPLAIN", built, wantBuilt, tc.why)
			}
		})
	}
}

// TestIndexIntersect_SmallLabelNeverComposes pins the population floor, which
// #2266 hoisted ahead of the counting. Hoisting a check changes WHEN a verdict is
// reached; this asserts it did not change WHAT the verdict is.
func TestIndexIntersect_SmallLabelNeverComposes(t *testing.T) {
	g, eng := iiSmallLabelFixture(t)

	const q = `MATCH (n:Small) WHERE n.a < 60 AND n.b < 60 RETURN n.a AS a`
	before := indexIntersectBuildCount.Load()
	plan, err := eng.Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if built := indexIntersectBuildCount.Load() - before; built != 0 {
		t.Fatalf("the intersection composed %d times over a label of %d nodes, "+
			"which is below the seek floor of %d", built, iiSmallPop, rangeSeekMinLabelPopulation)
	}
	if strings.Contains(plan, composedMarker) {
		t.Fatalf("a sub-floor label composed:\n%s", plan)
	}
	// Anti-degeneracy: the shape must really be a conjunction of two INDEXED
	// properties, so the floor is what declines it rather than a missing index.
	for _, want := range []string{"n.a", "n.b"} {
		if !strings.Contains(q, want) {
			t.Fatalf("the fixture query lost its %s conjunct", want)
		}
	}
	// Each CREATE INDEX registers a string btree AND its unified numeric companion
	// (#1652), so two DDL statements yield four subscribers. Both properties hold
	// integers, so it is the *_num companions that a conjunct here would probe.
	got := g.IndexManager().ListIndexes()
	for _, want := range []string{"small_a_btree_num", "small_b_btree_num"} {
		found := false
		for _, name := range got {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("the sub-floor fixture is missing index %q (has %v); the test only "+
				"proves the population floor declines when both properties ARE indexed", want, got)
		}
	}
}
