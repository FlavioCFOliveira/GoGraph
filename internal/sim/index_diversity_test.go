package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestIndexDiversity_Scenario_Passes runs the index-diversity scenario: a hash
// (string), btree (numeric), and btree (string) index are created over an
// above-threshold graph (parallel backfill) and must stay seek-vs-scan
// consistent through write churn and every crash/recovery.
func TestIndexDiversity_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioIndexDiversity)
	if !ok {
		t.Fatalf("index-diversity scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("index-diversity run: %v", err)
	}
	if report != nil {
		t.Fatalf("index-diversity reported a violation (index inconsistency):\n%s", report)
	}
}

// TestIndexConsistency_NumericBranch exercises the numeric (btree) branch of the
// consistency checker directly on a small graph (fast): the integer-keyed scan
// must group exactly like the integer-bound seek resolves. It guards the numeric
// scan/seek path the index-diversity scenario relies on without the cost of the
// 9000-node parallel backfill.
func TestIndexConsistency_NumericBranch(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := NewEngineAdapter(cypher.NewEngine(g))
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		q := "CREATE (:Person {name:'p', age:$age})"
		if _, err := eng.RunWrite(ctx, q, map[string]any{"age": int64(i % 7)}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	if _, err := eng.RunWrite(ctx, "CREATE INDEX i_age FOR (n:Person) ON (n.age) OPTIONS {indexType:'btree'}", nil); err != nil {
		t.Fatalf("create numeric index: %v", err)
	}
	if v := CheckIndexConsistency(0, nil, eng, IndexSpec{Label: "Person", Property: "age", Numeric: true}); len(v) > 0 {
		t.Fatalf("numeric index inconsistent: %s", v[0].Message)
	}
}
