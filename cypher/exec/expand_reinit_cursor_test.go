package exec_test

// expand_reinit_cursor_test.go — regression coverage for the per-run state
// [exec.Expand.Init] failed to reset (found under rmp #2251, present at commit
// 35990293).
//
// # The defect
//
// Init is not called once per query. It is called once per OUTER ROW under a
// correlated Apply, so it must leave the operator in the state a fresh run
// expects. It reset op.srcID and op.fwdDone, but NOT the reverse cursor
// (revStart/revEnd) and NOT the CREATE-multiplicity pending queue
// (pendingRemaining/pendingRow).
//
// Neither is reset by anything else either: [exec.Expand.advanceRevEdge] gates on
// revStart < revEnd alone, and [exec.Expand.Next] emits from the pending queue and
// consults the reverse cursor BEFORE it pulls its first input row. So a run that
// inherited either one from an abandoned previous run emitted the PREVIOUS
// source's rows and attributed them to the next source — with op.srcID still -1,
// and with the input row still empty, so the emitted row was even the wrong WIDTH.
//
// # Why abandonment is the normal case, not an exotic one
//
// An EXISTS / SemiApply stops at the first match; a LIMIT stops at the n-th. The
// correlated Apply above then re-Inits for the next outer row. Any source whose
// run was not drained to exhaustion leaves a residue.
//
// Both tests below FAIL against the unmodified operator at 35990293 and pass
// after Init resets the state. TestRelTypeColumn_ExistsReverseLeaksPriorSource in
// package cypher is the engine-level counterpart.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// TestExpand_ReInitResetsReverseCursor drives the abandonment directly: pull ONE
// row of a two-slot reverse run, re-Init, and require the second run to see
// exactly the reverse edges of its own input rows and nothing else.
func TestExpand_ReInitResetsReverseCursor(t *testing.T) {
	ctx := context.Background()
	// Two arcs INTO node 2 (from 0 and from 1); node 3 has no incoming arc at all.
	fwd := buildCSR(4, [][2]int{{0, 2}, {1, 2}})
	rev := buildCSR(4, [][2]int{{2, 0}, {2, 1}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(2)}, exec.Row{expr.IntegerValue(3)})
	op := exec.NewExpand(input, exec.StaticAdjacency(fwd, rev, nil), exec.ExpandConfig{
		Direction: exec.DirIn,
		InputCol:  0,
	})

	// Run 1, ABANDONED after one row — node 2's reverse run has two slots, so one
	// is left unconsumed. This is what an EXISTS / SemiApply does.
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init 1: %v", err)
	}
	var row exec.Row
	ok, err := op.Next(&row)
	if err != nil {
		t.Fatalf("Next 1: %v", err)
	}
	if !ok {
		t.Fatal("run 1 emitted nothing, so the abandonment this test needs never happened")
	}

	// Run 2 — a fresh Init. It must behave exactly as if run 1 had never happened.
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init 2: %v", err)
	}
	rows, err := exec.Drain(ctx, op)
	if err != nil {
		t.Fatalf("Drain 2: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("run 2 emitted %d rows, want 2 (node 2 has two incoming arcs, node 3 none) — "+
			"a third row is the previous run's unconsumed reverse slot", len(rows))
	}
	for i, r := range rows {
		if len(r) != 4 {
			t.Errorf("row %d width = %d, want 4 (input col + src + edge + dst): %v — "+
				"a narrow row means the row was built with NO input row bound", i, len(r), r)
			continue
		}
		src, srcOK := r[1].(expr.IntegerValue)
		if !srcOK {
			t.Errorf("row %d src column is %T, want IntegerValue", i, r[1])
			continue
		}
		if src < 0 {
			t.Errorf("row %d has srcID = %d — the operator emitted an edge with NO source "+
				"loaded, i.e. before its first input row", i, int64(src))
		}
		if in, inOK := r[0].(expr.IntegerValue); inOK && int64(in) != int64(src) {
			t.Errorf("row %d attributes the edge to source %d while the input row says %d",
				i, int64(src), int64(in))
		}
	}
}

// TestExpand_ReInitResetsMultiplicityQueue is the same defect on the other piece
// of per-run state Init left behind: the CREATE-multiplicity pending queue.
//
// [exec.Expand.Next] emits from that queue BEFORE anything else, so a re-Init that
// inherits a non-empty queue republishes the previous run's row — verbatim,
// including its source and destination — as the first row of the new run.
func TestExpand_ReInitResetsMultiplicityQueue(t *testing.T) {
	ctx := context.Background()
	// One arc 0→1, recorded as if it had been CREATEd three times, so emitting it
	// once leaves two copies queued.
	fwd := buildCSR(3, [][2]int{{0, 1}})
	rev := buildCSR(3, [][2]int{{1, 0}})

	input := newSliceOperator(exec.Row{expr.IntegerValue(0)})
	op := exec.NewExpand(input, exec.StaticAdjacency(fwd, rev, nil), exec.ExpandConfig{
		Direction:      exec.DirOut,
		InputCol:       0,
		MultiplicityFn: func(_, _ uint64) int64 { return 3 },
	})

	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init 1: %v", err)
	}
	var row exec.Row
	ok, err := op.Next(&row)
	if err != nil {
		t.Fatalf("Next 1: %v", err)
	}
	if !ok {
		t.Fatal("run 1 emitted nothing, so no multiplicity was ever queued")
	}

	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init 2: %v", err)
	}
	rows, err := exec.Drain(ctx, op)
	if err != nil {
		t.Fatalf("Drain 2: %v", err)
	}
	// One arc × multiplicity 3 = exactly three rows for the single input row.
	if len(rows) != 3 {
		t.Errorf("run 2 emitted %d rows, want exactly 3 (one arc at multiplicity 3); "+
			"more means the previous run's queued copies were replayed into this one", len(rows))
	}
}
