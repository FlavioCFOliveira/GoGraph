package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// newTestEngine builds an in-memory directed engine for checker/adapter tests.
func newTestEngine(t *testing.T) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	return cypher.NewEngine(g)
}

// TestEngineAdapter_Counts verifies the adapter's count probes track real
// engine mutations.
func TestEngineAdapter_Counts(t *testing.T) {
	eng := newTestEngine(t)
	ctx := context.Background()
	a := NewEngineAdapter(eng)

	if n, err := a.NodeCount(); err != nil || n != 0 {
		t.Fatalf("initial NodeCount: n=%d err=%v", n, err)
	}

	mustRun(t, eng, "CREATE (n:Person {name:$name, age:$age})",
		map[string]expr.Value{"name": expr.StringValue("Ada"), "age": expr.IntegerValue(36)})
	mustRun(t, eng, "CREATE (n:Person {name:$name, age:$age})",
		map[string]expr.Value{"name": expr.StringValue("Alan"), "age": expr.IntegerValue(41)})
	mustRun(t, eng, "MATCH (a:Person {name:$a}),(b:Person {name:$b}) CREATE (a)-[:KNOWS]->(b)",
		map[string]expr.Value{"a": expr.StringValue("Ada"), "b": expr.StringValue("Alan")})

	if n, err := a.NodeCount(); err != nil || n != 2 {
		t.Fatalf("NodeCount after creates: n=%d err=%v", n, err)
	}
	if n, err := a.EdgeCount(); err != nil || n != 1 {
		t.Fatalf("EdgeCount after creates: n=%d err=%v", n, err)
	}
	_ = ctx
}

// TestInvariantChecker_CleanWhenInSync verifies no violations when oracle and
// engine agree.
func TestInvariantChecker_CleanWhenInSync(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	oracle := NewGraphOracle()
	chk := NewInvariantChecker(NewSeed(1))

	params := map[string]any{"name": "Ada", "age": int64(36)}
	mustRun(t, eng, tmplCreatePerson, toExpr(t, params))
	oracle.ApplyCreate(tmplCreatePerson, params)

	if v := chk.Check(1, oracle, a); v != nil {
		t.Fatalf("expected clean check, got %v", v)
	}
	if chk.HasViolations() {
		t.Fatalf("checker recorded violations: %v", chk.Violations())
	}
}

// TestInvariantChecker_DetectsCountMismatch verifies a deliberate divergence is
// caught as an ACID_CONSISTENCY violation.
func TestInvariantChecker_DetectsCountMismatch(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	oracle := NewGraphOracle()
	chk := NewInvariantChecker(NewSeed(1))

	// Oracle thinks a node exists; engine has none.
	oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": "Ghost", "age": int64(1)})

	v := chk.Check(1, oracle, a)
	if len(v) == 0 {
		t.Fatal("expected a violation for count mismatch")
	}
	found := false
	for _, viol := range v {
		if viol.Kind == ViolationACIDConsistency {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected ACID_CONSISTENCY, got %v", v)
	}
}

func mustRun(t *testing.T, eng *cypher.Engine, q string, p map[string]expr.Value) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, p)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Run(%q) drain: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Run(%q) close: %v", q, err)
	}
}

func toExpr(t *testing.T, p map[string]any) map[string]expr.Value {
	t.Helper()
	ev, err := toExprParams(p)
	if err != nil {
		t.Fatalf("toExprParams: %v", err)
	}
	return ev
}
