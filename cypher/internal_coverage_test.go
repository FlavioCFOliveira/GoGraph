package cypher

import (
	"context"
	"testing"

	"gograph/cypher/exec"
	"gograph/cypher/funcs"
	"gograph/cypher/ir"
	"gograph/graph"
	"gograph/graph/adjlist"
	"gograph/graph/lpg"
)

// ─────────────────────────────────────────────────────────────────────────────
// BuildPlan — direct access to unexported walker/labelSrc types
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildPlan_AllNodesScan(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("A")
	g.AddNode("B")

	walker := &lpgNodeWalker{g: g}
	labelSrc := &lpgLabelResolver{g: g}

	scan := ir.NewAllNodesScan("n")
	plan := ir.NewProduceResults([]string{"n"}, scan)

	op, cols, err := BuildPlan(plan, walker, labelSrc, funcs.DefaultRegistry, nil)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(cols) != 1 || cols[0] != "n" {
		t.Errorf("unexpected cols: %v", cols)
	}

	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("op.Init: %v", err)
	}
	defer op.Close()

	count := 0
	var row exec.Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("op.Next: %v", err)
		}
		if !ok {
			break
		}
		count++
	}
	if count != 2 {
		t.Errorf("expected 2 rows, got %d", count)
	}
}

func TestBuildPlan_NonProduceResultsRoot(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	walker := &lpgNodeWalker{g: g}
	labelSrc := &lpgLabelResolver{g: g}

	scan := ir.NewAllNodesScan("n")
	_, _, err := BuildPlan(scan, walker, labelSrc, funcs.DefaultRegistry, nil)
	if err == nil {
		t.Fatal("expected error for non-ProduceResults root")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// BuildPlanWithMutator
// ─────────────────────────────────────────────────────────────────────────────

func TestBuildPlanWithMutator_AllNodesScan(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("X")

	walker := &lpgNodeWalker{g: g}
	labelSrc := &lpgLabelResolver{g: g}
	mutator := &lpgMutatorAdapter{g: g}

	scan := ir.NewAllNodesScan("n")
	plan := ir.NewProduceResults([]string{"n"}, scan)

	op, cols, err := BuildPlanWithMutator(plan, walker, labelSrc, funcs.DefaultRegistry, nil, mutator)
	if err != nil {
		t.Fatalf("BuildPlanWithMutator: %v", err)
	}
	if len(cols) != 1 || cols[0] != "n" {
		t.Errorf("unexpected cols: %v", cols)
	}

	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("op.Init: %v", err)
	}
	defer op.Close()

	var row exec.Row
	for {
		ok, err := op.Next(&row)
		if err != nil {
			t.Fatalf("op.Next: %v", err)
		}
		if !ok {
			break
		}
	}
}

func TestBuildPlanWithMutator_WriteOnlyRoot(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	walker := &lpgNodeWalker{g: g}
	labelSrc := &lpgLabelResolver{g: g}
	mutator := &lpgMutatorAdapter{g: g}

	// A write-only plan (no ProduceResults wrapper) — exercises the
	// non-ProduceResults branch in BuildPlanWithMutator.
	scan := ir.NewAllNodesScan("n")
	op, _, err := BuildPlanWithMutator(scan, walker, labelSrc, funcs.DefaultRegistry, nil, mutator)
	if err != nil {
		t.Fatalf("BuildPlanWithMutator (write-only): %v", err)
	}
	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("op.Init: %v", err)
	}
	defer op.Close()
}

// ─────────────────────────────────────────────────────────────────────────────
// lpgMutatorAdapter: edge operations
// ─────────────────────────────────────────────────────────────────────────────

func TestLpgMutatorAdapter_HasAndRemoveEdge(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	g.AddNode("S")
	g.AddNode("D")
	g.AddEdge("S", "D", 1.0)

	a := &lpgMutatorAdapter{g: g}

	if !a.HasEdge("S", "D") {
		t.Error("expected edge S→D to exist")
	}
	if a.HasEdge("D", "S") {
		t.Error("expected no reverse edge D→S")
	}

	a.RemoveEdge("S", "D")
	if a.HasEdge("S", "D") {
		t.Error("expected edge S→D to be removed")
	}
}

func TestLpgMutatorAdapter_DelEdgeProperty(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	g.AddNode("S")
	g.AddNode("D")
	g.AddEdge("S", "D", 1.0)
	g.SetEdgeProperty("S", "D", "weight", lpg.Int64Value(42))

	a := &lpgMutatorAdapter{g: g}
	a.DelEdgeProperty("S", "D", "weight")
}

func TestLpgMutatorAdapter_WalkNodeIDs(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("P")
	g.AddNode("Q")

	a := &lpgMutatorAdapter{g: g}
	count := 0
	a.WalkNodeIDs(func(_ graph.NodeID) bool {
		count++
		return true
	})
	if count != 2 {
		t.Errorf("expected 2 nodes, got %d", count)
	}
}

func TestLpgMutatorAdapter_ResolveNodeID(t *testing.T) {
	t.Parallel()
	g := lpg.New[string, float64](adjlist.Config{})
	g.AddNode("N")

	a := &lpgMutatorAdapter{g: g}

	id, ok := a.ResolveNodeID("N")
	if !ok {
		t.Fatal("expected to resolve N")
	}
	if id == 0 {
		t.Error("expected non-zero NodeID")
	}

	_, ok2 := a.ResolveNodeID("nonexistent")
	if ok2 {
		t.Error("expected false for nonexistent node")
	}
}
