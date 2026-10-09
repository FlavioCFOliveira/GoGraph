package cypher_test

// subquery_union_refused_test.go — a UNION inside an EXISTS or COUNT subquery
// body is ANSWERED FROM EVERY BRANCH (rmp #2627), where it used to be refused
// (rmp #2615) and, before that, answered from its first branch alone.
//
// # The defect the refusal replaced, and this support replaces in turn
//
// The grammar admits `regularQuery` in both positions so a UNION parses, but
// ast.ExistsSubquery.Query and ast.CountSubquery.Query were typed
// *ast.SingleQuery and could not hold one. The visitor kept the first branch and
// discarded the rest with no error and no notification. MEASURED then, on a node
// with a :W edge and no :Z edge:
//
//	EXISTS { MATCH (x)-[:Z]->() RETURN 1 UNION MATCH (x)-[:W]->() RETURN 1 }
//	  returned false, where the second branch matches
//
// rmp #2615 refused the shape; rmp #2627 widens both fields to ast.Query and
// answers it, as Neo4j and Memgraph do (grammar citations in
// cypher/parser/visitor.go, subqueryBody).
//
// # Why the branches return DIFFERENT values
//
// `COUNT { … RETURN 1 UNION … RETURN 1 }` cannot tell UNION from UNION ALL, nor a
// dropped branch from a kept one: UNION de-duplicates, both branches yield the
// row `1`, and the answer is 1 either way. The cases below use branches whose
// rows differ, and branches whose rows partly overlap, so each mode and each
// dropped branch changes the count.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// unionSubqueryFixture seeds three :N nodes:
//
//	a {id:'a', k:0}   a-[:W]->b, a-[:W]->c, a-[:Y]->b      (no :Z edge anywhere)
//	b {id:'b', v:1}
//	c {id:'c', v:2}
//
// so, from a, the :W neighbours carry v ∈ {1, 2}, the :Y neighbour carries
// v = 1, and the :Z branch is empty. `a.k` is 0, so `1 / x.k` raises
// "/ by zero" when — and only when — a row reaching it is evaluated.
func unionSubqueryFixture(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for _, q := range []string{
		"CREATE (:N {id:'a', k:0})",
		"CREATE (:N {id:'b', v:1})",
		"CREATE (:N {id:'c', v:2})",
		"MATCH (a:N {id:'a'}),(b:N {id:'b'}) CREATE (a)-[:W]->(b)",
		"MATCH (a:N {id:'a'}),(c:N {id:'c'}) CREATE (a)-[:W]->(c)",
		"MATCH (a:N {id:'a'}),(b:N {id:'b'}) CREATE (a)-[:Y]->(b)",
	} {
		if _, err := eng.RunInTx(ctx, q, nil); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	return eng
}

// unionSubqueryRows runs q and returns every row's first column rendered as a string,
// or the error the run raised, whether at Run or while the rows were pulled.
func unionSubqueryRows(t *testing.T, eng *cypher.Engine, q string, params map[string]expr.Value) ([]string, error) {
	t.Helper()
	res, err := eng.Run(context.Background(), q, params)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Close() }()
	var rows []string
	for res.Next() {
		parts := make([]string, 0, 2)
		for i := range res.Columns() {
			parts = append(parts, res.ValueAt(i).String())
		}
		rows = append(rows, strings.Join(parts, "|"))
	}
	if err := res.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

// TestSubqueryUnionIsRefused was the rmp #2615 refusal gate. Since rmp #2627 it
// asserts the opposite: every query it used to require refused is ANSWERED, and
// answered from every branch, against hand-computed expectations from the
// fixture above. Each case names the answer a dropped branch would give, so a
// regression to first-branch-only reads as exactly that.
func TestSubqueryUnionIsRefused(t *testing.T) {
	eng := unionSubqueryFixture(t)

	const anchor = "MATCH (x:N {id:'a'}) RETURN "
	for _, tc := range []struct {
		name  string
		query string
		want  string
		// firstBranchOnly is what answering from the first branch alone gives.
		firstBranchOnly string
	}{
		{
			name: "EXISTS: only the second branch matches",
			query: anchor + "EXISTS { MATCH (x)-[:Z]->() RETURN 1 AS v " +
				"UNION MATCH (x)-[:W]->() RETURN 1 AS v } AS c",
			want: "true", firstBranchOnly: "false",
		},
		{
			name: "EXISTS: no branch matches",
			query: anchor + "EXISTS { MATCH (x)-[:Z]->() RETURN 1 AS v " +
				"UNION MATCH (x)<-[:W]-() RETURN 1 AS v } AS c",
			want: "false", firstBranchOnly: "false",
		},
		{
			name: "EXISTS: three branches, only the third matches",
			query: anchor + "EXISTS { MATCH (x)-[:Z]->() RETURN 1 AS v " +
				"UNION ALL MATCH (x)<-[:Y]-() RETURN 1 AS v " +
				"UNION ALL MATCH (x)-[:Y]->() RETURN 1 AS v } AS c",
			want: "true", firstBranchOnly: "false",
		},
		{
			name: "COUNT UNION: disjoint constant rows from two matching branches",
			query: anchor + "COUNT { MATCH (x)-[:Y]->() RETURN 1 AS v " +
				"UNION MATCH (x)-[:W]->() RETURN 2 AS v } AS c",
			// Y yields {1}; W yields {2, 2}, de-duplicated to {2}. {1} ∪ {2} = 2 rows.
			want: "2", firstBranchOnly: "1",
		},
		{
			name: "COUNT UNION: overlapping rows are de-duplicated ACROSS branches",
			query: anchor + "COUNT { MATCH (x)-[:W]->(m) RETURN m.v AS v " +
				"UNION MATCH (x)-[:Y]->(m) RETURN m.v AS v } AS c",
			// W yields {1, 2}; Y yields {1}. {1, 2} ∪ {1} = {1, 2}.
			want: "2", firstBranchOnly: "2",
		},
		{
			name: "COUNT UNION ALL: the same overlap is kept",
			query: anchor + "COUNT { MATCH (x)-[:W]->(m) RETURN m.v AS v " +
				"UNION ALL MATCH (x)-[:Y]->(m) RETURN m.v AS v } AS c",
			// 2 rows from W plus 1 row from Y.
			want: "3", firstBranchOnly: "2",
		},
		{
			name: "COUNT UNION: de-duplication within one branch",
			query: anchor + "COUNT { MATCH (x)-[:W]->() RETURN 7 AS v " +
				"UNION MATCH (x)-[:Z]->() RETURN 7 AS v } AS c",
			// W yields {7, 7}; UNION de-duplicates it to one row.
			want: "1", firstBranchOnly: "1",
		},
		{
			name: "COUNT UNION ALL: duplicates within one branch are kept",
			query: anchor + "COUNT { MATCH (x)-[:W]->() RETURN 7 AS v " +
				"UNION ALL MATCH (x)-[:Z]->() RETURN 7 AS v } AS c",
			want: "2", firstBranchOnly: "2",
		},
		{
			name: "COUNT UNION: three branches",
			query: anchor + "COUNT { MATCH (x)-[:Z]->(m) RETURN m.v AS v " +
				"UNION MATCH (x)-[:Y]->(m) RETURN m.v AS v " +
				"UNION MATCH (x)-[:W]->(m) RETURN m.v AS v } AS c",
			// {} ∪ {1} ∪ {1, 2} = {1, 2}.
			want: "2", firstBranchOnly: "0",
		},
		{
			name: "COUNT UNION ALL: three branches",
			query: anchor + "COUNT { MATCH (x)-[:Z]->(m) RETURN m.v AS v " +
				"UNION ALL MATCH (x)-[:Y]->(m) RETURN m.v AS v " +
				"UNION ALL MATCH (x)-[:W]->(m) RETURN m.v AS v } AS c",
			// 0 + 1 + 2.
			want: "3", firstBranchOnly: "0",
		},
		{
			name: "COUNT UNION: multi-column rows compare on every column",
			query: anchor + "COUNT { MATCH (x)-[:W]->(m) RETURN m.v AS v, 'w' AS t " +
				"UNION MATCH (x)-[:Y]->(m) RETURN m.v AS v, 'y' AS t } AS c",
			// (1,w), (2,w), (1,y): three distinct rows, because t differs.
			want: "3", firstBranchOnly: "2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := unionSubqueryRows(t, eng, tc.query, nil)
			if err != nil {
				t.Fatalf("a UNION subquery body was refused or failed: %v (#2627)", err)
			}
			if len(rows) != 1 {
				t.Fatalf("got %d rows %v, want exactly one", len(rows), rows)
			}
			if rows[0] != tc.want {
				t.Errorf("got %s, want %s (answering from the first branch alone gives %s)",
					rows[0], tc.want, tc.firstBranchOnly)
			}
		})
	}
}

// TestSubqueryUnion_CorrelatesPerOuterRow drives the same UNION body for every
// outer row, in both expression positions — a projection and a WHERE — and both
// the bounded COUNT comparison and NOT EXISTS. A UNION body in a WHERE is
// routed to the expression evaluator rather than a SemiApply (cypher/ir/exists.go),
// so this is the case that proves that route correlates.
func TestSubqueryUnion_CorrelatesPerOuterRow(t *testing.T) {
	eng := unionSubqueryFixture(t)

	const union = "MATCH (x)-[:W]->(m) RETURN m.v AS v UNION MATCH (x)<-[:W]-(m) RETURN m.k AS v"
	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{
			// a: W-out {1, 2}, W-in none → 2. b and c: W-out none, W-in from a → {0} → 1.
			name:  "COUNT per outer row",
			query: "MATCH (x:N) RETURN x.id, COUNT { " + union + " } AS c ORDER BY x.id",
			want:  []string{`"a"|2`, `"b"|1`, `"c"|1`},
		},
		{
			name:  "EXISTS in WHERE",
			query: "MATCH (x:N) WHERE EXISTS { MATCH (x)-[:Y]->() RETURN 1 AS v UNION MATCH (x)<-[:Y]-() RETURN 1 AS v } RETURN x.id ORDER BY x.id",
			want:  []string{`"a"`, `"b"`},
		},
		{
			name:  "NOT EXISTS in WHERE",
			query: "MATCH (x:N) WHERE NOT EXISTS { MATCH (x)-[:Y]->() RETURN 1 AS v UNION MATCH (x)<-[:Y]-() RETURN 1 AS v } RETURN x.id ORDER BY x.id",
			want:  []string{`"c"`},
		},
		{
			name:  "bounded COUNT comparison in WHERE",
			query: "MATCH (x:N) WHERE COUNT { " + union + " } > 1 RETURN x.id ORDER BY x.id",
			want:  []string{`"a"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := unionSubqueryRows(t, eng, tc.query, nil)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if strings.Join(rows, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", rows, tc.want)
			}
		})
	}
}

// TestSubqueryUnion_LiteralsAndParametersInALaterBranch pins that a later
// branch is planned with the enclosing query's parameter map: a `$p` and a
// string literal (which the plan cache hoists onto an auto-parameter) written
// in the SECOND branch must resolve, or the branch silently contributes nothing.
func TestSubqueryUnion_LiteralsAndParametersInALaterBranch(t *testing.T) {
	eng := unionSubqueryFixture(t)
	for _, tc := range []struct {
		name   string
		query  string
		params map[string]expr.Value
		want   string
	}{
		{
			name: "parameter",
			query: "MATCH (x:N {id:'a'}) RETURN COUNT { MATCH (x)-[:Z]->() RETURN 0 AS v " +
				"UNION MATCH (x)-[:W]->(m) WHERE m.v = $p RETURN m.v AS v } AS c",
			params: map[string]expr.Value{"p": expr.IntegerValue(2)},
			want:   "1",
		},
		{
			name: "string literal",
			query: "MATCH (x:N {id:'a'}) RETURN COUNT { MATCH (x)-[:Z]->() RETURN 0 AS v " +
				"UNION MATCH (x)-[:W]->(m) WHERE m.id = 'c' RETURN m.v AS v } AS c",
			want: "1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := unionSubqueryRows(t, eng, tc.query, tc.params)
			if err != nil {
				t.Fatalf("%v", err)
			}
			if len(rows) != 1 || rows[0] != tc.want {
				t.Errorf("got %v, want [%s]", rows, tc.want)
			}
		})
	}
}

// TestSubqueryUnion_ExistsShortCircuits asserts STRUCTURALLY that EXISTS stops
// at the first branch that yields a row: the branch after it would raise
// "/ by zero" if any of its rows were evaluated, so a true answer with no error
// is only possible if that branch never ran.
//
// The two controls make the assertion non-vacuous. COUNT over the identical
// body must raise the error — proving the erroring branch DOES error when it is
// driven — and an EXISTS whose matching branch comes AFTER the erroring one
// must raise it too — proving branches are driven in document order and that an
// empty branch does not end the walk.
func TestSubqueryUnion_ExistsShortCircuits(t *testing.T) {
	eng := unionSubqueryFixture(t)
	const anchor = "MATCH (x:N {id:'a'}) RETURN "
	const erroring = "MATCH (x)-[:W]->() RETURN 1 / x.k AS v"
	const matching = "MATCH (x)-[:Y]->() RETURN 1 AS v"

	for _, mode := range []string{"UNION", "UNION ALL"} {
		t.Run(mode, func(t *testing.T) {
			// The first branch matches; the second must never be evaluated.
			rows, err := unionSubqueryRows(t, eng, anchor+"EXISTS { "+matching+" "+mode+" "+erroring+" } AS c", nil)
			if err != nil {
				t.Fatalf("EXISTS evaluated the branch AFTER the one that matched: %v", err)
			}
			if len(rows) != 1 || rows[0] != "true" {
				t.Fatalf("got %v, want [true]", rows)
			}

			// Three branches: an empty one, a matching one, then the erroring one.
			rows, err = unionSubqueryRows(t, eng, anchor+"EXISTS { MATCH (x)-[:Z]->() RETURN 1 AS v "+mode+" "+
				matching+" "+mode+" "+erroring+" } AS c", nil)
			if err != nil {
				t.Fatalf("EXISTS evaluated the third branch after the second matched: %v", err)
			}
			if len(rows) != 1 || rows[0] != "true" {
				t.Fatalf("got %v, want [true]", rows)
			}

			// Control 1: COUNT drives every branch, so it must hit the error.
			if _, err := unionSubqueryRows(t, eng, anchor+"COUNT { "+matching+" "+mode+" "+erroring+" } AS c", nil); err == nil ||
				!strings.Contains(err.Error(), "by zero") {
				t.Fatalf("COUNT over the same body did not raise the division error (%v), so the "+
					"short-circuit assertion above proves nothing", err)
			}

			// Control 2: with the erroring branch FIRST, EXISTS must reach it.
			if _, err := unionSubqueryRows(t, eng, anchor+"EXISTS { "+erroring+" "+mode+" "+matching+" } AS c", nil); err == nil ||
				!strings.Contains(err.Error(), "by zero") {
				t.Fatalf("EXISTS with the erroring branch first did not raise the division error (%v)", err)
			}
		})
	}
}

// TestSubqueryUnion_ColumnCompatibilityIsEnforced pins that the UNION rules of a
// top-level query hold inside a subquery body, as they do in Neo4j (sources in
// cypher/parser/visitor.go, subqueryBody): differing columns are a compile-time
// DifferentColumnsInUnion, and mixing UNION with UNION ALL is
// InvalidClauseComposition.
func TestSubqueryUnion_ColumnCompatibilityIsEnforced(t *testing.T) {
	eng := unionSubqueryFixture(t)
	const anchor = "MATCH (x:N {id:'a'}) RETURN "
	for _, tc := range []struct {
		name, query, want string
	}{
		{"COUNT, different column names", anchor +
			"COUNT { MATCH (x)-[:W]->() RETURN 1 AS a UNION MATCH (x)-[:Y]->() RETURN 2 AS b } AS c",
			"DifferentColumnsInUnion"},
		{"EXISTS, different column names", anchor +
			"EXISTS { MATCH (x)-[:W]->() RETURN 1 AS a UNION ALL MATCH (x)-[:Y]->() RETURN 2 AS b } AS c",
			"DifferentColumnsInUnion"},
		{"COUNT, different column counts", anchor +
			"COUNT { MATCH (x)-[:W]->() RETURN 1 AS a UNION MATCH (x)-[:Y]->() RETURN 1 AS a, 2 AS b } AS c",
			"DifferentColumnsInUnion"},
		{"three branches, the third differs", anchor +
			"COUNT { MATCH (x)-[:W]->() RETURN 1 AS a UNION MATCH (x)-[:Y]->() RETURN 2 AS a " +
			"UNION MATCH (x)-[:Z]->() RETURN 3 AS z } AS c",
			"DifferentColumnsInUnion"},
		{"mixed UNION and UNION ALL", anchor +
			"COUNT { MATCH (x)-[:W]->() RETURN 1 AS a UNION MATCH (x)-[:Y]->() RETURN 2 AS a " +
			"UNION ALL MATCH (x)-[:Z]->() RETURN 3 AS a } AS c",
			"InvalidClauseComposition"},
		{"an update clause in a later EXISTS branch", anchor +
			"EXISTS { MATCH (x)-[:W]->() RETURN 1 AS a UNION CREATE (y:Q) RETURN 2 AS a } AS c",
			"InvalidClauseComposition"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := unionSubqueryRows(t, eng, tc.query, nil)
			if err == nil {
				t.Fatalf("the query was answered; want a %s error", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name %s: %v", tc.want, err)
			}
		})
	}
	// The rejected update must not have been applied.
	rows, err := unionSubqueryRows(t, eng, "MATCH (q:Q) RETURN count(q) AS n", nil)
	if err != nil || len(rows) != 1 || rows[0] != "0" {
		t.Fatalf("a refused subquery body wrote to the graph: rows=%v err=%v", rows, err)
	}
}

// TestSubqueryWithoutUnionStillWorks is the control: one-branch bodies take the
// path they always took.
func TestSubqueryWithoutUnionStillWorks(t *testing.T) {
	eng := unionSubqueryFixture(t)

	for _, tc := range []struct {
		name  string
		query string
		want  string
	}{
		{"EXISTS matching", "MATCH (x:N {id:'a'}) RETURN EXISTS { MATCH (x)-[:W]->() RETURN 1 } AS c", "true"},
		{"EXISTS not matching", "MATCH (x:N {id:'a'}) RETURN EXISTS { MATCH (x)-[:Z]->() RETURN 1 } AS c", "false"},
		{"COUNT", "MATCH (x:N {id:'a'}) RETURN COUNT { MATCH (x)-[:W]->() RETURN 1 } AS c", "2"},
		{"COUNT DISTINCT body", "MATCH (x:N {id:'a'}) RETURN COUNT { MATCH (x)-[:W]->() RETURN DISTINCT 1 AS v } AS c", "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := unionSubqueryRows(t, eng, tc.query, nil)
			if err != nil {
				t.Fatalf("a subquery WITHOUT a UNION failed: %v", err)
			}
			if len(rows) != 1 || rows[0] != tc.want {
				t.Errorf("got %v, want [%s]", rows, tc.want)
			}
		})
	}
}

// TestSubqueryUnion_ConcurrentFirstExecution runs one cached UNION subquery from
// many goroutines at once, its branches carrying anonymous entities. Every branch
// is translated per execution from the shared, cached AST, so every branch's
// anonymous names must have been minted while the AST was still private (the
// rmp #2508 precondition, extended to every branch by rmp #2627). Under -race a
// branch that wrote a name into the shared AST is a reported race; without it,
// the answers below still have to be right.
func TestSubqueryUnion_ConcurrentFirstExecution(t *testing.T) {
	eng := unionSubqueryFixture(t)
	const q = "MATCH (x:N {id:'a'}) RETURN COUNT { MATCH (x)-[:W]->() RETURN 1 AS v " +
		"UNION ALL MATCH (x)-[:Y]->(:N)-[]-() RETURN 2 AS v } AS c"
	const goroutines = 16
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows, err := unionSubqueryRows(t, eng, q, nil)
			if err != nil {
				errs <- err
				return
			}
			// W from a: 2 rows. a-[:Y]->b then any OTHER edge at b: only a-[:W]->b, so
			// 1 row (the :Y edge itself is excluded by relationship uniqueness). 2 + 1 = 3.
			if len(rows) != 1 || rows[0] != "3" {
				errs <- fmt.Errorf("got %v, want [3]", rows)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
