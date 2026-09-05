package cypher

// degree_tombstone_cap_test.go — rmp #2265 at the Cypher layer: the untyped
// degree count answers correctly when its cap is honoured INSIDE a walk whose
// leading adjacency slots are tombstoned.
//
// The cap's own contract is pinned one layer down, in
// graph/lpg/out_degree_bounded_test.go, which asserts literal degrees under every
// interesting limit. What is pinned HERE is that the layer which asks for the cap
// still gets the right answers — EXISTS caps at 1, a comparison against a literal
// caps at literal+1 — and which query shapes reach the degree path at all.

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// degreeCapFixture builds three :H hubs over one graph:
//
//	"h"       5 tombstoned out-edges FIRST, then 3 live ones  → live degree 3
//	"allDead" 5 tombstoned out-edges only                     → live degree 0
//	"clean"   3 live out-edges, none dead                     → live degree 3
//
// Those three numbers — 3, 0, 3 — are the whole oracle for this file, and the
// leading-tombstone layout of "h" is what makes a wrongly charged cap visible.
func degreeCapFixture(t *testing.T, dead, live int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})

	add := func(key, label string) {
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		if err := g.SetNodeLabel(key, label); err != nil {
			t.Fatalf("SetNodeLabel(%s): %v", key, err)
		}
		// A stable identifier the queries can select by. Internal node ids are not
		// usable here: they are an allocation order, not a fixture property.
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(key)); err != nil {
			t.Fatalf("SetNodeProperty(%s.name): %v", key, err)
		}
	}
	edge := func(src, dst string) {
		if err := g.AddEdgeLabeled(src, dst, 1, "K"); err != nil {
			t.Fatalf("AddEdgeLabeled(%s->%s): %v", src, dst, err)
		}
	}

	add("h", "H")
	add("allDead", "H")
	add("clean", "H")

	// DEAD FIRST — the order of these calls is the order of the adjacency column.
	for i := 0; i < dead; i++ {
		d := fmt.Sprintf("dead%d", i)
		add(d, "D")
		edge("h", d)
		edge("allDead", d)
	}
	for i := 0; i < live; i++ {
		l := fmt.Sprintf("live%d", i)
		add(l, "L")
		edge("h", l)
		edge("clean", l)
	}
	for i := 0; i < dead; i++ {
		g.RemoveNode(fmt.Sprintf("dead%d", i))
	}
	return g
}

// TestDegreeRewrite_CappedCountIsCorrectOverLeadingTombstones drives every capped
// shape the evaluator produces against the fixture's hand-counted degrees
// {h: 3, allDead: 0, clean: 3}.
func TestDegreeRewrite_CappedCountIsCorrectOverLeadingTombstones(t *testing.T) {
	eng := NewEngine(degreeCapFixture(t, 5, 3))

	cases := []struct {
		name string
		q    string
		want []string
	}{
		// Uncapped, so the true degrees are visible and the rest of the file can be
		// read against them.
		{"the true degrees", `MATCH (a:H) RETURN COUNT { (a)-->() } AS c ORDER BY c`,
			[]string{"0", "3", "3"}},

		// EXISTS caps at 1 — the tightest cap there is, and the one a slot-charged
		// cap would answer wrongly for "h", whose first five slots are dead.
		{"EXISTS sees past the leading tombstones",
			`MATCH (a:H) RETURN EXISTS { (a)-->() } AS e ORDER BY e`,
			[]string{"false", "true", "true"}},

		// A comparison against a literal caps at literal+1. Each of the six
		// operators is exercised, because the cap the evaluator derives differs per
		// operator and each has to stay sufficient.
		{"> 0", `MATCH (a:H) WHERE COUNT { (a)-->() } > 0 RETURN count(a) AS n`, []string{"2"}},
		{"> 2", `MATCH (a:H) WHERE COUNT { (a)-->() } > 2 RETURN count(a) AS n`, []string{"2"}},
		{"> 3", `MATCH (a:H) WHERE COUNT { (a)-->() } > 3 RETURN count(a) AS n`, []string{"0"}},
		{">= 3", `MATCH (a:H) WHERE COUNT { (a)-->() } >= 3 RETURN count(a) AS n`, []string{"2"}},
		{"= 3", `MATCH (a:H) WHERE COUNT { (a)-->() } = 3 RETURN count(a) AS n`, []string{"2"}},
		{"= 0", `MATCH (a:H) WHERE COUNT { (a)-->() } = 0 RETURN count(a) AS n`, []string{"1"}},
		{"<> 3", `MATCH (a:H) WHERE COUNT { (a)-->() } <> 3 RETURN count(a) AS n`, []string{"1"}},
		{"< 1", `MATCH (a:H) WHERE COUNT { (a)-->() } < 1 RETURN count(a) AS n`, []string{"1"}},
		{"<= 0", `MATCH (a:H) WHERE COUNT { (a)-->() } <= 0 RETURN count(a) AS n`, []string{"1"}},

		// The TYPED walk over the same fixture. It already honoured its cap, so
		// these are a control: they must not move.
		{"typed EXISTS", `MATCH (a:H) RETURN EXISTS { (a)-[:K]->() } AS e ORDER BY e`,
			[]string{"false", "true", "true"}},
		{"typed count", `MATCH (a:H) RETURN COUNT { (a)-[:K]->() } AS c ORDER BY c`,
			[]string{"0", "3", "3"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inljRun(t, eng, tc.q, nil)
			want := make([]string, 0, len(tc.want))
			for _, w := range tc.want {
				want = append(want, w+"\x1f")
			}
			if len(got) != len(want) {
				t.Fatalf("%q returned %d rows, want %d\n  got  %q\n  want %q",
					tc.q, len(got), len(want), got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%q row %d: got %q, hand-counted answer is %q (full: %q)",
						tc.q, i, got[i], want[i], got)
				}
			}
		})
	}
}

// TestDegreeRewrite_CapAgreesWithUncappedAcrossShapes sweeps the fixture's shape
// so a cap that were charged wrongly could not hide behind one lucky layout.
//
// For every (dead, live) pair the uncapped count and the EXISTS answer must both
// follow from `live` alone: the tombstoned prefix is invisible to the answer, and
// only its COST was ever supposed to change.
func TestDegreeRewrite_CapAgreesWithUncappedAcrossShapes(t *testing.T) {
	for _, dead := range []int{0, 1, 6} {
		for _, live := range []int{0, 1, 4} {
			t.Run(fmt.Sprintf("dead=%d/live=%d", dead, live), func(t *testing.T) {
				eng := NewEngine(degreeCapFixture(t, dead, live))

				gotCount := inljRun(t, eng, `MATCH (a:H) WHERE a.name = 'h' `+
					`RETURN COUNT { (a)-->() } AS c`, nil)
				if len(gotCount) != 1 {
					t.Fatalf("expected one row for the hub, got %d: %q", len(gotCount), gotCount)
				}
				if want := fmt.Sprintf("%d\x1f", live); gotCount[0] != want {
					t.Fatalf("hub count = %q, want %q (the %d tombstoned slots must not be counted)",
						gotCount[0], want, dead)
				}

				gotExists := inljRun(t, eng, `MATCH (a:H) WHERE a.name = 'h' `+
					`RETURN EXISTS { (a)-->() } AS e`, nil)
				if len(gotExists) != 1 {
					t.Fatalf("expected one row for the hub, got %d: %q", len(gotExists), gotExists)
				}
				want := "false\x1f"
				if live > 0 {
					want = "true\x1f"
				}
				if gotExists[0] != want {
					t.Fatalf("hub EXISTS = %q, want %q with %d live edges behind %d tombstoned ones",
						gotExists[0], want, live, dead)
				}
			})
		}
	}
}

// TestDegreeRewrite_WhichShapesReachTheDegreePath asserts, from the rewrite's own
// counter, WHICH query shapes are answered from the adjacency degree.
//
// It exists because rmp #2265's benchmark first measured
// `WHERE EXISTS { (a)-->() }` and saw no cliff at all. That shape is claimed by
// the plan-level SemiApply rewrite before the expression evaluator ever sees it,
// so it never reaches the degree path — the benchmark was measuring a different
// operator and would have reported the defect as absent. Recording the routing
// here keeps that mistake from being made twice.
func TestDegreeRewrite_WhichShapesReachTheDegreePath(t *testing.T) {
	cases := []struct {
		name        string
		q           string
		wantRewrite bool
	}{
		{"EXISTS in a RETURN projection",
			`MATCH (a:H) RETURN EXISTS { (a)-->() } AS e`, true},
		{"COUNT in a RETURN projection",
			`MATCH (a:H) RETURN COUNT { (a)-->() } AS c`, true},
		{"COUNT compared against a literal in WHERE",
			`MATCH (a:H) WHERE COUNT { (a)-->() } > 2 RETURN count(a)`, true},
		// Claimed by SemiApply before the degree rewrite is consulted.
		{"EXISTS as a bare WHERE predicate",
			`MATCH (a:H) WHERE EXISTS { (a)-->() } RETURN count(a)`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := NewEngine(degreeCapFixture(t, 2, 2))
			before := degreeRewriteCount.Load()
			res, err := eng.Run(context.Background(), tc.q, nil)
			if err != nil {
				t.Fatalf("run %q: %v", tc.q, err)
			}
			for res.Next() { // intentional full drain
			}
			if err := res.Err(); err != nil {
				t.Fatalf("drain %q: %v", tc.q, err)
			}
			if err := res.Close(); err != nil {
				t.Fatalf("close %q: %v", tc.q, err)
			}
			if got := degreeRewriteCount.Load() != before; got != tc.wantRewrite {
				t.Fatalf("the degree rewrite fired = %v, want %v, for %q",
					got, tc.wantRewrite, tc.q)
			}
		})
	}
}

// TestDegreeCeiling pins the cap-to-limit conversion, including the clamp that
// keeps a cap wider than an int from wrapping.
func TestDegreeCeiling(t *testing.T) {
	cases := []struct {
		name  string
		limit int64
		want  int
	}{
		{"no cap requested", -1, maxDegreeLimit},
		{"a very negative cap is still no cap", -1 << 40, maxDegreeLimit},
		{"zero", 0, 0},
		{"one", 1, 1},
		{"an ordinary cap", 4096, 4096},
		{"the widest int cap", int64(maxDegreeLimit), maxDegreeLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := degreeCeiling(tc.limit); got != tc.want {
				t.Fatalf("degreeCeiling(%d) = %d, want %d", tc.limit, got, tc.want)
			}
		})
	}
}
