package exec

// rowcount_hint_test.go — white-box tests for the optional row-count hint
// (#1720). The hint is an unexported contract between the operator tree and the
// materialise drain, so it is tested inside package exec.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// hintWalker is a minimal nodeWalker stub for the hint tests.
type hintWalker struct{ ids []graph.NodeID }

func (w *hintWalker) WalkNodeIDs(fn func(graph.NodeID) bool) {
	for _, id := range w.ids {
		if !fn(id) {
			return
		}
	}
}

func TestAllNodesScan_RowCountHint(t *testing.T) {
	op := NewAllNodesScan(&hintWalker{ids: []graph.NodeID{0, 1, 2, 3, 4}})

	// Before Init the slice is empty; the exact count is 0 and ok is true
	// (an empty scan is a valid, exact upper bound of zero).
	if n, ok := op.rowCountHint(); !ok || n != 0 {
		t.Fatalf("pre-Init hint = (%d, %v), want (0, true)", n, ok)
	}

	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	n, ok := op.rowCountHint()
	if !ok || n != 5 {
		t.Fatalf("post-Init hint = (%d, %v), want (5, true)", n, ok)
	}
}

func TestProject_RowCountHint_ForwardsChild(t *testing.T) {
	scan := NewAllNodesScan(&hintWalker{ids: []graph.NodeID{0, 1, 2}})
	proj, err := NewProject(scan, []ProjectionItem{{
		Alias: "n",
		Eval:  func(r Row) (expr.Value, error) { return r[0], nil },
	}})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if err := proj.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	n, ok := proj.rowCountHint()
	if !ok || n != 3 {
		t.Fatalf("Project hint = (%d, %v), want (3, true) forwarded from scan", n, ok)
	}
}

// nonHintOp is an operator that does NOT implement rowCountHinter, used to prove
// Project reports "unknown" when its child exposes no bound.
type nonHintOp struct{}

func (nonHintOp) Init(context.Context) error { return nil }
func (nonHintOp) Next(*Row) (bool, error)    { return false, nil }
func (nonHintOp) Close() error               { return nil }

func TestProject_RowCountHint_UnknownChild(t *testing.T) {
	proj, err := NewProject(nonHintOp{}, nil)
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	if n, ok := proj.rowCountHint(); ok {
		t.Fatalf("Project hint over non-hinting child = (%d, true), want ok=false", n)
	}
}

func TestResultSet_RowCountHint(t *testing.T) {
	scan := NewAllNodesScan(&hintWalker{ids: []graph.NodeID{0, 1, 2, 3}})
	proj, err := NewProject(scan, []ProjectionItem{{
		Alias: "n",
		Eval:  func(r Row) (expr.Value, error) { return r[0], nil },
	}})
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	rs := Run(context.Background(), proj, []string{"n"})
	defer func() { _ = rs.Close() }()

	n, ok := rs.RowCountHint()
	if !ok || n != 4 {
		t.Fatalf("ResultSet.RowCountHint = (%d, %v), want (4, true)", n, ok)
	}

	// The hint must be an upper bound on the rows actually drained.
	var drained int
	for rs.Next() {
		drained++
	}
	if err := rs.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if drained != n {
		t.Fatalf("drained %d rows, hint reported %d (must be exact for a full scan)", drained, n)
	}
}

func TestResultSet_RowCountHint_NoHinter(t *testing.T) {
	rs := Run(context.Background(), nonHintOp{}, nil)
	defer func() { _ = rs.Close() }()
	if n, ok := rs.RowCountHint(); ok {
		t.Fatalf("RowCountHint over non-hinting plan = (%d, true), want ok=false", n)
	}
}
