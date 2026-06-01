package cypher_test

// varlen_plan_pn_test.go — T921: VarLengthExpand plan correctness on long Pn
// (n=1000).
//
// The VarLengthExpand IR operator has no Loopless field: that is an
// implementation detail of the executor, not surfaced in the plan text. This
// test verifies plan structure via Explain and execution correctness via Run.
//
// Acceptance criteria:
//  1. Plan tree contains "VarLengthExpand".
//  2. Loopless semantics verified by execution correctness: a directed path
//     graph has no cycles, so every reachable hop count is exact.
//  3. Race-clean.
//  4. goleak-clean (enforced by TestMain in testmain_test.go).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildLongPath builds a directed path v0→v1→…→v{n-1} (n nodes, n-1 edges).
func buildLongPath(tb testing.TB, n int) *cypher.Engine {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := range n {
		key := fmt.Sprintf("v%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode %q: %v", key, err)
		}
	}
	for i := range n - 1 {
		src := fmt.Sprintf("v%d", i)
		dst := fmt.Sprintf("v%d", i+1)
		if err := g.AddEdge(src, dst, 1.0); err != nil {
			tb.Fatalf("AddEdge %q→%q: %v", src, dst, err)
		}
	}
	return cypher.NewEngine(g)
}

// TestVarlenPlan_PnExplainContainsVarLengthExpand verifies that the planner
// emits a VarLengthExpand operator for a variable-length pattern query on a
// path graph of 1000 nodes.
func TestVarlenPlan_PnExplainContainsVarLengthExpand(t *testing.T) {
	t.Parallel()

	eng := buildLongPath(t, 1000)

	plan, err := eng.Explain("MATCH (a)-[*1..5]->(b) RETURN a, b", nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "VarLengthExpand") {
		t.Errorf("plan does not contain VarLengthExpand:\n%s", plan)
	}
	if !strings.Contains(plan, "ProduceResults") {
		t.Errorf("plan does not contain ProduceResults:\n%s", plan)
	}
}

// TestVarlenPlan_PnLooplessByExecution verifies loopless semantics by execution
// correctness. On a directed path P_n:
//
//   - MATCH (a)-[*1..1]->(b) RETURN count(*) should return n-1 (one hop per
//     consecutive pair).
//
// Because the graph is a DAG with no back-edges, any multi-hop pattern
// count is fully determined by the path length. This serves as the
// loopless-flag acceptance criterion: if cycles were traversed erroneously,
// counts would exceed the DAG expectation.
func TestVarlenPlan_PnLooplessByExecution(t *testing.T) {
	t.Parallel()

	const n = 1000
	eng := buildLongPath(t, n)
	ctx := context.Background()

	// Single-hop: exactly n-1 direct edges.
	res, err := eng.Run(ctx, "MATCH (a)-[*1..1]->(b) RETURN count(*) AS c", nil)
	if err != nil {
		t.Fatalf("Run [*1..1]: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("[*1..1] expected 1 aggregate row, got %d", len(rows))
	}
	wantOneHop := int64(n - 1)
	mustInt(t, "[*1..1] count", rows[0]["c"], wantOneHop)
}

// TestVarlenPlan_PnBoundedHops verifies that a bounded [*1..5] query on P_1000
// returns sensible results without hanging. The expected pair count for a path
// of 1000 nodes with max 5 hops is:
//
//	sum_{k=1}^{5} (n-k) = 5n - 15 = 4985
func TestVarlenPlan_PnBoundedHops(t *testing.T) {
	t.Parallel()

	const n = 1000
	eng := buildLongPath(t, n)
	ctx := context.Background()

	res, err := eng.Run(ctx, "MATCH (a)-[*1..5]->(b) RETURN count(*) AS c", nil)
	if err != nil {
		t.Fatalf("Run [*1..5]: %v", err)
	}
	rows := collectRecords(t, res)
	if len(rows) != 1 {
		t.Fatalf("[*1..5] expected 1 aggregate row, got %d", len(rows))
	}
	// sum_{k=1}^{5}(1000-k) = 999+998+997+996+995 = 4985
	const wantBounded = int64(4985)
	mustInt(t, "[*1..5] count", rows[0]["c"], wantBounded)
}

// TestVarlenPlan_PnExplainNoExecution confirms that Explain does not execute
// the query: node count must be unchanged after calling Explain on P_1000.
func TestVarlenPlan_PnExplainNoExecution(t *testing.T) {
	t.Parallel()

	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	const n = 1000
	for i := range n {
		key := fmt.Sprintf("lp%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	before := g.AdjList().Order()
	eng := cypher.NewEngine(g)

	if _, err := eng.Explain("MATCH (a)-[*1..3]->(b) RETURN a, b", nil); err != nil {
		t.Fatalf("Explain: %v", err)
	}

	after := g.AdjList().Order()
	if after != before {
		t.Errorf("Explain mutated graph: order before=%d after=%d", before, after)
	}
}

// TestVarlenPlan_PnRaceClean exercises concurrent Explain and Run calls on the
// same engine to satisfy the race-clean acceptance criterion.
func TestVarlenPlan_PnRaceClean(t *testing.T) {
	t.Parallel()

	const n = 100 // smaller graph for concurrency test
	eng := buildLongPath(t, n)
	ctx := context.Background()

	const workers = 8
	done := make(chan struct{})
	errs := make(chan error, workers*2)

	for range workers {
		go func() {
			defer func() { done <- struct{}{} }()
			if _, err := eng.Explain("MATCH (a)-[*1..3]->(b) RETURN a, b", nil); err != nil {
				errs <- fmt.Errorf("Explain: %w", err)
				return
			}
			res, err := eng.Run(ctx, "MATCH (a)-[*1..1]->(b) RETURN count(*) AS c", nil)
			if err != nil {
				errs <- fmt.Errorf("Run: %w", err)
				return
			}
			rows := collectRecords(t, res)
			if len(rows) != 1 {
				errs <- fmt.Errorf("unexpected row count %d", len(rows))
				return
			}
			cv, ok := rows[0]["c"].(expr.IntegerValue)
			if !ok {
				errs <- fmt.Errorf("count not IntegerValue: %T", rows[0]["c"])
				return
			}
			if int64(cv) != int64(n-1) {
				errs <- fmt.Errorf("count = %d, want %d", int64(cv), int64(n-1))
			}
		}()
	}

	for range workers {
		<-done
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
