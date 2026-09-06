package exec

// parallel_storage_accesses_test.go — the morsel-parallel leaves' db-hits figure
// (rmp #2762).
//
// The engine-level gate lives in cypher/profile_dbhits_honesty_test.go, where a
// real PROFILE of each leaf is compared against the serial plan for the same
// graph. This file is its white-box complement, and it exists for three things
// that gate cannot reach:
//
//  1. storageAccesses is UNEXPORTED, so only a package-internal test can read the
//     figure without going through the renderer. A gate that can only see the
//     rendered string cannot tell a counter that is wrong from a renderer that is.
//  2. The engine picks the morsel size (1024) and the worker budget. Here both are
//     forced: a morsel size of 7 over 4001 nodes is 572 morsels, so the per-morsel
//     atomic add is exercised hundreds of times across every worker the governor
//     grants, under -race. One add on one morsel would prove nothing about the
//     accumulation being exact under concurrency; 572 of them do.
//  3. The counter must start at zero and be MOVED by the drain. Each arm asserts
//     the before-value as well as the after-value, so an operator whose counter
//     was somehow pre-loaded could not pass.
//
// Every arm also carries a non-vacuity oracle in the same shape: the figure the
// leaf reports must differ from the rows it emitted. That is what makes each
// assertion a statement about a COUNTER rather than one that
// [StorageRecordScan]'s row derivation would satisfy just as well — and for two
// of the three leaves the row count is not even the same order of magnitude.

import (
	"context"
	"runtime"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// psaNodes is deliberately not a multiple of psaMorsel, so the final morsel is
// short and a counter that charged a fixed morsel size rather than the actual
// slice length would over-report by exactly one morsel's worth.
const (
	psaNodes  = 4001
	psaMorsel = 7
)

// psaForceWorkers raises GOMAXPROCS for the duration of one arm so the governor
// grants more than one worker and the shared counter is genuinely contended. A
// machine with fewer cores would otherwise run some arms single-threaded and the
// concurrency this file exists to exercise would silently not happen.
func psaForceWorkers(t *testing.T, n int) {
	t.Helper()
	prev := runtime.GOMAXPROCS(n)
	t.Cleanup(func() { runtime.GOMAXPROCS(prev) })
}

// TestParallelCountScan_StorageAccessesCountsEveryNodeReference drives the count
// leaf over many morsels and asserts it reports one access per node reference its
// workers consumed.
//
// The non-vacuity oracle is structural and total: a count leaf emits exactly ONE
// row whatever it walked, so rows=1 against dbhits=4001. No derivation from the
// row count could produce this figure, which is why the leaf never carried
// [StorageRecordScan] and why rmp #2762 had to count.
func TestParallelCountScan_StorageAccessesCountsEveryNodeReference(t *testing.T) {
	defer goleak.VerifyNone(t)
	psaForceWorkers(t, 8)

	op := NewParallelCountScan(newBudgetIDWalker(psaNodes), psaMorsel, nil)
	if before := op.storageAccesses(); before != 0 {
		t.Fatalf("storageAccesses() = %d before Init, want 0. The figure must be MOVED "+
			"by the scan; a non-zero start would make the assertion below pass without "+
			"anything having been counted.", before)
	}

	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ParallelCountScan emitted %d rows, want exactly 1", len(rows))
	}
	if got := rows[0][0]; got != expr.IntegerValue(psaNodes) {
		t.Fatalf("the count itself is %v, want %d — the fixture is wrong, so the "+
			"db-hits assertion below would be measured against the wrong graph",
			got, psaNodes)
	}
	if got := op.storageAccesses(); got != psaNodes {
		t.Errorf("storageAccesses() = %d, want %d — one per node reference the workers "+
			"consumed over %d morsels. The leaf's own count says it saw %d nodes, so a "+
			"different db-hits figure means the counter and the work have parted "+
			"company (rmp #2762).",
			got, psaNodes, (psaNodes+psaMorsel-1)/psaMorsel, psaNodes)
	}
	if op.storageAccesses() == int64(len(rows)) {
		t.Errorf("storageAccesses() equals the emitted row count (%d). This leaf emits "+
			"one row however many nodes it walked, so that would mean the figure is "+
			"derived from rows rather than counted.", len(rows))
	}
}

// TestParallelScanProject_StorageAccessesCountsScannedNotEmitted drives the fused
// scan leaf with a filter that admits half the nodes, so the figure it must report
// is the SCANNED count and not the emitted one.
//
// That is the whole point of the arm. The fused sub-plan carries the WHERE, which
// is exactly the shape the parallel scan exists for, so a leaf marked
// [StorageRecordScan] would have reported the admitted rows — a plausible number
// that is wrong by the selectivity of the predicate.
func TestParallelScanProject_StorageAccessesCountsScannedNotEmitted(t *testing.T) {
	defer goleak.VerifyNone(t)
	psaForceWorkers(t, 8)

	// budgetEvenFactory keeps the even NodeIDs: one row per two morsel nodes.
	wantRows := (psaNodes + 1) / 2

	op := NewParallelScanProject(newBudgetIDWalker(psaNodes), budgetEvenFactory, psaMorsel, nil)
	if before := op.storageAccesses(); before != 0 {
		t.Fatalf("storageAccesses() = %d before Init, want 0", before)
	}

	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(rows) != wantRows {
		t.Fatalf("the fused sub-plan emitted %d rows, want %d. The filter's selectivity "+
			"is what makes this arm discriminating, so a wrong row count means the arm "+
			"is not testing what it claims.", len(rows), wantRows)
	}
	if got := op.storageAccesses(); got != psaNodes {
		t.Errorf("storageAccesses() = %d, want %d — every node reference the workers "+
			"scanned, not the %d rows the predicate admitted (rmp #2762).",
			got, psaNodes, wantRows)
	}
	if op.storageAccesses() == int64(len(rows)) {
		t.Errorf("storageAccesses() equals the emitted row count (%d) on a scan of %d "+
			"nodes: the figure is tracking the filter's output, which is the exact "+
			"under-report a StorageRecordScan marker would have produced.",
			len(rows), psaNodes)
	}
}

// TestParallelAggregateScan_StorageAccessesCountsScannedNotGroups drives the
// aggregate leaf with a GROUP BY whose group count is far below the node count.
//
// Its row count is the number of GROUPS, so for this leaf a rows-derived figure is
// not merely imprecise but unrelated to the storage work: 100 groups over 4001
// nodes.
func TestParallelAggregateScan_StorageAccessesCountsScannedNotGroups(t *testing.T) {
	defer goleak.VerifyNone(t)
	psaForceWorkers(t, 8)

	const groups = 100
	// One pre-aggregation row per node: [group key, count argument].
	factory := func(ids []graph.NodeID) (Operator, error) {
		rows := make([]Row, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, Row{expr.IntegerValue(int64(id) % groups), expr.BoolValue(true)})
		}
		return &aggRowSlice{rows: rows}, nil
	}

	op := NewParallelAggregateScan(newBudgetIDWalker(psaNodes), factory, 1,
		[]AggReducerKind{ReduceCountStar}, psaMorsel, nil)
	if before := op.storageAccesses(); before != 0 {
		t.Fatalf("storageAccesses() = %d before Init, want 0", before)
	}

	rows, err := Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(rows) != groups {
		t.Fatalf("the aggregate emitted %d rows, want %d groups", len(rows), groups)
	}
	if got := op.storageAccesses(); got != psaNodes {
		t.Errorf("storageAccesses() = %d, want %d — every node reference the workers "+
			"scanned, not the %d groups they folded them into (rmp #2762).",
			got, psaNodes, groups)
	}
	if op.storageAccesses() == int64(len(rows)) {
		t.Errorf("storageAccesses() equals the group count (%d) rather than the %d "+
			"nodes scanned: the figure is tracking the aggregate's output.",
			len(rows), psaNodes)
	}
}

// TestParallelAggregateScan_StorageAccessesCountsTheInlinePathToo pins the arm the
// multi-worker test cannot reach: at a governor budget of one worker the operator
// short-circuits to [ParallelAggregateScan.runMorselsInline], which runs on the
// CALLING goroutine with no work channel and no goroutine at all (#2115).
//
// Both paths share runMorsel, which is where the charge is made, so the figure is
// path-independent by construction — but "by construction" is exactly the kind of
// claim that stops being true when someone moves the charge into runWorker. This
// asserts it instead.
func TestParallelAggregateScan_StorageAccessesCountsTheInlinePathToo(t *testing.T) {
	defer goleak.VerifyNone(t)

	factory := func(ids []graph.NodeID) (Operator, error) {
		rows := make([]Row, 0, len(ids))
		for range ids {
			rows = append(rows, Row{expr.BoolValue(true)})
		}
		return &aggRowSlice{rows: rows}, nil
	}

	// A single morsel clamps ParallelGovernor.Enter's budget to 1 whatever
	// GOMAXPROCS is, which is what selects the inline path.
	op := NewParallelAggregateScan(newBudgetIDWalker(psaNodes), factory, 0,
		[]AggReducerKind{ReduceCountStar}, 1<<20, nil)
	if _, err := Drain(context.Background(), op); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !op.ranInline {
		t.Fatalf("the operator did not take the budget==1 inline path, so this arm " +
			"covers the same code the multi-worker arm already does and proves nothing " +
			"extra")
	}
	if got := op.storageAccesses(); got != psaNodes {
		t.Errorf("storageAccesses() = %d on the inline path, want %d. The figure must "+
			"not depend on whether the governor spawned goroutines: a reader comparing "+
			"two PROFILEs cannot see the worker budget.", got, psaNodes)
	}
}
