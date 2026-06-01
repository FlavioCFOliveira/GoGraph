// Package cypher_alloc_test contains per-operator micro-benchmarks and
// allocation gate tests for the four hot-path Volcano operators:
// AllNodesScan, Filter, Project, and ResultSet.
//
// # Gate tests (TestZeroAlloc_*)
//
// Each gate test pre-initialises the operator tree outside testing.AllocsPerRun
// and measures a single Next() call inside the closure. This isolates per-call
// heap cost from the constant setup cost of Init/NewXxx.
//
// Expected allocs per Next() call:
//
//	AllNodesScan: 0  — reuses a fixed [1]expr.Value backing buffer
//	Filter:       0  — delegates to child; predicate is stack-only
//	Project:      1  — boxes expr.IntegerValue into the expr.Value interface slot
//	ResultSet:    2  — Project boxing + re-box into Record map[string]interface{}
//
// # Benchmarks (Benchmark*)
//
// Full 500-node Init→drain→Close cycles with b.ReportAllocs() to surface
// regressions in CI via `go test -bench=. -benchmem`.
package cypher_alloc_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// ─────────────────────────────────────────────────────────────────────────────
// Test fixtures
// ─────────────────────────────────────────────────────────────────────────────

// staticWalker implements the exec.nodeWalker interface (internal to exec)
// using a fixed slice of NodeIDs. It is used instead of lpg.Graph to keep the
// bench package free of the lpg dependency and to make fixture construction
// trivial.
type staticWalker struct {
	ids []graph.NodeID
}

func (w *staticWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for _, id := range w.ids {
		if !fn(id) {
			return
		}
	}
}

// newWalker returns a staticWalker with n sequentially-numbered NodeIDs.
func newWalker(n int) *staticWalker {
	ids := make([]graph.NodeID, n)
	for i := range ids {
		ids[i] = graph.NodeID(i)
	}
	return &staticWalker{ids: ids}
}

// predTrue is a FilterFn that always passes every row.
func predTrue(row exec.Row) (expr.Value, error) { return expr.BoolValue(true), nil }

// projFirst is a ProjectionItem that returns the first column of the input row.
var projFirst = exec.ProjectionItem{
	Alias: "n",
	Eval:  func(row exec.Row) (expr.Value, error) { return row[0], nil },
}

// gate500 and gate10 are the shared walkers used by benchmarks and gate tests.
var (
	gate10  *staticWalker // 10 nodes — fast for AllocsPerRun
	gate500 *staticWalker // 500 nodes — used by Benchmark* functions
)

// TestMain seeds fixtures once and runs all tests.
func TestMain(m *testing.M) {
	gate10 = newWalker(10)
	gate500 = newWalker(500)
	os.Exit(m.Run())
}

// ─────────────────────────────────────────────────────────────────────────────
// Drain helper — drains an already-Init'd operator, returning row count.
// ─────────────────────────────────────────────────────────────────────────────

func drainOp(op exec.Operator) (int, error) {
	var (
		row   exec.Row
		count int
	)
	for {
		ok, err := op.Next(&row)
		if err != nil {
			return count, err
		}
		if !ok {
			break
		}
		count++
	}
	return count, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// AllNodesScan
// ─────────────────────────────────────────────────────────────────────────────

// TestZeroAlloc_AllNodesScan asserts that AllNodesScan.Next allocates nothing
// per call after Init. The operator is pre-initialised on a 200-node graph
// outside the AllocsPerRun closure; only a single Next() call sits inside.
// 200 nodes gives 200 clean iterations before exhaustion.
func TestZeroAlloc_AllNodesScan(t *testing.T) {
	ctx := context.Background()
	w := newWalker(200)
	op := exec.NewAllNodesScan(w)
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = op.Close() })

	var row exec.Row
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = op.Next(&row)
	})

	// AllNodesScan.Next reuses a fixed [1]expr.Value backing buffer and
	// performs no heap allocations after Init. The measured budget is 0.
	if allocs > 0 {
		t.Errorf("AllNodesScan.Next: want 0 allocs/op, got %.2f", allocs)
	}
}

// BenchmarkAllNodesScan measures the full Init→drain→Close cycle on 500 nodes.
// Warmup runs one full cycle before b.ResetTimer so that slice growth during
// Init's first pass does not inflate the measured allocation count.
func BenchmarkAllNodesScan(b *testing.B) {
	ctx := context.Background()

	// Warmup: one full pass so the nodeIDs backing slice is pre-sized.
	warmup := exec.NewAllNodesScan(gate500)
	_ = warmup.Init(ctx)
	_, _ = drainOp(warmup)
	_ = warmup.Close()

	b.ResetTimer()
	b.ReportAllocs()

	for range b.N {
		op := exec.NewAllNodesScan(gate500)
		if err := op.Init(ctx); err != nil {
			b.Fatal(err)
		}
		if _, err := drainOp(op); err != nil {
			b.Fatal(err)
		}
		_ = op.Close()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// FilterOp (exec.Filter)
// ─────────────────────────────────────────────────────────────────────────────

// TestZeroAlloc_FilterOp asserts that Filter.Next allocates nothing per call.
// The operator tree is pre-initialised outside AllocsPerRun; only one Next()
// call sits inside the measured closure.
func TestZeroAlloc_FilterOp(t *testing.T) {
	ctx := context.Background()
	w := newWalker(200)
	scan := exec.NewAllNodesScan(w)
	filter := exec.NewFilter(scan, predTrue)
	if err := filter.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = filter.Close() })

	var row exec.Row
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = filter.Next(&row)
	})

	// Filter.Next delegates to AllNodesScan.Next (0 alloc) and calls the
	// predicate (stack-only). Budget: 0 allocs/op.
	if allocs > 0 {
		t.Errorf("Filter.Next: want 0 allocs/op, got %.2f", allocs)
	}
}

// BenchmarkFilterOp wraps AllNodesScan with a pass-through Filter.
func BenchmarkFilterOp(b *testing.B) {
	ctx := context.Background()

	// Warmup.
	{
		s := exec.NewAllNodesScan(gate500)
		f := exec.NewFilter(s, predTrue)
		_ = f.Init(ctx)
		_, _ = drainOp(f)
		_ = f.Close()
	}

	b.ResetTimer()
	b.ReportAllocs()

	for range b.N {
		scan := exec.NewAllNodesScan(gate500)
		filter := exec.NewFilter(scan, predTrue)
		if err := filter.Init(ctx); err != nil {
			b.Fatal(err)
		}
		if _, err := drainOp(filter); err != nil {
			b.Fatal(err)
		}
		_ = filter.Close()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Project
// ─────────────────────────────────────────────────────────────────────────────

// TestZeroAlloc_Project asserts that Project.Next allocates at most 1 heap
// object per call. The operator tree is pre-initialised outside AllocsPerRun.
//
// Project.Next declares "var inputRow Row" — a nil slice header — and passes
// its address to the child. When AllNodesScan writes "*out = op.buf[:]" the
// slice header is stored on the Project stack frame. The child value row[0] is
// then copied into outBuf[0]. The expr.IntegerValue (an int64-based named type)
// must be boxed into the expr.Value interface slot in outBuf, which costs one
// allocation. This is the only expected per-call allocation.
func TestZeroAlloc_Project(t *testing.T) {
	ctx := context.Background()
	w := newWalker(200)
	scan := exec.NewAllNodesScan(w)
	filter := exec.NewFilter(scan, predTrue)
	proj, err := exec.NewProject(filter, []exec.ProjectionItem{projFirst})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := proj.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = proj.Close() })

	var row exec.Row
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = proj.Next(&row)
	})

	// 1 alloc/op: boxing expr.IntegerValue into the expr.Value interface slot
	// of Project.outBuf. The slice header for "var inputRow Row" is stack-
	// allocated.
	if allocs > 1 {
		t.Errorf("Project.Next: want ≤1 alloc/op, got %.2f", allocs)
	}
}

// BenchmarkProjectOp wraps AllNodesScan → Filter → Project.
func BenchmarkProjectOp(b *testing.B) {
	ctx := context.Background()
	items := []exec.ProjectionItem{projFirst}

	// Warmup.
	{
		s := exec.NewAllNodesScan(gate500)
		f := exec.NewFilter(s, predTrue)
		p, _ := exec.NewProject(f, items)
		_ = p.Init(ctx)
		_, _ = drainOp(p)
		_ = p.Close()
	}

	b.ResetTimer()
	b.ReportAllocs()

	for range b.N {
		scan := exec.NewAllNodesScan(gate500)
		filter := exec.NewFilter(scan, predTrue)
		proj, err := exec.NewProject(filter, items)
		if err != nil {
			b.Fatal(err)
		}
		if err := proj.Init(ctx); err != nil {
			b.Fatal(err)
		}
		if _, err := drainOp(proj); err != nil {
			b.Fatal(err)
		}
		_ = proj.Close()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// ResultSet (exec.Run)
// ─────────────────────────────────────────────────────────────────────────────

// TestZeroAlloc_ResultSet asserts that ResultSet.Next allocates at most 2 heap
// objects per call. The operator tree is pre-initialised via exec.Run outside
// AllocsPerRun; only one Next() call sits inside the closure.
//
// Breakdown of the 2 expected allocations:
//  1. Project.Next boxes expr.IntegerValue into outBuf[0] (see TestZeroAlloc_Project).
//  2. ResultSet.Next assigns row[0] (a boxed expr.Value) into the Record map
//     (type map[string]interface{}); the runtime re-boxes the interface value
//     into the interface{} map slot.
//
// The Record map itself is pre-allocated once in exec.Run and reused across
// every Next call, so no per-row map allocation occurs.
func TestZeroAlloc_ResultSet(t *testing.T) {
	ctx := context.Background()
	w := newWalker(200)
	scan := exec.NewAllNodesScan(w)
	filter := exec.NewFilter(scan, predTrue)
	proj, err := exec.NewProject(filter, []exec.ProjectionItem{projFirst})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	rs := exec.Run(ctx, proj, []string{"n"})
	t.Cleanup(func() { _ = rs.Close() })

	allocs := testing.AllocsPerRun(100, func() {
		rs.Next() //nolint:errcheck // gate test; error checked outside
	})

	// 2 allocs/op: (1) IntegerValue boxing in Project, (2) interface{} re-box
	// in the Record map assignment.
	if allocs > 2 {
		t.Errorf("ResultSet.Next: want ≤2 allocs/op, got %.2f", allocs)
	}
}

// BenchmarkResultSet measures the full pipeline: AllNodesScan → Filter →
// Project → ResultSet.Next on 500 nodes.
func BenchmarkResultSet(b *testing.B) {
	ctx := context.Background()
	items := []exec.ProjectionItem{projFirst}

	// Warmup.
	{
		s := exec.NewAllNodesScan(gate500)
		f := exec.NewFilter(s, predTrue)
		p, _ := exec.NewProject(f, items)
		rs := exec.Run(ctx, p, []string{"n"})
		for rs.Next() {
		}
		_ = rs.Close()
	}

	b.ResetTimer()
	b.ReportAllocs()

	for range b.N {
		scan := exec.NewAllNodesScan(gate500)
		filter := exec.NewFilter(scan, predTrue)
		proj, err := exec.NewProject(filter, items)
		if err != nil {
			b.Fatal(err)
		}
		rs := exec.Run(ctx, proj, []string{"n"})
		for rs.Next() {
		}
		if err := rs.Err(); err != nil {
			b.Fatal(err)
		}
		_ = rs.Close()
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Sanity check: verify fixture produces correct node count
// ─────────────────────────────────────────────────────────────────────────────

func TestFixture_WalkerNodeCount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		walker *staticWalker
		want   int
	}{
		{"gate10", gate10, 10},
		{"gate500", gate500, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			op := exec.NewAllNodesScan(tc.walker)
			if err := op.Init(ctx); err != nil {
				t.Fatalf("Init: %v", err)
			}
			n, err := drainOp(op)
			if err != nil {
				t.Fatalf("drain: %v", err)
			}
			_ = op.Close()
			if n != tc.want {
				t.Errorf("got %d nodes, want %d", n, tc.want)
			}
		})
	}
}

// TestNewProject_EmptyItems verifies the constructor accepts an empty
// items slice (e.g. WITH * over a pattern that binds no variables).
func TestNewProject_EmptyItems(t *testing.T) {
	scan := exec.NewAllNodesScan(gate10)
	proj, err := exec.NewProject(scan, nil)
	if err != nil {
		t.Fatalf("NewProject with empty items: unexpected error %v", err)
	}
	if proj == nil {
		t.Fatal("NewProject with empty items returned nil operator")
	}
}

// TestFilter_Pred verifies Filter passes only rows satisfying the predicate.
func TestFilter_Pred(t *testing.T) {
	// Predicate that rejects the first node (NodeID 0).
	rejectFirst := func(row exec.Row) (expr.Value, error) {
		iv, ok := row[0].(expr.IntegerValue)
		if !ok {
			return expr.BoolValue(false), fmt.Errorf("unexpected type %T", row[0])
		}
		return expr.BoolValue(int64(iv) > 0), nil
	}

	ctx := context.Background()
	scan := exec.NewAllNodesScan(gate10)
	filter := exec.NewFilter(scan, rejectFirst)
	if err := filter.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	n, err := drainOp(filter)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	_ = filter.Close()

	// 10 nodes, NodeID 0 is rejected → 9 rows expected.
	if n != 9 {
		t.Errorf("got %d rows, want 9", n)
	}
}
