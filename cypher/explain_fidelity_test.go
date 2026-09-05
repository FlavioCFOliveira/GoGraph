package cypher_test

// explain_fidelity_test.go — rmp #2720.
//
// [cypher.Engine.explainPhysical] claims to build the physical operator tree
// "exactly as the read path builds it". Nothing tested that claim against a real
// EXECUTION: the existing gates compare EXPLAIN against a runtime counter for two
// specific substitutions (explain_physical_test.go), which proves the rendering
// names the right operator but not that the WHOLE tree matches.
//
// The oracle here is the plan a run actually built. PROFILE installs its
// instrumentation inside the same buildReadPhysical the un-prefixed read path
// uses and then executes, so the tree it captures is the tree that ran; EXPLAIN
// builds and throws away. Stripping the measurements off the profiled tree leaves
// a structure that must be identical to EXPLAIN's, operator for operator and
// detail for detail. It is not a comparison of two renderings: one side ran.
//
// Three conditions are covered because each could break the claim on its own:
//
//   - COLD, with an empty plan cache;
//   - WARM, on a plan-cache HIT, where EXPLAIN and the run read a cached
//     planCacheEntry rather than re-analysing;
//   - PARAMETERISED, where an access-path gate reads a parameter's VALUE. The
//     sprint's own history records a parameter that full-scanned where the
//     identical literal seeked, which is exactly the shape that would make EXPLAIN
//     lie about a run.
//
// The fourth test records the one case where EXPLAIN and the run genuinely
// DIVERGE, and pins it so it cannot be quietly forgotten.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// planShape renders a captured tree's STRUCTURE — operator names, their details
// and their nesting — with every measurement omitted, so a plan built by EXPLAIN
// and a plan captured from a real run are directly comparable.
func planShape(n *exec.PlanNode, depth int) string {
	var b strings.Builder
	b.WriteString(strings.Repeat("  ", depth))
	b.WriteString(n.Name)
	if n.Detail != "" {
		b.WriteString(" [" + n.Detail + "]")
	}
	b.WriteByte('\n')
	for i := range n.Children {
		b.WriteString(planShape(&n.Children[i], depth+1))
	}
	return b.String()
}

// explainShape returns the shape of the plan `EXPLAIN <query>` reports. EXPLAIN
// executes nothing, so this is a plan built and discarded.
func explainShape(t *testing.T, eng *cypher.Engine, query string, params map[string]expr.Value) string {
	t.Helper()
	r, err := eng.Run(context.Background(), "EXPLAIN "+query, params)
	if err != nil {
		t.Fatalf("EXPLAIN %s: %v", query, err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
	}
	if err := r.Err(); err != nil {
		t.Fatalf("EXPLAIN %s drain: %v", query, err)
	}
	p := r.Plan()
	if p == nil {
		t.Fatalf("EXPLAIN %s returned no plan", query)
	}
	return planShape(p, 0)
}

// executedShape returns the shape of the plan that ACTUALLY RAN, captured by
// PROFILE from the executed operator tree.
func executedShape(t *testing.T, eng *cypher.Engine, query string, params map[string]expr.Value) string {
	t.Helper()
	r, err := eng.Run(context.Background(), "PROFILE "+query, params)
	if err != nil {
		t.Fatalf("PROFILE %s: %v", query, err)
	}
	defer func() { _ = r.Close() }()
	for r.Next() {
	}
	if err := r.Err(); err != nil {
		t.Fatalf("PROFILE %s drain: %v", query, err)
	}
	p := r.Profile()
	if p == nil {
		t.Fatalf("PROFILE %s returned no profile", query)
	}
	if !p.Profiled {
		t.Fatalf("PROFILE %s captured a tree with no measurements, so it did not "+
			"execute and cannot serve as the oracle", query)
	}
	return planShape(p, 0)
}

// seedFidelityGraph builds 300 :P nodes over 30 age buckets with an index on
// age, which is enough for the index-seek and join substitutions to fire.
func seedFidelityGraph(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	for i := 0; i < 300; i++ {
		runHonestyWrite(t, eng, fmt.Sprintf("CREATE (:P {age: %d, name: 'n%d'})", i%30, i))
	}
	runHonestyWrite(t, eng, "CREATE INDEX FOR (n:P) ON (n.age)")
	return eng
}

// TestExplainFidelity_MatchesTheTreeThatRuns is the acceptance gate for the
// claim on Engine.explainPhysical.
func TestExplainFidelity_MatchesTheTreeThatRuns(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		query  string
		params map[string]expr.Value
	}{
		{
			name:  "literal equality reaching an index",
			query: "MATCH (n:P) WHERE n.age = 7 RETURN n.name",
		},
		{
			name:   "the same predicate supplied as a parameter",
			query:  "MATCH (n:P) WHERE n.age = $a RETURN n.name",
			params: map[string]expr.Value{"a": expr.IntegerValue(7)},
		},
		{
			name:  "an equi-join the planner may substitute",
			query: "MATCH (a:P), (b:P) WHERE a.age = b.age RETURN count(*)",
		},
		{
			name:   "a range predicate from a parameter",
			query:  "MATCH (n:P) WHERE n.age > $lo RETURN n.name",
			params: map[string]expr.Value{"lo": expr.IntegerValue(25)},
		},
		{
			name:  "an expansion",
			query: "MATCH (a:P)-[:KNOWS]->(b) RETURN b.name",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			eng := seedFidelityGraph(t)

			// COLD: nothing has planned this query yet.
			cold := explainShape(t, eng, c.query, c.params)
			coldRan := executedShape(t, eng, c.query, c.params)
			if cold != coldRan {
				t.Errorf("COLD: EXPLAIN renders a plan the run did not build.\n"+
					"EXPLAIN:\n%s\nEXECUTED:\n%s", cold, coldRan)
			}

			// WARM: both sides now read a cached planCacheEntry. A cache that
			// carried a decision taken for a different binding would show here.
			warm := explainShape(t, eng, c.query, c.params)
			warmRan := executedShape(t, eng, c.query, c.params)
			if warm != warmRan {
				t.Errorf("WARM (plan-cache hit): EXPLAIN renders a plan the run did not "+
					"build.\nEXPLAIN:\n%s\nEXECUTED:\n%s", warm, warmRan)
			}
			if cold != warm {
				t.Errorf("EXPLAIN reports a different plan cold and warm, so the plan "+
					"cache changes what a diagnostic reports.\nCOLD:\n%s\nWARM:\n%s",
					cold, warm)
			}
		})
	}
}

// TestExplainFidelity_UnboundParameterDivergesFromTheRun pins the ONE case where
// EXPLAIN's plan is not the plan that runs.
//
// EXPLAIN deliberately does not require parameters — plan_prefix.go documents
// that, and Neo4j behaves the same way — but an access-path gate that reads a
// parameter's VALUE cannot fire without one, so the plan rendered for an unbound
// `$a` is a full label scan while the run with `$a` bound seeks an index. Nothing
// in the rendered output says the plan was chosen without the value.
//
// The test asserts the divergence rather than the absence of it, so that a future
// change which closes the gap (by requiring the parameter, by rendering a
// warning, or by planning the same way in both cases) FAILS here and is
// reconciled with the documentation deliberately.
func TestExplainFidelity_UnboundParameterDivergesFromTheRun(t *testing.T) {
	t.Parallel()
	eng := seedFidelityGraph(t)
	const q = "MATCH (n:P) WHERE n.age = $a RETURN n.name"
	bound := map[string]expr.Value{"a": expr.IntegerValue(7)}

	unbound := explainShape(t, eng, q, nil)
	withValue := explainShape(t, eng, q, bound)
	ran := executedShape(t, eng, q, bound)

	if withValue != ran {
		t.Fatalf("EXPLAIN with the parameter BOUND already disagrees with the run, so "+
			"this test is measuring the wrong thing.\nEXPLAIN:\n%s\nEXECUTED:\n%s",
			withValue, ran)
	}
	if unbound == ran {
		t.Errorf("EXPLAIN with an UNBOUND parameter now renders the same plan the "+
			"bound run builds. That is an improvement, not a failure — but it "+
			"contradicts the note on Engine.runExplainPrefixed and the audit in "+
			"docs/explain-profile-honesty-audit-2026-09-03.md, both of which record "+
			"the divergence as current behaviour. Update them with this change.\n%s",
			unbound)
	}
	// The specific shape of the divergence, so the record is concrete rather than
	// merely "they differ".
	if !strings.Contains(unbound, "NodeByLabelScan") {
		t.Errorf("the unbound plan no longer scans; the divergence has changed shape "+
			"and the documentation describing it is now wrong:\n%s", unbound)
	}
	if !strings.Contains(ran, "NodeByIndexRangeScan") && !strings.Contains(ran, "NodeByIndexSeek") {
		t.Errorf("the bound run no longer seeks an index, so this case no longer "+
			"demonstrates a plan a reader of EXPLAIN would be misled about:\n%s", ran)
	}
}
