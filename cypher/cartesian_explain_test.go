package cypher_test

// cartesian_explain_test.go — regression gate for #1807 (sprint 253): a
// disconnected MATCH (a plain uncorrelated Apply) is now labelled
// CartesianProduct in EXPLAIN to match Neo4j and make the cardinality-blowup
// join greppable. Execution is unchanged (the operator semantics are identical).

import (
	"context"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

func TestDisconnectedMatchExplainCartesian_1807(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)

	plan, err := eng.Explain(`MATCH (a:A), (b:B) RETURN a, b`, nil)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(plan, "CartesianProduct") {
		t.Errorf("disconnected MATCH plan should contain CartesianProduct:\n%s", plan)
	}
	if strings.Contains(plan, "\nApply") || strings.Contains(plan, " Apply ") || strings.HasPrefix(strings.TrimSpace(plan), "Apply") {
		t.Errorf("plain Apply label should be gone (relabelled CartesianProduct):\n%s", plan)
	}

	// Execution is unchanged: 2 A-nodes x 3 B-nodes = 6 rows.
	for _, q := range []string{`CREATE (:A)`, `CREATE (:A)`, `CREATE (:B)`, `CREATE (:B)`, `CREATE (:B)`} {
		r, err := eng.RunInTx(context.Background(), q, nil)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		r.Close()
	}
	res, err := eng.Run(context.Background(), `MATCH (a:A), (b:B) RETURN a, b`, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer res.Close()
	n := 0
	for res.Next() {
		n++
	}
	if n != 6 {
		t.Errorf("Cartesian product rows = %d, want 6", n)
	}
}
