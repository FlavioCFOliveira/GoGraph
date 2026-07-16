package search

// defined_weight_validation_test.go — regression test for the production-
// readiness audit finding [D2] (rmp #2038).
//
// floatInvalid/anyFloatInvalid type-switched on the EXACT dynamic type
// (case float32:/case float64:), but the Weight constraint permits DEFINED
// types (type Cost float64) via its ~float32|~float64 arms. A defined float
// type matched neither case, so the NaN/±Inf gate was skipped and the
// algorithm returned a silently wrong result instead of ErrInvalidInput. The
// fix resolves the underlying kind via reflection for defined types while
// keeping every builtin weight (including integers) reflection-free.

import (
	"errors"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

type namedFloat float64 // a defined float weight type (underlying float64)
type namedInt int64     // a defined integer weight type (underlying int64)

// TestDefinedFloatWeight_NaNRejected: a defined float weight carrying NaN must
// trip the gate on Dijkstra and AStar, exactly like builtin float64. FAILS on
// the pre-fix code (silently returned a NaN distance / no error).
func TestDefinedFloatWeight_NaNRejected(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, namedFloat](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge(0->1): %v", err)
	}
	if err := a.AddEdge(1, 2, namedFloat(math.NaN())); err != nil {
		t.Fatalf("AddEdge(1->2): %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)
	dst, _ := a.Mapper().Lookup(2)

	if d, err := Dijkstra(c, src); !errors.Is(err, ErrInvalidInput) || d != nil {
		t.Fatalf("Dijkstra(namedFloat NaN): d=%v err=%v, want (nil, ErrInvalidInput)", d, err)
	}
	zeroH := func(graph.NodeID) namedFloat { return 0 }
	if p, cost, err := AStar(c, src, dst, zeroH); !errors.Is(err, ErrInvalidInput) || p != nil || cost != 0 {
		t.Fatalf("AStar(namedFloat NaN): p=%v cost=%v err=%v, want (nil, 0, ErrInvalidInput)", p, cost, err)
	}
}

// TestDefinedFloatWeight_InfRejected: the same for +Inf.
func TestDefinedFloatWeight_InfRejected(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, namedFloat](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 1.0); err != nil {
		t.Fatalf("AddEdge(0->1): %v", err)
	}
	if err := a.AddEdge(1, 2, namedFloat(math.Inf(+1))); err != nil {
		t.Fatalf("AddEdge(1->2): %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)

	if d, err := Dijkstra(c, src); !errors.Is(err, ErrInvalidInput) || d != nil {
		t.Fatalf("Dijkstra(namedFloat +Inf): d=%v err=%v, want (nil, ErrInvalidInput)", d, err)
	}
}

// TestDefinedIntWeight_SkipsGate is the control: a defined integer weight type
// must skip the float gate (no false positive, no per-element scan) and run to
// completion on a valid graph.
func TestDefinedIntWeight_SkipsGate(t *testing.T) {
	t.Parallel()
	a := adjlist.New[int, namedInt](adjlist.Config{Directed: true})
	if err := a.AddEdge(0, 1, 5); err != nil {
		t.Fatalf("AddEdge(0->1): %v", err)
	}
	if err := a.AddEdge(1, 2, 7); err != nil {
		t.Fatalf("AddEdge(1->2): %v", err)
	}
	c := csr.BuildFromAdjList(a)
	src, _ := a.Mapper().Lookup(0)

	d, err := Dijkstra(c, src)
	if err != nil {
		t.Fatalf("Dijkstra(namedInt): unexpected error %v", err)
	}
	dstID, _ := a.Mapper().Lookup(2)
	if got, ok := d.Distance(dstID); !ok || got != 12 {
		t.Fatalf("Dijkstra(namedInt) Distance(2) = %d (ok=%v), want 12", got, ok)
	}
}
