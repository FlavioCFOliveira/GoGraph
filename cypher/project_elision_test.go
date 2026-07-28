package cypher

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countOperator reports how many nodes of the rendered plan name the given
// operator. The rendering puts one operator per line, so counting lines whose
// operator name matches is exact — and it deliberately matches on a word
// boundary so "Project" does not also count "ColumnarProject".
func countOperator(plan, name string) int {
	n := 0
	for _, line := range strings.Split(plan, "\n") {
		// Strip the tree drawing prefix and any trailing detail annotation.
		trimmed := strings.TrimLeft(line, " │└├─\t")
		if trimmed == "" {
			continue
		}
		if word, _, _ := strings.Cut(trimmed, " "); word == name {
			n++
		}
	}
	return n
}

// TestProjectElision_SingleReturnRendersOneProject is the regression gate for
// rmp #2239: the physical build used to lay its final column passthrough on top
// of the projection it had just built for the ir.Project node, so a single
// RETURN produced TWO Project operators where one does the work.
//
// Each case below rendered two before the fix and renders one after it, so the
// test fails on the old behaviour. The shapes cover the elision firing directly
// (RETURN over a scan, a filter, an expand, an aggregate, an UNWIND) and firing
// through the row-shape-preserving operators the walk descends past.
func TestProjectElision_SingleReturnRendersOneProject(t *testing.T) {
	t.Parallel()
	eng := seedPeople(t, 40, 5)

	cases := []struct {
		query string
		// operator is the projection kind the plan should carry exactly one of.
		operator string
	}{
		{"MATCH (n:P) RETURN n", "Project"},
		{"MATCH (n:P) WHERE n.age > 1 RETURN n", "Project"},
		{"MATCH (n:P) RETURN count(*)", "Project"},
		{"MATCH (a:P)-[r]->(b:P) RETURN a, b", "Project"},
		{"UNWIND [1,2,3] AS x RETURN x", "Project"},
		// Through the shape-preserving walk: Sort, Limit, Skip and Distinct all
		// re-emit their input row unchanged, so the projection beneath them
		// already produces the result columns.
		{"MATCH (n:P) RETURN n ORDER BY n.age", "Project"},
		{"MATCH (n:P) RETURN n LIMIT 3", "Project"},
		{"MATCH (n:P) RETURN n SKIP 2", "Project"},
		{"MATCH (n:P) RETURN n ORDER BY n.age SKIP 1 LIMIT 3", "Project"},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			plan := explainOK(t, eng, tc.query)
			if got := countOperator(plan, tc.operator); got != 1 {
				t.Errorf("want exactly one %s for a single RETURN, got %d:\n%s",
					tc.operator, got, plan)
			}
		})
	}
}

// TestProjectElision_KeepsPassthroughWhenItWouldWiden is the adversarial half,
// and the case that decides whether the elision is SAFE rather than merely
// smaller.
//
// A variable-to-index schema records where a column SITS, not how WIDE the row
// is. `MATCH (a)-[r]->(b:P) RETURN a, r` maps a to index 0 and r to index 1, so
// it satisfies isIdentityPassthrough — yet the row arriving from the expand also
// carries b. Eliding on that evidence alone would emit a third column that the
// query never asked for. exec.EmitsExactly consults the child's declared ARITY
// instead, so the passthrough survives here.
func TestProjectElision_KeepsPassthroughWhenItWouldWiden(t *testing.T) {
	t.Parallel()
	eng := seedRelated(t, 12)

	const q = "MATCH (a:P)-[r:KNOWS]->(b:P) RETURN a, r"
	cols, rows := runCollect(t, eng, q)

	if want := []string{"a", "r"}; !slices.Equal(cols, want) {
		t.Fatalf("result columns = %v, want %v — the passthrough was elided over a "+
			"wider row and leaked a column the query did not project", cols, want)
	}
	if len(rows) == 0 {
		t.Fatal("fixture produced no rows, so this case no longer proves anything")
	}
	for i, rec := range rows {
		if len(rec) != 2 {
			t.Fatalf("row %d has %d columns %v, want 2", i, len(rec), keysOf(rec))
		}
		if _, leaked := rec["b"]; leaked {
			t.Fatalf("row %d leaked the unprojected column b: %v", i, keysOf(rec))
		}
	}
}

// TestProjectElision_ResultsAreUnchanged is the absolute oracle required by
// acceptance criterion 2. It does not compare one build against another — both
// arms would share any defect in the elision — but against column names, column
// order and values written out by hand.
func TestProjectElision_ResultsAreUnchanged(t *testing.T) {
	t.Parallel()
	eng := seedRelated(t, 4)

	t.Run("column order follows the RETURN, not the schema", func(t *testing.T) {
		t.Parallel()
		// b is bound AFTER a and r by the expand, so a RETURN that names it first
		// must reorder — which is precisely what the passthrough exists to do and
		// what the elision must not skip.
		cols, rows := runCollect(t, eng, "MATCH (a:P)-[r:KNOWS]->(b:P) RETURN b, a")
		if want := []string{"b", "a"}; !slices.Equal(cols, want) {
			t.Errorf("columns = %v, want %v", cols, want)
		}
		for i, rec := range rows {
			if len(rec) != 2 {
				t.Errorf("row %d: %d columns, want 2 (%v)", i, len(rec), keysOf(rec))
			}
		}
	})

	t.Run("scalar values survive the elision", func(t *testing.T) {
		t.Parallel()
		cols, rows := runCollect(t, eng, "MATCH (n:P) RETURN n.name AS nm ORDER BY nm")
		if want := []string{"nm"}; !slices.Equal(cols, want) {
			t.Fatalf("columns = %v, want %v", cols, want)
		}
		got := make([]string, 0, len(rows))
		for _, rec := range rows {
			got = append(got, fmt.Sprint(rec["nm"]))
		}
		want := []string{`"p0"`, `"p1"`, `"p2"`, `"p3"`}
		if !slices.Equal(got, want) {
			t.Errorf("values = %v, want %v", got, want)
		}
	})

	t.Run("an aggregate still reports its alias", func(t *testing.T) {
		t.Parallel()
		cols, rows := runCollect(t, eng, "MATCH (n:P) RETURN count(*) AS total")
		if want := []string{"total"}; !slices.Equal(cols, want) {
			t.Fatalf("columns = %v, want %v", cols, want)
		}
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
		if got := fmt.Sprint(rows[0]["total"]); got != "4" {
			t.Errorf("count = %s, want 4", got)
		}
	})
}

// --- fixtures and helpers ---------------------------------------------------

// seedRelated builds n :P nodes named p0..p(n-1) in a KNOWS ring, so an expand
// binds three variables (a, r, b) and the RETURN can project a strict subset of
// them in a different order.
func seedRelated(t *testing.T, n int) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	for i := 0; i < n; i++ {
		mustRun(t, eng, fmt.Sprintf("CREATE (:P {name: 'p%d', age: %d})", i, i))
	}
	for i := 0; i < n; i++ {
		mustRun(t, eng, fmt.Sprintf(
			"MATCH (a:P {name: 'p%d'}), (b:P {name: 'p%d'}) CREATE (a)-[:KNOWS]->(b)",
			i, (i+1)%n))
	}
	return eng
}

// runCollect executes q and returns its declared columns and every record.
func runCollect(t *testing.T, eng *Engine, q string) ([]string, []map[string]any) {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	cols := append([]string(nil), res.Columns()...)
	var rows []map[string]any
	for res.Next() {
		rec := make(map[string]any, len(cols))
		for k, v := range res.Record() {
			rec[k] = v
		}
		rows = append(rows, rec)
	}
	if rerr := res.Err(); rerr != nil {
		t.Fatalf("Run(%q) drain: %v", q, rerr)
	}
	if cerr := res.Close(); cerr != nil {
		t.Fatalf("Run(%q) close: %v", q, cerr)
	}
	return cols, rows
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
