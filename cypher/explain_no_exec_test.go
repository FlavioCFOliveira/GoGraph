package cypher_test

// explain_no_exec_test.go — T908: EXPLAIN returns plan without execution.
//
// Verifies:
//  1. Explain returns a non-empty plan string.
//  2. The plan contains known operator names (AllNodesScan / NodeByLabelScan /
//     ProduceResults).
//  3. The graph is not mutated by Explain.
//  4. Race-clean (parallel sub-tests, no shared mutable state).
//  5. goleak-clean (no goroutine leak).

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestExplain_NoExec is the T908 acceptance test.
func TestExplain_NoExec(t *testing.T) {
	t.Parallel()

	t.Run("AllNodesScan_plan_returned", func(t *testing.T) {
		t.Parallel()

		g := lpg.New[string, float64](adjlist.Config{})
		for i := range 5 {
			if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		eng := cypher.NewEngine(g)

		plan, err := eng.Explain("MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if plan == "" {
			t.Fatal("Explain returned empty plan string")
		}
		for _, want := range []string{"AllNodesScan", "ProduceResults"} {
			if !strings.Contains(plan, want) {
				t.Errorf("plan missing %q:\n%s", want, plan)
			}
		}
	})

	t.Run("NodeByLabelScan_plan_returned", func(t *testing.T) {
		t.Parallel()

		g := lpg.New[string, float64](adjlist.Config{})
		for i := range 5 {
			key := fmt.Sprintf("p%d", i)
			if err := g.AddNode(key); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
			if err := g.SetNodeLabel(key, "Person"); err != nil {
				t.Fatalf("SetNodeLabel: %v", err)
			}
		}
		eng := cypher.NewEngine(g)

		plan, err := eng.Explain("MATCH (n:Person) RETURN n", nil)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		if plan == "" {
			t.Fatal("Explain returned empty plan string")
		}
		for _, want := range []string{"NodeByLabelScan", "ProduceResults"} {
			if !strings.Contains(plan, want) {
				t.Errorf("plan missing %q:\n%s", want, plan)
			}
		}
	})

	t.Run("no_graph_mutation", func(t *testing.T) {
		t.Parallel()

		g := lpg.New[string, float64](adjlist.Config{})
		for i := range 3 {
			if err := g.AddNode(fmt.Sprintf("m%d", i)); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		before := g.AdjList().Order()
		eng := cypher.NewEngine(g)

		_, err := eng.Explain("MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}

		after := g.AdjList().Order()
		if after != before {
			t.Errorf("Explain mutated graph: node count before=%d after=%d", before, after)
		}
	})

	t.Run("no_execution_no_rows", func(t *testing.T) {
		t.Parallel()

		// Explain must not produce rows — verified indirectly: Run after Explain
		// must still return the expected row count (i.e. Explain did not consume
		// the graph's internal cursor state or mutate any structure).
		g := lpg.New[string, float64](adjlist.Config{})
		const n = 7
		for i := range n {
			if err := g.AddNode(fmt.Sprintf("x%d", i)); err != nil {
				t.Fatalf("AddNode: %v", err)
			}
		}
		eng := cypher.NewEngine(g)

		if _, err := eng.Explain("MATCH (n) RETURN n", nil); err != nil {
			t.Fatalf("Explain: %v", err)
		}

		res, err := eng.Run(context.Background(), "MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatalf("Run after Explain: %v", err)
		}
		defer res.Close()

		var count int
		for res.Next() {
			count++
		}
		if err := res.Err(); err != nil {
			t.Fatalf("Result.Err: %v", err)
		}
		if count != n {
			t.Errorf("Run after Explain: want %d rows, got %d", n, count)
		}
	})

	t.Run("empty_graph_plan_returned", func(t *testing.T) {
		t.Parallel()

		g := lpg.New[string, float64](adjlist.Config{})
		eng := cypher.NewEngine(g)

		plan, err := eng.Explain("MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatalf("Explain on empty graph: %v", err)
		}
		if plan == "" {
			t.Fatal("Explain on empty graph returned empty plan string")
		}
	})
}
