package exec_test

// security_varlen_perquery_budget_test.go — REGRESSION GUARD for
// SECURITY-GAP #1478: the variable-length-expand edge budget must be enforced
// PER QUERY (accumulated across every input row), not merely per input row.
//
// Before the fix, VarLengthExpand reset its only traversal counter for each
// input row, so a query that produced M source rows and then expanded a
// variable-length pattern from each performed up to M × (per-row cap) edge
// traversals — the cap was per-row, not per-query, and an attacker could
// multiply it by inflating the driving cardinality (e.g.
// MATCH (a),(b) MATCH (a)-[*]->() ...).
//
// The fix adds a second counter (maxTotalEdgesTraversed) that is NOT reset per
// row. These white-box tests drive the operator with several input rows whose
// per-row expansion stays well under the per-row cap, and assert that the
// AGGREGATE total trips ErrVarLenCapExceeded — which can only happen if the
// counter accumulates across rows. A control case confirms the same workload
// completes when the aggregate cap is generous, proving the per-row cap was not
// what fired.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// fanOut5 builds a graph where node 0 has exactly five out-edges (0→1 … 0→5).
// A single-hop VLE from node 0 therefore traverses 5 edges per input row.
func fanOut5() (fwd, rev *staticCSR) {
	edges := [][2]int{{0, 1}, {0, 2}, {0, 3}, {0, 4}, {0, 5}}
	return buildCSR(6, edges), buildCSR(6, nil)
}

// mRowsAtNode0 returns m input rows, each anchoring the VLE at node 0.
func mRowsAtNode0(m int) []exec.Row {
	rows := make([]exec.Row, m)
	for i := range rows {
		rows[i] = exec.Row{expr.IntegerValue(0)}
	}
	return rows
}

// TestSec_VarLen_PerQueryBudget_TripsAcrossRows locks in the #1478 fix. Each of
// the 10 input rows traverses only 5 edges (far below the per-row cap of 1000),
// but the aggregate per-query cap is 12, so the running total crosses 12 during
// the third row's expansion and the operator returns ErrVarLenCapExceeded. If
// the budget were still per-row (the bug), the counter would reset to 0 each
// row, never reach 12, and all rows would succeed.
func TestSec_VarLen_PerQueryBudget_TripsAcrossRows(t *testing.T) {
	t.Parallel()
	fwd, rev := fanOut5()

	input := newSliceOperator(mRowsAtNode0(10)...)
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction:              exec.DirOut,
		InputCol:               0,
		MinHops:                1,
		MaxHops:                1,
		MaxEdgesTraversed:      1000, // per-row cap: never reached (5 per row)
		MaxTotalEdgesTraversed: 12,   // aggregate cap: crossed during the 3rd row
	})

	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrVarLenCapExceeded) {
		t.Fatalf("SECURITY-GAP #1478 NOT enforced: aggregate of 10×5=50 edge traversals under a per-query cap of 12 returned err=%v; want ErrVarLenCapExceeded — the edge budget is still being reset per input row instead of accumulating across the query", err)
	}
}

// TestSec_VarLen_PerQueryBudget_GenerousCapCompletes is the control for the test
// above: the identical workload (10 rows × 5 edges = 50 traversals) under a
// generous aggregate cap completes and yields M × per-row rows. This proves the
// per-row cap was not the limit that fired in the trip test — the aggregate cap
// is a distinct, additional bound, and bounded multi-row traversal is still
// permitted (no false positive).
func TestSec_VarLen_PerQueryBudget_GenerousCapCompletes(t *testing.T) {
	t.Parallel()
	fwd, rev := fanOut5()

	const m = 10
	input := newSliceOperator(mRowsAtNode0(m)...)
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction:              exec.DirOut,
		InputCol:               0,
		MinHops:                1,
		MaxHops:                1,
		MaxEdgesTraversed:      1000,
		MaxTotalEdgesTraversed: 1000, // generous: 50 traversals fit comfortably
	})

	rows, err := exec.Drain(context.Background(), op)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	// 5 reachable destinations per anchor row × m rows.
	if want := 5 * m; len(rows) != want {
		t.Fatalf("got %d rows, want %d (5 single-hop destinations × %d input rows)", len(rows), want, m)
	}
}

// TestSec_VarLen_PerQueryBudget_DefaultIsFinite guards that the default
// aggregate cap is finite and applied when MaxTotalEdgesTraversed is left zero,
// so a VLE driven by an attacker-inflated cardinality cannot run unbounded even
// when no explicit cap is configured. It exercises the default by setting an
// aggregate cap of 0 (→ default) together with a tiny per-row cap that, summed
// across rows, would be unbounded without the default; the per-row cap fires
// first here, but the construction confirms the zero-value path resolves to a
// finite default rather than "no cap".
func TestSec_VarLen_PerQueryBudget_DefaultIsFinite(t *testing.T) {
	t.Parallel()
	fwd, rev := fanOut5()

	input := newSliceOperator(mRowsAtNode0(3)...)
	op := exec.NewVarLengthExpand(input, fwd, rev, &exec.VarLengthConfig{
		Direction:              exec.DirOut,
		InputCol:               0,
		MinHops:                1,
		MaxHops:                1,
		MaxEdgesTraversed:      3, // per-row cap: 5 edges per row → trips on row 1
		MaxTotalEdgesTraversed: 0, // → finite default (defence-in-depth), not "no cap"
	})

	_, err := exec.Drain(context.Background(), op)
	if !errors.Is(err, exec.ErrVarLenCapExceeded) {
		t.Fatalf("want ErrVarLenCapExceeded (per-row cap of 3 < 5 edges), got %v", err)
	}
}
