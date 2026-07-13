package sim

import (
	"context"
	"testing"
)

// TestMergeRel_Scenario_Passes runs the registered merge-rel scenario: MERGE
// (a)-[r:KNOWS]->(b) ON CREATE SET r.n=1 ON MATCH SET r.n=r.n+1 must be
// idempotent on edge count and its counter must round-trip and survive crash +
// checkpoint recovery. A nil report means the counter held on every check.
func TestMergeRel_Scenario_Passes(t *testing.T) {
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("DefaultRegistry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioMergeRel)
	if !ok {
		t.Fatalf("merge-rel scenario not registered")
	}
	report, err := sc.Run(context.Background(), sc.DefaultSeed)
	if err != nil {
		t.Fatalf("merge-rel run: %v", err)
	}
	if report != nil {
		t.Fatalf("merge-rel reported a violation (idempotency or counter broke):\n%s", report)
	}
}

// TestMergeRel_MultiSeed exercises the op mix across seeds.
func TestMergeRel_MultiSeed(t *testing.T) {
	for _, seed := range []uint64{1, 7, 0xBEEF, 0x3E4EE10} {
		report, err := runMergeRel(context.Background(), seed)
		if err != nil {
			t.Fatalf("runMergeRel(%#x): %v", seed, err)
		}
		if report != nil {
			t.Fatalf("runMergeRel(%#x) violation:\n%s", seed, report)
		}
	}
}

// TestMergeRel_Idempotentcy verifies the core MERGE-relationship invariant end
// to end: MERGEing the same pair twice does not create a parallel edge and
// increments r.n, and the checker agrees with the oracle. It also proves
// CheckMergeRel CATCHES a counter divergence.
func TestMergeRel_Idempotency(t *testing.T) {
	eng := newTestEngine(t)
	a := NewEngineAdapter(eng)
	oracle := NewGraphOracle()
	ctx := context.Background()

	for _, nm := range []string{"Ada", "Alan"} {
		if _, err := a.RunWrite(ctx, tmplCreatePerson, map[string]any{"name": nm, "age": int64(30)}); err != nil {
			t.Fatalf("create %s: %v", nm, err)
		}
		oracle.ApplyCreate(tmplCreatePerson, map[string]any{"name": nm, "age": int64(30)})
	}

	merge := func() {
		t.Helper()
		if _, err := a.RunWrite(ctx, tmplMergeKnowsN, map[string]any{"a": "Ada", "b": "Alan"}); err != nil {
			t.Fatalf("merge: %v", err)
		}
		oracle.ApplyMerge(tmplMergeKnowsN, map[string]any{"a": "Ada", "b": "Alan"})
	}

	merge() // ON CREATE: r.n=1, one edge
	merge() // ON MATCH:  r.n=2, still one edge
	merge() // ON MATCH:  r.n=3, still one edge

	if n, err := a.EdgeCount(); err != nil || n != 1 {
		t.Fatalf("edge count after 3 merges: n=%d err=%v, want 1 (idempotent)", n, err)
	}
	if oracle.EdgeCount() != 1 {
		t.Fatalf("oracle edge count: %d, want 1", oracle.EdgeCount())
	}
	if v := CheckMergeRel(0, oracle, a); len(v) > 0 {
		t.Fatalf("faithful counter should be clean, got: %v", v)
	}

	// Counter divergence: oracle claims 99 but engine holds 3.
	for _, e := range oracle.KnowsEdgesByName() {
		e.Props["n"] = int64(99)
	}
	if v := CheckMergeRel(0, oracle, a); len(v) == 0 {
		t.Fatalf("checker missed a counter divergence")
	}
}
