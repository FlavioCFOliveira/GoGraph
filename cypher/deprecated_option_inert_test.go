package cypher

// deprecated_option_inert_test.go — the contract test for
// [EngineOptions.EdgeTypeFilterCacheCapacity] after rmp #2251 retired the cache
// it used to size.
//
// The field is kept only so code written against an earlier release keeps
// compiling. "Kept" is a promise about the COMPILE; this test is the promise
// about the RUNTIME: whatever value is set, the Engine must behave identically.
// A silently-honoured option would be worse than a removed one, and a silently
// ALLOCATING one worse still, so the assertion covers results and plans
// byte-for-byte across the whole legal range including zero and negative.
//
// # Results and plans alone would NOT be enough, and that is why the counters are here
//
// The cache this option sized was a pure AMORTISATION: it never changed a result
// and never changed a plan, only how often the O(V+E) build ran. So a
// results-and-plans comparison would have passed even against the old,
// fully-honoured option, and would prove nothing about inertness. The signal that
// CAN tell the two apart is the structural one: with a per-type-set cache in
// force, capacity 1 evicts between the three type sets below and rebuilds where
// capacity 1 000 000 does not. [slotTypeResolveCount] and
// [csrPairUncachedBuildCount] are therefore bracketed per arm and required to be
// IDENTICAL across every capacity, and non-zero, so the test can actually fail.
//
// It lives in package cypher rather than cypher_test deliberately: staticcheck's
// SA1019 exempts uses of a deprecated identifier from the package that declares
// it, so the one place in the module allowed to reference the field is the file
// that pins its inertness.

import (
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// inertOptionQueries exercise the machinery the retired cache served: typed
// expansion in both directions, over SEVERAL distinct relationship-type sets.
// One type set could not distinguish a working per-type-set cache from an absent
// one; three can, which is the whole reason the capacity existed.
var inertOptionQueries = []string{
	`MATCH (a)-[r:K]->(b) RETURN a.sid, b.sid ORDER BY a.sid, b.sid`,
	`MATCH (a)-[r:M]->(b) RETURN a.sid, b.sid ORDER BY a.sid, b.sid`,
	`MATCH (a)-[r:K|M]->(b) RETURN count(r) AS c`,
	`MATCH (a)<-[r:K]-(b) RETURN a.sid, b.sid ORDER BY a.sid, b.sid`,
	`MATCH (a)-[r:K]-(b) RETURN count(r) AS c`,
}

// buildInertOptionFixture builds a small multigraph carrying two relationship
// types and a node with several incoming arcs.
func buildInertOptionFixture(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	keys := []string{"a", "b", "c", "d"}
	for _, k := range keys {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", k, err)
		}
		if err := g.SetNodeProperty(k, "sid", lpg.StringValue(k)); err != nil {
			t.Fatalf("SetNodeProperty(%q): %v", k, err)
		}
	}
	for _, e := range [][3]string{
		{"a", "b", "K"}, {"b", "c", "M"}, {"c", "d", "K"}, {"a", "d", "M"}, {"d", "b", "K"},
	} {
		if err := g.AddEdge(e[0], e[1], 1); err != nil {
			t.Fatalf("AddEdge(%v): %v", e, err)
		}
		g.SetEdgeLabel(e[0], e[1], e[2])
	}
	return g
}

// TestEdgeTypeFilterCacheCapacityOptionIsInert proves the deprecated option
// changes nothing: every capacity produces byte-identical results AND a
// byte-identical plan for every query.
func TestEdgeTypeFilterCacheCapacityOptionIsInert(t *testing.T) {
	// Zero (the "use the default" encoding), negative (the misconfiguration the
	// retired constructor clamped), one (the capacity that used to force an
	// eviction on every second type set), the published default, and an absurdly
	// large value. If any of these were still honoured, the one-capacity arm would
	// diverge from the large-capacity arm.
	capacities := []int{0, -1, 1, DefaultEdgeTypeFilterCacheCapacity, 1_000_000}

	var wantResults, wantPlans []string
	var wantResolves, wantPairs uint64
	var wantFrom int
	for i, capacity := range capacities {
		g := buildInertOptionFixture(t)
		eng := NewEngineWithOptions(g, EngineOptions{EdgeTypeFilterCacheCapacity: capacity})

		// Bracket the arm: read the structural counters immediately before and
		// immediately after the drive, so nothing outside it is folded in.
		resolvesBefore := slotTypeResolveCount.Load()
		pairsBefore := csrPairUncachedBuildCount.Load()

		results := make([]string, 0, len(inertOptionQueries))
		plans := make([]string, 0, len(inertOptionQueries))
		for _, q := range inertOptionQueries {
			rows := degreeRun(t, eng, q)
			if len(rows) == 0 {
				t.Fatalf("capacity=%d: query returned no rows, so this comparison is vacuous: %s",
					capacity, q)
			}
			results = append(results, q+" => "+strings.Join(rows, "|"))

			plan, err := eng.Explain(q, nil)
			if err != nil {
				t.Fatalf("capacity=%d: Explain(%q): %v", capacity, q, err)
			}
			plans = append(plans, q+" => "+plan)
		}

		resolves := slotTypeResolveCount.Load() - resolvesBefore
		pairs := csrPairUncachedBuildCount.Load() - pairsBefore

		// Non-vacuity: at least one plan must actually contain an Expand, or the
		// battery is not exercising typed expansion at all and could not detect a
		// change in it.
		if !strings.Contains(strings.Join(plans, "\n"), "Expand") {
			t.Fatalf("capacity=%d: no plan contains an Expand operator, so the queries do not "+
				"exercise the typed-expansion path this option used to affect", capacity)
		}
		// Non-vacuity: the drive must have done the O(V+E) work at least once, or a
		// zero-versus-zero counter comparison below would agree about nothing.
		if resolves == 0 || pairs == 0 {
			t.Fatalf("capacity=%d: the drive performed %d slot-type resolutions and %d CSR pair "+
				"builds; a counter that never moves cannot detect a honoured option",
				capacity, resolves, pairs)
		}

		if i == 0 {
			wantResults, wantPlans, wantFrom = results, plans, capacity
			wantResolves, wantPairs = resolves, pairs
			continue
		}
		if resolves != wantResolves || pairs != wantPairs {
			t.Errorf("STRUCTURAL WORK DIFFERS between EdgeTypeFilterCacheCapacity=%d and =%d: "+
				"resolutions %d vs %d, pair builds %d vs %d. The option is still sizing "+
				"something — a capacity that evicts would rebuild more often than one that "+
				"does not, which is exactly this difference.",
				wantFrom, capacity, wantResolves, resolves, wantPairs, pairs)
		}
		for k := range results {
			if results[k] != wantResults[k] {
				t.Errorf("RESULTS DIFFER between EdgeTypeFilterCacheCapacity=%d and =%d — the "+
					"deprecated option is still being honoured somewhere.\n  at %d: %s\n  at %d: %s",
					wantFrom, capacity, wantFrom, wantResults[k], capacity, results[k])
			}
			if plans[k] != wantPlans[k] {
				t.Errorf("PLANS DIFFER between EdgeTypeFilterCacheCapacity=%d and =%d — the "+
					"deprecated option is reaching the planner.\n  at %d:\n%s\n  at %d:\n%s",
					wantFrom, capacity, wantFrom, wantPlans[k], capacity, plans[k])
			}
		}
	}
	t.Logf("inert across %d capacities %v: %d queries; results, plans and structural work "+
		"(%d slot-type resolutions, %d CSR pair builds per arm) all identical",
		len(capacities), capacities, len(inertOptionQueries), wantResolves, wantPairs)
}
