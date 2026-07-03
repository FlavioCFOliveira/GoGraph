// Package cypher_alloc_test contains per-operator micro-benchmarks and
// allocation gate tests for the four hot-path Volcano operators:
// AllNodesScan, Filter, Project, and ResultSet.
//
// # Gate tests (TestZeroAlloc_*)
//
// Each gate test pre-initialises the operator tree outside testing.AllocsPerRun
// and measures a single Next() call inside the closure (TestZeroAlloc_ResultSet
// measures a Next+Record pair, since Record is where a real caller reads a
// row). This isolates per-call heap cost from the constant setup cost of
// Init/NewXxx.
//
// The gate walkers start at NodeID [gateAllocNodeIDStart] (>= 256), not 0:
// Go's runtime boxes a small integer (0-255) into an interface via a shared
// staticuint64s array with no heap allocation, so a gate confined to IDs < 256
// would report 0 allocs/op regardless of whether the operator actually
// allocates for realistic NodeIDs. This project's own
// BenchmarkAllNodesScan_PerNodeAllocCost (cypher/exec/scan_all_test.go)
// measures ~759 allocs/op on 1000 realistic NodeIDs — these gates previously
// used newWalker(200) (IDs 0-199, all inside the free range) and so were
// vacuous, always reporting 0 regardless of the operator's real cost
// (production-readiness audit finding P4).
//
// Expected allocs per Next() call on a realistic (>= 256) NodeID:
//
//	AllNodesScan: 1  — boxes the scanned NodeID into an expr.IntegerValue in
//	                   op.buf[0] (the single allocation site for this whole
//	                   pipeline; see AllNodesScan.Next in scan_all.go)
//	Filter:       1  — delegates to child; the predicate is stack-only, so
//	                   Filter contributes nothing beyond AllNodesScan's box
//	Project:      1  — projFirst is a pass-through (`return row[0], nil`), so
//	                   it forwards the already-boxed interface value from
//	                   AllNodesScan without a second allocation
//	ResultSet:    1  — Next forwards the same box; Record's map write
//	                   (map[string]interface{}[col] = row[i]) does not add a
//	                   second allocation, since widening an already-boxed
//	                   interface value and overwriting an existing map key
//	                   are both allocation-free (see TestZeroAlloc_ResultSet)
//
// # Benchmarks (Benchmark*)
//
// Full 500-node Init→drain→Close cycles with b.ReportAllocs() to surface
// regressions in CI via `go test -bench=. -benchmem`. gate500 (like the gate
// walkers above) starts at [gateAllocNodeIDStart], not 0: an ID range
// straddling the < 256 free small-int boxing threshold would let roughly
// half of each run's Next() calls skip the real boxing allocation, quietly
// deflating the allocs/op these benchmarks report to CI and
// docs/benchmarks.
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

// newWalker returns a staticWalker with n sequentially-numbered NodeIDs
// starting at 0.
func newWalker(n int) *staticWalker {
	return newWalkerFrom(0, n)
}

// gateAllocNodeIDStart is the first NodeID used by the TestZeroAlloc_* gates
// below. It must be >= 256: Go's runtime keeps a shared staticuint64s array
// for small integers 0-255, so boxing a NodeID in that range into an
// expr.Value interface never allocates, regardless of whether the exercised
// operator is actually zero-alloc. A gate walker confined to IDs < 256 (as
// these gates originally were, via newWalker(200)) would report 0 allocs/op
// even for a genuinely allocating operator — see BenchmarkAllNodesScan_
// PerNodeAllocCost in cypher/exec/scan_all_test.go, which measures ~759
// allocs/op for the identical operator on realistic NodeIDs (production-
// readiness audit finding P4).
const gateAllocNodeIDStart = 1000

// newWalkerFrom returns a staticWalker with n sequentially-numbered NodeIDs
// starting at start.
func newWalkerFrom(start, n int) *staticWalker {
	ids := make([]graph.NodeID, n)
	for i := range ids {
		ids[i] = graph.NodeID(start + i)
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
	gate500 *staticWalker // 500 realistic-NodeID nodes — used by Benchmark* functions
)

// TestMain seeds fixtures once and runs all tests.
func TestMain(m *testing.M) {
	gate10 = newWalker(10)
	gate500 = newWalkerFrom(gateAllocNodeIDStart, 500)
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

// TestZeroAlloc_AllNodesScan asserts that AllNodesScan.Next allocates at
// most 1 heap object per call after Init: reusing the fixed [1]expr.Value
// backing buffer itself costs nothing, but boxing the scanned NodeID into
// an expr.IntegerValue interface value does, for any NodeID >= 256 (see the
// package doc). The operator is pre-initialised on a 200-node graph outside
// the AllocsPerRun closure; only a single Next() call sits inside. 200 nodes
// gives 200 clean iterations before exhaustion.
func TestZeroAlloc_AllNodesScan(t *testing.T) {
	ctx := context.Background()
	w := newWalkerFrom(gateAllocNodeIDStart, 200)
	op := exec.NewAllNodesScan(w)
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = op.Close() })

	var row exec.Row
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = op.Next(&row)
	})

	// 1 alloc/op: boxing the scanned NodeID into op.buf[0]. The [1]expr.Value
	// backing buffer itself is reused across calls and costs nothing.
	if allocs > 1 {
		t.Errorf("AllNodesScan.Next: want <=1 alloc/op, got %.2f", allocs)
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

// TestZeroAlloc_FilterOp asserts that Filter.Next contributes nothing of its
// own beyond whatever its child allocates. The operator tree is
// pre-initialised outside AllocsPerRun; only one Next() call sits inside the
// measured closure.
func TestZeroAlloc_FilterOp(t *testing.T) {
	ctx := context.Background()
	w := newWalkerFrom(gateAllocNodeIDStart, 200)
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

	// 1 alloc/op: Filter.Next delegates to AllNodesScan.Next (1 alloc, the
	// NodeID boxing) and calls the predicate, which is stack-only. Budget:
	// <=1 allocs/op — Filter itself must add nothing on top of its child.
	if allocs > 1 {
		t.Errorf("Filter.Next: want <=1 alloc/op, got %.2f", allocs)
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
// object per call, and that the 1 it does allocate is not its own —
// projFirst is a pass-through ("return row[0], nil"), so op.outBuf[i] = v
// is a plain interface-to-interface copy (no allocation): the single
// allocation is the child's (AllNodesScan boxing the scanned NodeID),
// forwarded through Project unchanged. Project's own scratch state (the
// reused op.inputRow header) is stack/struct-resident and costs nothing.
// The operator tree is pre-initialised outside AllocsPerRun.
func TestZeroAlloc_Project(t *testing.T) {
	ctx := context.Background()
	w := newWalkerFrom(gateAllocNodeIDStart, 200)
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

	// 1 alloc/op: the NodeID box forwarded from AllNodesScan, unchanged.
	// Budget: <=1 allocs/op — Project itself must add nothing on top.
	if allocs > 1 {
		t.Errorf("Project.Next: want <=1 alloc/op, got %.2f", allocs)
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

// TestZeroAlloc_ResultSet asserts that a full ResultSet.Next + Record read
// cycle allocates at most 1 heap object per call — the same single
// allocation forwarded all the way from AllNodesScan (see
// TestZeroAlloc_AllNodesScan / TestZeroAlloc_Project), with nothing added by
// either Next or Record. The operator tree is pre-initialised via exec.Run
// outside AllocsPerRun; Record's lazily-allocated backing map is warmed up
// by one untimed Next+Record pair before the measured closure, matching the
// steady-state cost a real multi-row query pays after its first row.
//
// Record's map write does not cost a second allocation: fillCurrent (in
// produce_results.go) does `rs.current[col] = rs.curRow[i]`, writing an
// already-boxed expr.Value into an existing map[string]interface{} key.
// Converting an interface value to a wider interface type reuses its
// existing (type, data) word pair — it does not re-box the underlying
// concrete value — and overwriting an existing map key never grows the map.
// This was verified empirically (not assumed): an earlier version of this
// gate asserted <=2 allocs/op for a claimed "map re-box" allocation that
// TestZeroAlloc_ResultSet never actually measured (its closure called only
// Next, never Record); measuring the two separately confirmed a plain
// Next-only cycle and a Next+Record cycle both cost exactly 1 allocs/op —
// the same class of unverified-threshold gap this whole file's fix (audit
// finding P4) exists to close.
func TestZeroAlloc_ResultSet(t *testing.T) {
	ctx := context.Background()
	w := newWalkerFrom(gateAllocNodeIDStart, 200)
	scan := exec.NewAllNodesScan(w)
	filter := exec.NewFilter(scan, predTrue)
	proj, err := exec.NewProject(filter, []exec.ProjectionItem{projFirst})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	rs := exec.Run(ctx, proj, []string{"n"})
	t.Cleanup(func() { _ = rs.Close() })

	rs.Next()
	rs.Record()

	allocs := testing.AllocsPerRun(100, func() {
		rs.Next()
		rs.Record()
	})

	// 1 alloc/op: the NodeID box forwarded from AllNodesScan. Neither Next
	// nor Record adds anything of its own for this pass-through projection.
	if allocs > 1 {
		t.Errorf("ResultSet.Next+Record: want <=1 alloc/op, got %.2f", allocs)
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
