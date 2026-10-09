package cypher

// profile_write_internal_test.go — the profiling wrapper is ABSENT from an
// unprofiled write build and present on every node of a profiled one (rmp #2790,
// keeping rmp #2222 AC 3 true on the write path).
//
// The assertion is structural: the built operator tree is walked through
// [exec.PlanChildren] and every node's concrete type is inspected, so a wrapper
// that leaked into an unprofiled build fails it whatever the wrapper does.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// operatorTypes returns the concrete type of every operator reachable from op,
// root first, descending through wrappers as well as plain operators.
func operatorTypes(op exec.Operator) []string {
	if op == nil {
		return nil
	}
	out := []string{fmt.Sprintf("%T", op)}
	if kids, ok := op.(exec.PlanChildren); ok {
		for _, c := range kids.PlanChildren() {
			out = append(out, operatorTypes(c)...)
		}
	}
	return out
}

func TestProfileWrite_WrapperIsAbsentFromAnUnprofiledWriteBuild(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	t.Cleanup(func() { _ = eng.Close() })
	if r, err := eng.RunInTx(context.Background(), "CREATE (:P {v: 1})", nil); err != nil {
		t.Fatalf("seed: %v", err)
	} else {
		_ = r.Close()
	}

	const q = "MATCH (n:P) SET n.w = 2 CREATE (n)-[:R]->(:Q) RETURN n.v AS v"
	entry, _, err := eng.parseAndAnalyse(q)
	if err != nil || entry.semaErr != nil {
		t.Fatalf("parse %q: %v %v", q, err, entry.semaErr)
	}

	build := func(prof *exec.Profiler) []string {
		t.Helper()
		snap := g.BeginRead()
		defer g.EndRead(snap)
		rv := g.ReadAt(snap)
		la := &lpgMutatorAdapter{g: g, eng: eng}
		la.counters = &la.countersStore
		la.buf, la.undo = &la.bufStore, &la.undoStore
		la.conTxn = &la.conTxnStore
		op, _, err := buildPlanWithMutatorFull(entry.plan, &lpgNodeWalker{g: rv}, &lpgLabelResolver{g: rv},
			eng.reg, nil, la, nil, nil, 0, nil, planGates{}, nil, prof)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return operatorTypes(op)
	}

	plain := build(nil)
	for _, typ := range plain {
		if strings.Contains(typ, "profiled") {
			t.Fatalf("an UNPROFILED write build contains the profiling wrapper %s: %v", typ, plain)
		}
	}
	// Non-vacuity: the write operators are really in the walked tree.
	joined := strings.Join(plain, ",")
	for _, want := range []string{"SetProperty", "CreateNode", "CreateRelationship"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the walk did not reach %s, so the absence above proves nothing: %v", want, plain)
		}
	}

	profiled := build(exec.NewProfiler())
	wrappers := 0
	for _, typ := range profiled {
		if strings.Contains(typ, "profiled") {
			wrappers++
		}
	}
	// Every operator of the unprofiled build is wrapped exactly once in the
	// profiled one. A wrapper reports the CHILDREN of the operator it wraps, so
	// the profiled walk visits one wrapper per operator and nothing else.
	if wrappers != len(plain) || len(profiled) != len(plain) {
		t.Errorf("profiled build walks %d nodes, %d of them wrappers; want exactly one wrapper per "+
			"operator of the unprofiled build (%d):\n  unprofiled %v\n  profiled   %v",
			len(profiled), wrappers, len(plain), plain, profiled)
	}
}
