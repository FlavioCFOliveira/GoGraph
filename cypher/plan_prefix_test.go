package cypher_test

// plan_prefix_test.go — the EXPLAIN / PROFILE statement prefix (rmp #2721).
//
// The load-bearing test here is TestPlanPrefix_ExplainExecutesNothing. It is
// written with a CONTROL arm for every case: the same statement without the
// prefix is run against a second, identical graph and must change it. Without
// that arm the test would pass just as well on a build where the statement never
// worked at all, which is the failure mode an "assert nothing happened" test
// invites.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newPrefixGraph builds a small labelled graph: n0..n(count-1), each :Person
// with an age property, joined in a chain by :KNOWS.
func newPrefixGraph(t *testing.T, count int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	for i := range count {
		key := fmt.Sprintf("p%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(20+i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for i := 1; i < count; i++ {
		src, dst := fmt.Sprintf("p%d", i-1), fmt.Sprintf("p%d", i)
		if err := g.AddEdge(src, dst, 1); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel(src, dst, "KNOWS")
	}
	return g
}

// drainRows counts the rows of a result and closes it.
func drainRows(t *testing.T, r *cypher.Result) int {
	t.Helper()
	n := 0
	for r.Next() {
		n++
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Result.Err: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Result.Close: %v", err)
	}
	return n
}

// TestPlanPrefix_ExplainExecutesNothing is the safety acceptance test: a
// side-effecting statement prefixed with EXPLAIN must leave the graph exactly as
// it was, and must report no rows.
//
// Each case runs TWICE against two freshly built, identical graphs: once with
// the prefix (the graph must not move) and once without it (the graph MUST
// move). The control arm is what makes the first arm mean something.
func TestPlanPrefix_ExplainExecutesNothing(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		stmt string
	}{
		{"create", "CREATE (:Person {age: 99})"},
		{"detach_delete", "MATCH (n:Person) DETACH DELETE n"},
		{"set", "MATCH (n:Person) SET n.age = 1"},
		{"merge", "MERGE (:Person {age: 999})"},
		{"remove_label", "MATCH (n:Person) REMOVE n:Person"},
		{"create_with_return", "CREATE (m:Person {age: 77}) RETURN m"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()

			// ── Arm A: with the prefix. Nothing may change. ──────────────────
			gA := newPrefixGraph(t, 4)
			engA := cypher.NewEngine(gA)
			t.Cleanup(func() { _ = engA.Close() })
			beforeA := graphFingerprint(ctx, t, engA)

			rA, err := engA.RunAny(ctx, "EXPLAIN "+c.stmt, nil)
			if err != nil {
				t.Fatalf("RunAny(EXPLAIN %s): %v", c.stmt, err)
			}
			if p := rA.Plan(); p == nil {
				t.Error("Result.Plan() is nil for an EXPLAIN statement")
			}
			if p := rA.Profile(); p != nil {
				t.Error("Result.Profile() is non-nil for an EXPLAIN statement")
			}
			if n := drainRows(t, rA); n != 0 {
				t.Errorf("EXPLAIN produced %d rows, want 0", n)
			}
			afterA := graphFingerprint(ctx, t, engA)
			if beforeA != afterA {
				t.Errorf("EXPLAIN %s mutated the graph: %s -> %s", c.stmt, beforeA, afterA)
			}

			// ── Arm B (control): without the prefix. Something MUST change. ──
			gB := newPrefixGraph(t, 4)
			engB := cypher.NewEngine(gB)
			t.Cleanup(func() { _ = engB.Close() })
			beforeB := graphFingerprint(ctx, t, engB)

			rB, err := engB.RunAny(ctx, c.stmt, nil)
			if err != nil {
				t.Fatalf("RunAny(%s): %v", c.stmt, err)
			}
			_ = drainRows(t, rB)
			afterB := graphFingerprint(ctx, t, engB)
			if beforeB == afterB {
				t.Fatalf("control arm did not move the graph for %q (fingerprint %s); "+
					"the EXPLAIN assertion above proves nothing", c.stmt, beforeB)
			}
			if beforeA != beforeB {
				t.Fatalf("the two arms started from different graphs: %s vs %s", beforeA, beforeB)
			}
		})
	}
}

// graphFingerprint summarises the observable graph state: node count, edge
// count, and the multiset of (label, age) pairs. It is deliberately coarse — it
// only has to distinguish "changed" from "unchanged".
func graphFingerprint(ctx context.Context, t *testing.T, eng *cypher.Engine) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []string{
		"MATCH (n) RETURN count(n) AS c",
		"MATCH ()-[r]->() RETURN count(r) AS c",
		"MATCH (n:Person) RETURN count(n) AS c",
		"MATCH (n) WHERE n.age IS NOT NULL RETURN sum(n.age) AS c",
	} {
		r, err := eng.Run(ctx, q, nil)
		if err != nil {
			t.Fatalf("fingerprint %q: %v", q, err)
		}
		for r.Next() {
			fmt.Fprintf(&b, "%v|", r.ValueAt(0))
		}
		if err := r.Err(); err != nil {
			t.Fatalf("fingerprint %q: %v", q, err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("fingerprint close %q: %v", q, err)
		}
	}
	return b.String()
}

// TestPlanPrefix_ExplainMatchesEngineExplain pins the prefix to the Go API: the
// captured tree, rendered, must be byte-identical to what Engine.Explain prints
// for the same statement. That is what makes "no third rendering path" testable
// rather than merely asserted.
func TestPlanPrefix_ExplainMatchesEngineExplain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newPrefixGraph(t, 6)
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	for _, q := range []string{
		"MATCH (n) RETURN n",
		"MATCH (n:Person) RETURN n",
		"MATCH (n:Person) WHERE n.age > 21 RETURN n.age AS a ORDER BY a",
		"MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a, b",
		"MATCH (n:Person) RETURN count(n) AS c",
	} {
		t.Run(q, func(t *testing.T) {
			want, err := eng.Explain(q, nil)
			if err != nil {
				t.Fatalf("Engine.Explain: %v", err)
			}
			r, err := eng.Run(ctx, "EXPLAIN "+q, nil)
			if err != nil {
				t.Fatalf("Run(EXPLAIN): %v", err)
			}
			defer func() { _ = r.Close() }()
			node := r.Plan()
			if node == nil {
				t.Fatal("Result.Plan() is nil")
			}
			if got := exec.RenderPlanNode(node); got != want {
				t.Errorf("rendered plan differs\n got: %q\nwant: %q", got, want)
			}
		})
	}
}

// TestPlanPrefix_ExplainReportsQueryColumns holds the result SHAPE: an EXPLAIN
// returns the statement's OWN column signature with zero rows, as Neo4j does,
// rather than a synthetic one-column rendering of the plan.
func TestPlanPrefix_ExplainReportsQueryColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newPrefixGraph(t, 3)
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	cases := []struct {
		stmt string
		cols []string
	}{
		{"MATCH (n:Person) RETURN n", []string{"n"}},
		{"MATCH (n:Person) RETURN n.age AS age, n AS who", []string{"age", "who"}},
		{"MATCH (n:Person) RETURN count(n) AS c", []string{"c"}},
		{"CREATE (m:Person) RETURN m", []string{"m"}},
	}
	for _, c := range cases {
		t.Run(c.stmt, func(t *testing.T) {
			r, err := eng.RunAny(ctx, "EXPLAIN "+c.stmt, nil)
			if err != nil {
				t.Fatalf("RunAny: %v", err)
			}
			got := r.Columns()
			if strings.Join(got, ",") != strings.Join(c.cols, ",") {
				t.Errorf("columns = %v, want %v", got, c.cols)
			}
			if n := drainRows(t, r); n != 0 {
				t.Errorf("rows = %d, want 0", n)
			}
		})
	}
}

// TestPlanPrefix_ProfileExecutesAndReturnsRows is the other half of the pair:
// PROFILE must EXECUTE. It asserts the rows are the query's real rows (equal to
// the un-prefixed run) and that the captured plan carries measurements.
func TestPlanPrefix_ProfileExecutesAndReturnsRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newPrefixGraph(t, 7)
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	const q = "MATCH (n:Person) WHERE n.age > 21 RETURN n.age AS age ORDER BY age"

	plain, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var want []string
	for plain.Next() {
		want = append(want, plain.ValueAt(0).String())
	}
	if err := plain.Err(); err != nil {
		t.Fatalf("Run err: %v", err)
	}
	if err := plain.Close(); err != nil {
		t.Fatalf("Run close: %v", err)
	}
	if len(want) == 0 {
		t.Fatal("the un-prefixed query produced no rows; the comparison below would be vacuous")
	}

	prof, err := eng.Run(ctx, "PROFILE "+q, nil)
	if err != nil {
		t.Fatalf("Run(PROFILE): %v", err)
	}
	defer func() { _ = prof.Close() }()
	if strings.Join(prof.Columns(), ",") != "age" {
		t.Errorf("columns = %v, want [age]", prof.Columns())
	}
	var got []string
	for prof.Next() {
		got = append(got, prof.ValueAt(0).String())
	}
	if err := prof.Err(); err != nil {
		t.Fatalf("PROFILE err: %v", err)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("PROFILE rows = %v, want %v", got, want)
	}
	if prof.Plan() != nil {
		t.Error("Result.Plan() is non-nil for a PROFILE statement")
	}
	node := prof.Profile()
	if node == nil {
		t.Fatal("Result.Profile() is nil for a PROFILE statement")
	}
	if !node.Profiled {
		t.Error("root plan node carries no measurements")
	}
	if node.Rows != int64(len(want)) {
		t.Errorf("root Rows = %d, want %d", node.Rows, len(want))
	}
	if total := totalDbHits(node); total == 0 {
		t.Error("no operator reported a db-hit; the profile is not measuring")
	}
}

// totalDbHits sums the db-hits over a captured plan tree.
func totalDbHits(n *exec.PlanNode) int64 {
	total := n.DbHits
	for i := range n.Children {
		total += totalDbHits(&n.Children[i])
	}
	return total
}

// TestPlanPrefix_ProfileRefusesWritingStatement pins the documented limitation:
// a writing statement cannot be profiled, because the profiling wrapper is
// installed by the READ builder. It must be REFUSED, never silently executed
// without a profile.
func TestPlanPrefix_ProfileRefusesWritingStatement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newPrefixGraph(t, 3)
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	before := graphFingerprint(ctx, t, eng)
	r, err := eng.RunAny(ctx, "PROFILE CREATE (:Person {age: 1})", nil)
	if err == nil {
		_ = r.Close()
		t.Fatal("PROFILE of a writing statement was accepted")
	}
	if !strings.Contains(err.Error(), "PROFILE") {
		t.Errorf("error does not name PROFILE: %v", err)
	}
	if after := graphFingerprint(ctx, t, eng); after != before {
		t.Errorf("a refused PROFILE still mutated the graph: %s -> %s", before, after)
	}
}

// TestPlanPrefix_IdentifiersSurviveTokenisation is the regression guard for the
// grammar change itself. Promoting EXPLAIN and PROFILE to lexer tokens would
// have DELETED them from the accepted language wherever an identifier is legal;
// they are listed in `symbol` so that it did not. Every statement here parses,
// plans and runs at the revision before the tokens existed.
func TestPlanPrefix_IdentifiersSurviveTokenisation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := lpg.New[string, float64](adjlist.Config{})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := g.SetNodeLabel("a", "Explain"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if err := g.SetNodeProperty("a", "profile", lpg.Int64Value(7)); err != nil {
		t.Fatalf("SetNodeProperty: %v", err)
	}
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	cases := []struct {
		stmt string
		want string
	}{
		{"MATCH (explain) RETURN count(explain) AS c", "1"},
		{"MATCH (n:Explain) RETURN n.profile AS profile", "7"},
		{"WITH 1 AS explain RETURN explain", "1"},
		{"UNWIND [3] AS profile RETURN profile", "3"},
		{"RETURN {explain: 4}.explain AS x", "4"},
		{"MATCH (n:Explain) RETURN n.profile + 1 AS x", "8"},
		// And the prefix still binds ahead of a statement that uses the words.
		{"EXPLAIN MATCH (explain) RETURN explain", ""},
	}
	for _, c := range cases {
		t.Run(c.stmt, func(t *testing.T) {
			r, err := eng.Run(ctx, c.stmt, nil)
			if err != nil {
				t.Fatalf("Run(%q): %v", c.stmt, err)
			}
			defer func() { _ = r.Close() }()
			var got []string
			for r.Next() {
				got = append(got, r.ValueAt(0).String())
			}
			if err := r.Err(); err != nil {
				t.Fatalf("Err: %v", err)
			}
			if strings.Join(got, ",") != c.want {
				t.Errorf("rows = %q, want %q", strings.Join(got, ","), c.want)
			}
		})
	}
}

// TestPlanPrefix_ExplainCarriesPlanTimeNotifications holds a property that is
// most of the point of EXPLAIN: seeing the planner's advisories WITHOUT running
// the query. The prefix dropped them at first — an EXPLAIN of a disconnected
// Cartesian product reported zero notifications where the un-prefixed run
// reported one — so the un-prefixed run is the control here.
func TestPlanPrefix_ExplainCarriesPlanTimeNotifications(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newPrefixGraph(t, 3)
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	const q = "MATCH (a:Person), (b:Person) RETURN a, b"

	ctrl, err := eng.Run(ctx, q, nil)
	if err != nil {
		t.Fatalf("Run(control): %v", err)
	}
	wantN := len(ctrl.Notifications())
	_ = drainRows(t, ctrl)
	if wantN == 0 {
		t.Fatal("the un-prefixed query produced no notification; the comparison below would be vacuous")
	}

	for _, prefix := range []string{"EXPLAIN ", "PROFILE "} {
		t.Run(prefix, func(t *testing.T) {
			r, err := eng.Run(ctx, prefix+q, nil)
			if err != nil {
				t.Fatalf("Run(%s): %v", prefix, err)
			}
			defer func() { _ = r.Close() }()
			if got := len(r.Notifications()); got != wantN {
				t.Errorf("%s reported %d notifications, want %d", prefix, got, wantN)
			}
		})
	}
}

// TestPlanPrefix_ExplainDoesNotRequireParameters pins the parameter contract, and
// its PROFILE half. EXPLAIN plans without a bound parameter, as Engine.Explain
// does; PROFILE executes, so it demands one like any other execution.
func TestPlanPrefix_ExplainDoesNotRequireParameters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newPrefixGraph(t, 3)
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	const q = "MATCH (n:Person) WHERE n.age > $threshold RETURN n"

	r, err := eng.Run(ctx, "EXPLAIN "+q, nil)
	if err != nil {
		t.Fatalf("EXPLAIN with no parameters: %v", err)
	}
	if r.Plan() == nil {
		t.Error("EXPLAIN with no parameters produced no plan")
	}
	if n := drainRows(t, r); n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}

	if pr, perr := eng.Run(ctx, "PROFILE "+q, nil); perr == nil {
		_ = pr.Close()
		t.Error("PROFILE ran with an unbound parameter")
	}
	// Control: with the parameter bound, PROFILE runs.
	pr, perr := eng.RunAny(ctx, "PROFILE "+q, map[string]any{"threshold": 20})
	if perr != nil {
		t.Fatalf("PROFILE with the parameter bound: %v", perr)
	}
	if pr.Profile() == nil {
		t.Error("PROFILE produced no profile")
	}
	_ = drainRows(t, pr)
}

// TestPlanPrefix_SchemaStatementIsRejected pins the documented limitation, and
// with it the property that matters: a schema statement is parsed by the
// hand-written DDL parser, which the Cypher grammar does not cover, so a
// prefixed one is a SYNTAX ERROR — and therefore creates nothing.
//
// The control arm runs each statement WITHOUT the prefix and requires it to
// succeed, so a case that fails for some unrelated reason cannot masquerade as
// this one.
func TestPlanPrefix_SchemaStatementIsRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []string{
		"CREATE INDEX pp_idx FOR (n:Person) ON (n.name)",
		"CREATE CONSTRAINT pp_c ON (n:Person) ASSERT n.name IS UNIQUE",
		"SHOW INDEXES",
	}
	for _, stmt := range cases {
		for _, prefix := range []string{"EXPLAIN ", "PROFILE "} {
			t.Run(prefix+stmt, func(t *testing.T) {
				t.Parallel()
				g := newPrefixGraph(t, 2)
				eng := cypher.NewEngine(g)
				t.Cleanup(func() { _ = eng.Close() })

				before := len(eng.ListIndexes())
				r, err := eng.RunAny(ctx, prefix+stmt, nil)
				if err == nil {
					_ = r.Close()
					t.Fatalf("%q was accepted", prefix+stmt)
				}
				if !strings.Contains(err.Error(), "parse") {
					t.Errorf("%q failed with a non-parse error: %v", prefix+stmt, err)
				}
				if after := len(eng.ListIndexes()); after != before {
					t.Errorf("%q changed the schema: %d -> %d indexes", prefix+stmt, before, after)
				}

				// Control: the same statement without the prefix must be accepted.
				ctrl, cerr := eng.RunAny(ctx, stmt, nil)
				if cerr != nil {
					t.Fatalf("control %q was refused too (%v); the assertion above proves nothing", stmt, cerr)
				}
				_ = drainRows(t, ctrl)
			})
		}
	}
}

// TestPlanPrefix_NoPrefixCarriesNoPlan holds the negative: an ordinary statement
// exposes neither a plan nor a profile, so a caller cannot mistake one query's
// diagnostics for another's.
func TestPlanPrefix_NoPrefixCarriesNoPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newPrefixGraph(t, 3)
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	r, err := eng.Run(ctx, "MATCH (n:Person) RETURN n", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Plan() != nil {
		t.Error("Plan() non-nil for an un-prefixed statement")
	}
	if r.Profile() != nil {
		t.Error("Profile() non-nil for an un-prefixed statement")
	}
	if n := drainRows(t, r); n != 3 {
		t.Errorf("rows = %d, want 3", n)
	}
}

// TestPlanPrefix_WritingStatementRendersLogicalPlan holds the writing-statement
// branch: EXPLAIN on a write has no physical tree to walk outside a transaction,
// so it captures the logical one — the same walk Engine.ExplainLogical renders.
// The captured tree must be a TREE (the root has children for a multi-operator
// plan), not a flattened list, which is what the depth reconstruction is for.
func TestPlanPrefix_WritingStatementRendersLogicalPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newPrefixGraph(t, 3)
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })

	const stmt = "MATCH (n:Person) WHERE n.age > 20 SET n.age = n.age + 1 RETURN n.age AS a"
	r, err := eng.RunAny(ctx, "EXPLAIN "+stmt, nil)
	if err != nil {
		t.Fatalf("RunAny: %v", err)
	}
	defer func() { _ = r.Close() }()
	node := r.Plan()
	if node == nil {
		t.Fatal("Plan() is nil")
	}
	if len(node.Children) == 0 {
		t.Fatalf("captured plan is a single node %q; the tree was flattened", node.Name)
	}
	// Every line the logical renderer emits must appear in the captured tree, and
	// the nesting must reproduce the renderer's own indentation.
	want, err := eng.ExplainLogical(stmt, nil)
	if err != nil {
		t.Fatalf("ExplainLogical: %v", err)
	}
	got := exec.RenderPlanNode(node)
	if countLines(got) != countLines(strings.TrimRight(want, "\n")) {
		t.Errorf("captured tree has %d lines, logical rendering has %d\ngot:\n%s\nwant:\n%s",
			countLines(got), countLines(strings.TrimRight(want, "\n")), got, want)
	}
	// The rendered tree carries the same operator names in the same order; the
	// logical rendering additionally carries cardinality annotations, which the
	// captured tree deliberately does not.
	for _, line := range strings.Split(got, "\n") {
		name := strings.TrimLeft(line, "│└├─ ")
		if name == "" {
			continue
		}
		if !strings.Contains(want, name) {
			t.Errorf("captured operator %q is absent from ExplainLogical:\n%s", name, want)
		}
	}
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
