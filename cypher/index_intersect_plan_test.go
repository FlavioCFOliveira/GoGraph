package cypher

// index_intersect_plan_test.go — plan-shape, differential and absolute-oracle gate
// for conjunctive indexed property predicates composed by bitmap intersection
// (#2134).
//
// Every case is checked three ways, for the reason sprint 311 established: both
// differential arms share the same residual Filter and the same row pipeline, so a
// defect they SHARE is invisible to an ON-versus-OFF comparison. The third check is
// an absolute oracle computed in Go straight from the fixture.
//
// Design and the superset proof: docs/design-bitmap-intersection.md §8.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// iiPop is the :Doc population — above rangeSeekMinLabelPopulation so the shipped
// per-conjunct gate can admit, and large enough that a conjunction is far more
// selective than either conjunct alone.
const iiPop = 20_000

// iiRow mirrors one fixture node, so the oracle is computed from what the test
// itself wrote rather than from anything the engine reports.
type iiRow struct {
	s    string
	a, b int64
}

// iiFixture seeds :Doc nodes with two independently btree-indexed numeric
// properties and one indexed string property. `a` cycles fast and `b` cycles slow,
// so `a < 10 AND b < 30` is satisfied by a small intersection while each conjunct
// alone covers far more of the label.
func iiFixture(t testing.TB) (*lpg.Graph[string, float64], []iiRow) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	rows := make([]iiRow, 0, iiPop)
	for i := 0; i < iiPop; i++ {
		key := fmt.Sprintf("d%06d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Doc"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		r := iiRow{a: int64(i % 1000), b: int64(i / 20), s: fmt.Sprintf("s%06d", i)}
		if err := g.SetNodeProperty(key, "a", lpg.Int64Value(r.a)); err != nil {
			t.Fatalf("SetNodeProperty a: %v", err)
		}
		if err := g.SetNodeProperty(key, "b", lpg.Int64Value(r.b)); err != nil {
			t.Fatalf("SetNodeProperty b: %v", err)
		}
		if err := g.SetNodeProperty(key, "s", lpg.StringValue(r.s)); err != nil {
			t.Fatalf("SetNodeProperty s: %v", err)
		}
		rows = append(rows, r)
	}
	return g, rows
}

// iiEngine builds an engine over g with all three btree indexes, with the
// intersection enabled or disabled.
func iiEngine(t testing.TB, g *lpg.Graph[string, float64], disable bool) *Engine {
	t.Helper()
	eng := NewEngineWithOptions(g, EngineOptions{
		DisableBitmapIntersection: disable,
		MaxResultRows:             MaxResultRowsUnlimited,
	})
	for _, ddl := range []string{
		`CREATE INDEX FOR (n:Doc) ON (n.a) OPTIONS {indexType:'btree'}`,
		`CREATE INDEX FOR (n:Doc) ON (n.b) OPTIONS {indexType:'btree'}`,
		`CREATE INDEX FOR (n:Doc) ON (n.s) OPTIONS {indexType:'btree'}`,
	} {
		if _, err := eng.Run(context.Background(), ddl, nil); err == nil {
			continue
		} else if !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	return eng
}

// iiRun returns the sorted first column of q's result.
func iiRun(t *testing.T, eng *Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run %q: %v", q, err)
	}
	out := make([]string, 0, 16)
	for res.Next() {
		v := res.ValueAt(0)
		if v == nil || expr.IsNull(v) {
			out = append(out, "<null>")
			continue
		}
		out = append(out, v.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iter %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	sort.Strings(out)
	return out
}

// composedMarker is what PlanDetail renders for a composed intersection: a second
// range clause ANDed into the first.
const composedMarker = "∩ range="

// TestIndexIntersect_ComposesTwoIndexes is the core gate for #2134.
func TestIndexIntersect_ComposesTwoIndexes(t *testing.T) {
	t.Parallel()
	g, rows := iiFixture(t)
	on := iiEngine(t, g, false)
	off := iiEngine(t, g, true)

	cases := []struct {
		name        string
		query       string
		wantCompose bool
		keep        func(iiRow) bool
	}{{
		// Two independently indexed numeric properties. Neither conjunct alone is
		// selective enough to be worth much; together they are.
		name:        "two_numeric_properties",
		query:       `MATCH (n:Doc) WHERE n.a < 10 AND n.b < 30 RETURN n.s AS s`,
		wantCompose: true,
		keep:        func(r iiRow) bool { return r.a < 10 && r.b < 30 },
	}, {
		// ACROSS index types: the numeric companion ANDed with a string btree. This
		// is the composite-index equivalent achieved with no composite index type.
		name:        "numeric_and_string",
		query:       `MATCH (n:Doc) WHERE n.a < 10 AND n.s < "s000300" RETURN n.s AS s`,
		wantCompose: true,
		keep:        func(r iiRow) bool { return r.a < 10 && r.s < "s000300" },
	}, {
		// A third conjunct whose own range covers most of the label fails the shipped
		// per-conjunct gate, so it is left to the residual Filter while the other two
		// still compose. Composing selectively is the point: a broad conjunct would
		// cost more to probe and AND than the rows it removes.
		name:        "third_conjunct_too_broad_is_left_to_the_filter",
		query:       `MATCH (n:Doc) WHERE n.a < 10 AND n.b < 30 AND n.s < "s019000" RETURN n.s AS s`,
		wantCompose: true,
		keep:        func(r iiRow) bool { return r.a < 10 && r.b < 30 && r.s < "s019000" },
	}, {
		// A two-sided range on ONE property is not a composition: it is the shipped
		// single-property range seek, and it must stay that way (no regression).
		name:        "single_property_two_sided_is_not_a_composition",
		query:       `MATCH (n:Doc) WHERE n.a > 2 AND n.a < 8 RETURN n.s AS s`,
		wantCompose: false,
		keep:        func(r iiRow) bool { return r.a > 2 && r.a < 8 },
	}, {
		// Only one conjunct is indexed, so there is nothing to intersect with.
		name:        "one_indexed_one_not",
		query:       `MATCH (n:Doc) WHERE n.a < 10 AND n.missing = 1 RETURN n.s AS s`,
		wantCompose: false,
		keep:        func(iiRow) bool { return false },
	}, {
		// A DISJUNCTION must never be composed: the parts are not necessary
		// conditions of the predicate, so intersecting them would drop rows.
		name:        "disjunction_never_composes",
		query:       `MATCH (n:Doc) WHERE n.a < 5 OR n.b < 5 RETURN n.s AS s`,
		wantCompose: false,
		keep:        func(r iiRow) bool { return r.a < 5 || r.b < 5 },
	}, {
		// A negated conjunct likewise: NOT inverts the condition, so its bound is not
		// a necessary condition of the predicate.
		name:        "negated_conjunct_never_composes",
		query:       `MATCH (n:Doc) WHERE NOT n.a < 10 AND n.b < 30 RETURN n.s AS s`,
		wantCompose: false,
		keep:        func(r iiRow) bool { return r.a >= 10 && r.b < 30 },
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			planOn, err := on.Explain(tc.query, nil)
			if err != nil {
				t.Fatalf("Explain on: %v", err)
			}
			composed := strings.Contains(planOn, composedMarker)
			if composed != tc.wantCompose {
				t.Fatalf("composed = %v, want %v\nplan:\n%s", composed, tc.wantCompose, planOn)
			}
			if tc.wantCompose {
				// The residual Filter is MANDATORY for a composed probe: each part is
				// only a superset of its conjunct (#F-EXEC1), so the exact predicate
				// must still be re-applied per surviving row.
				if !strings.Contains(planOn, "Filter") {
					t.Fatalf("composed probe lost its residual Filter — each part is only a superset\nplan:\n%s", planOn)
				}
				// Anti-degeneracy: the disabled arm must take a different plan.
				planOff, perr := off.Explain(tc.query, nil)
				if perr != nil {
					t.Fatalf("Explain off: %v", perr)
				}
				if strings.Contains(planOff, composedMarker) {
					t.Fatalf("disabled arm still composed — the differential is degenerate\nplan:\n%s", planOff)
				}
			}

			// The ABSOLUTE oracle, computed from the fixture the test wrote.
			want := make([]string, 0, 16)
			for _, r := range rows {
				if tc.keep(r) {
					want = append(want, `"`+r.s+`"`)
				}
			}
			sort.Strings(want)

			gotOn := iiRun(t, on, tc.query)
			gotOff := iiRun(t, off, tc.query)
			assertSameStrings(t, "composed vs Go oracle", gotOn, want)
			assertSameStrings(t, "single-index vs Go oracle", gotOff, want)
			assertSameStrings(t, "composed vs single-index", gotOn, gotOff)
		})
	}
}
