//go:build r4audit

// Package r4audit holds the evidence harness for the round-4 comparative audit
// (docs/audit-vs-neo4j-memgraph-2026-07-27.md). Every claim that report makes
// about GoGraph's own behaviour is reproducible from this package.
//
// It is gated behind the `r4audit` build tag because these are probes, not
// assertions: most of them print a matrix for a human to read rather than
// failing on a condition, so they do not belong in the `make ci` short layer.
//
//	go test -tags=r4audit -v ./bench/r4audit/
//
// | Test | Report finding |
// |---|---|
// | TestDocumentedExistsExampleIsRejected | C1 — docs/cypher.md:132 example fails |
// | TestSubqueryForms | C1 — the accepted/rejected boundary |
// | TestStringOrdering | C3 — code-point vs UTF-16 collation |
// | TestParamCoercion | E1 — typed slices rejected |
// | TestShowProjection | C2 — SHOW yields nil values |
// | TestPerOuterRowCost | P2 — the per-outer-row tax, COUNT{} at 88x |
// | TestPlans, TestSharedVariableJoinPlans, TestHashJoinTrigger | P3 — Explain renders the logical IR |
// | TestEdgeLoadDecomposition, TestRelCreateRootCause | W1 §2.1 steps 1-2 — which clause carries the cost |
// | TestSeekReachesWriteStatements, TestIsTheWriteClauseRelevant | W1 §2.1 steps 3-4 — the two refuted hypotheses |
// | TestUnwindSeekPlans | W1 — the CartesianProduct(Unwind, LabelScan) shape |
// | TestW1PartA_PlanShapes, TestW1PartA_GatesEngage, TestW1PartA_MinLabelWriteWin | W1 part A — the 145x min-label write win |
// | TestMergeBindsAllMatches | P0 — WITHDREW this report's own early-exit recommendation |
//
// The permanent regression gate for part A is NOT here — it is
// `cypher/write_path_gates_test.go`, in-package because it reads the planner's
// own build counters, and in the short layer because it must run in `make ci`.
//
// TestHashJoinTrigger is deliberately kept even though its premise was WRONG:
// it reads `Explain` and therefore reports "no hash join" for queries that do
// substitute one. That is finding P3 demonstrating itself, and it is the
// reason the report's physical-plan claims are sourced from the runtime
// counters instead.
package r4audit

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newEng seeds n :P nodes with a deterministic out-degree of 4 :K edges each,
// built through the Go API so seeding stays O(n) — a Cypher `MATCH (a),(b)`
// seeder is O(n²) and dominates the measurement at n ≥ 4000.
func newEng(tb testing.TB, n int) *cypher.Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "P"); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "id", lpg.Int64Value(int64(i))); err != nil {
			tb.Fatalf("SetNodeProperty(id): %v", err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(key)); err != nil {
			tb.Fatalf("SetNodeProperty(name): %v", err)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(i%90))); err != nil {
			tb.Fatalf("SetNodeProperty(age): %v", err)
		}
	}
	const stride = 104729 // prime — spreads neighbours across the id space
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("n%d", i)
		for k := 1; k <= 4; k++ {
			dst := fmt.Sprintf("n%d", (i+k*stride)%n)
			if err := g.AddEdge(src, dst, 0); err != nil {
				tb.Fatalf("AddEdge: %v", err)
			}
			g.SetEdgeLabel(src, dst, "K")
		}
	}
	return cypher.NewEngine(g)
}

func TestPlans(t *testing.T) {
	eng := newEng(t, 200)
	shapes := []struct{ name, q string }{
		{"exists-subquery", `MATCH (a:P) WHERE EXISTS { MATCH (a)-[:K]->(b:P) } RETURN a.id`},
		{"count-subquery", `MATCH (a:P) WHERE COUNT { MATCH (a)-[:K]->(b:P) } > 0 RETURN a.id`},
		{"collect-subquery", `MATCH (a:P) RETURN a.id, [ (a)-[:K]->(b) | b.id ] AS ns`},
		{"pattern-predicate", `MATCH (a:P) WHERE (a)-[:K]->(:P) RETURN a.id`},
		{"call-subquery", `MATCH (a:P) CALL { WITH a MATCH (a)-[:K]->(b) RETURN b } RETURN a.id, b.id`},
		{"varlen-1-3", `MATCH (a:P)-[:K*1..3]->(b:P) RETURN count(*)`},
		{"varlen-bounded-from", `MATCH (a:P {id: 7})-[:K*1..3]->(b:P) RETURN count(*)`},
		{"shortest-path", `MATCH (a:P {id: 7}), (b:P {id: 99}) MATCH p = shortestPath((a)-[:K*..6]-(b)) RETURN length(p)`},
		{"optional-match", `MATCH (a:P) OPTIONAL MATCH (a)-[:K]->(b) RETURN a.id, b.id`},
		{"unwind-create", `UNWIND range(1,10) AS i CREATE (:Q {id: i})`},
		{"unwind-merge", `UNWIND range(1,10) AS i MERGE (:P {id: i})`},
		{"func-call", `MATCH (a:P) RETURN toUpper(a.name), size(a.name), abs(a.age - 45)`},
		{"list-comprehension", `MATCH (a:P) RETURN [x IN range(1,5) WHERE x > 2 | x * a.age] AS l`},
		{"hash-join", `MATCH (a:P)-[:K]->(b:P), (c:P)-[:K]->(b) RETURN count(*)`},
		{"order-by-string", `MATCH (a:P) RETURN a.name ORDER BY a.name LIMIT 10`},
	}
	for _, s := range shapes {
		plan, err := eng.Explain(s.q, nil)
		if err != nil {
			t.Logf("=== %-22s ERROR: %v", s.name, err)
			continue
		}
		fmt.Printf("=== %s\n%s\n", s.name, plan)
	}
}
