package exec_test

// procedure_call_test.go — tests for ProcedureCallOp (task-301).

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/procs"
)

// ─────────────────────────────────────────────────────────────────────────────
// helpers
// ─────────────────────────────────────────────────────────────────────────────

// staticRows is an Operator that emits a pre-defined slice of rows.
type staticRows struct {
	rows []exec.Row
	idx  int
	ctx  context.Context //nolint:containedctx // test helper
}

func newStaticRows(rows ...exec.Row) *staticRows { return &staticRows{rows: rows} }

func (s *staticRows) Init(ctx context.Context) error {
	s.ctx = ctx
	s.idx = 0
	return nil
}

func (s *staticRows) Next(out *exec.Row) (bool, error) {
	if s.ctx.Err() != nil {
		return false, s.ctx.Err()
	}
	if s.idx >= len(s.rows) {
		return false, nil
	}
	*out = s.rows[s.idx]
	s.idx++
	return true, nil
}

func (s *staticRows) Close() error { return nil }

// drainProcOp drives op to exhaustion and returns all rows.
func drainProcOp(t *testing.T, op exec.Operator) []exec.Row {
	t.Helper()
	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() {
		if err := op.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	var rows []exec.Row
	for {
		var out exec.Row
		ok, err := op.Next(&out)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			break
		}
		cp := make(exec.Row, len(out))
		copy(cp, out)
		rows = append(rows, cp)
	}
	return rows
}

// makeReg creates a Registry with a single two-column procedure returning
// the given rows.
func makeReg(ns []string, name string, resultRows [][]expr.Value) *procs.Registry {
	reg := procs.NewRegistry()
	_ = reg.Register(procs.Signature{
		Namespace: ns,
		Name:      name,
		Outputs: []procs.NamedType{
			{Name: "col0", Kind: expr.KindString},
			{Name: "col1", Kind: expr.KindString},
		},
	}, func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
		return resultRows, nil
	})
	return reg
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Standalone CALL (child=nil) yields records
// ─────────────────────────────────────────────────────────────────────────────

func TestProcedureCallOp_Standalone(t *testing.T) {
	t.Parallel()
	resultRows := [][]expr.Value{
		{expr.StringValue("a"), expr.StringValue("1")},
		{expr.StringValue("b"), expr.StringValue("2")},
	}
	reg := makeReg([]string{"db"}, "indexes", resultRows)
	op := exec.NewProcedureCallOp([]string{"db"}, "indexes", nil, []string{"col0", "col1"}, nil, reg)

	rows := drainProcOp(t, op)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(rows))
	}
	if rows[0][0] != expr.StringValue("a") {
		t.Errorf("row[0][0] = %v, want a", rows[0][0])
	}
	if rows[1][1] != expr.StringValue("2") {
		t.Errorf("row[1][1] = %v, want 2", rows[1][1])
	}
}

func TestProcedureCallOp_Standalone_EmptyResult(t *testing.T) {
	t.Parallel()
	reg := makeReg([]string{"db"}, "labels", nil)
	op := exec.NewProcedureCallOp([]string{"db"}, "labels", nil, []string{"col0", "col1"}, nil, reg)
	rows := drainProcOp(t, op)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. YIELD restricts output columns (test that wider proc emits all cols;
//    YIELD filtering is a Projection concern)
// ─────────────────────────────────────────────────────────────────────────────

func TestProcedureCallOp_ThreeColumnProc_YieldsAll(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	_ = reg.Register(procs.Signature{
		Namespace: []string{"test"},
		Name:      "wide",
		Outputs: []procs.NamedType{
			{Name: "a", Kind: expr.KindString},
			{Name: "b", Kind: expr.KindString},
			{Name: "c", Kind: expr.KindString},
		},
	}, func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
		return [][]expr.Value{
			{expr.StringValue("x"), expr.StringValue("y"), expr.StringValue("z")},
		}, nil
	})
	// yieldVars names only a subset; ProcedureCallOp emits all 3 columns.
	op := exec.NewProcedureCallOp([]string{"test"}, "wide", nil, []string{"a", "b"}, nil, reg)
	rows := drainProcOp(t, op)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if len(rows[0]) != 3 {
		t.Errorf("expected 3 columns, got %d", len(rows[0]))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. Procedure raising an error propagates
// ─────────────────────────────────────────────────────────────────────────────

var errProcFailed = errors.New("proc: simulated failure")

func TestProcedureCallOp_ErrorPropagates(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	_ = reg.Register(procs.Signature{
		Namespace: []string{"test"},
		Name:      "failing",
	}, func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
		return nil, errProcFailed
	})
	op := exec.NewProcedureCallOp([]string{"test"}, "failing", nil, nil, nil, reg)
	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer op.Close() //nolint:errcheck // test cleanup
	var out exec.Row
	_, err := op.Next(&out)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, errProcFailed) {
		t.Errorf("error does not wrap errProcFailed: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 4. Unknown procedure returns error
// ─────────────────────────────────────────────────────────────────────────────

func TestProcedureCallOp_UnknownProcedure(t *testing.T) {
	t.Parallel()
	reg := procs.NewRegistry()
	op := exec.NewProcedureCallOp([]string{"db"}, "unknown", nil, nil, nil, reg)
	ctx := context.Background()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer op.Close() //nolint:errcheck // test cleanup
	var out exec.Row
	_, err := op.Next(&out)
	if err == nil {
		t.Fatal("expected ErrProcNotFound, got nil")
	}
	if !errors.Is(err, procs.ErrProcNotFound) {
		t.Errorf("error does not wrap ErrProcNotFound: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 5. Child-driving CALL: emits one batch per driving row
// ─────────────────────────────────────────────────────────────────────────────

func TestProcedureCallOp_ChildDriven(t *testing.T) {
	t.Parallel()
	// The procedure always returns two rows regardless of args.
	reg := procs.NewRegistry()
	_ = reg.Register(procs.Signature{
		Namespace: []string{"test"},
		Name:      "two",
		Outputs: []procs.NamedType{
			{Name: "v", Kind: expr.KindString},
		},
	}, func(_ context.Context, _ []expr.Value) ([][]expr.Value, error) {
		return [][]expr.Value{
			{expr.StringValue("row1")},
			{expr.StringValue("row2")},
		}, nil
	})

	// Build a child that emits 3 rows.
	child := newStaticRows(
		exec.Row{expr.StringValue("driver1")},
		exec.Row{expr.StringValue("driver2")},
		exec.Row{expr.StringValue("driver3")},
	)
	op := exec.NewProcedureCallOp([]string{"test"}, "two", nil, []string{"v"}, child, reg)
	rows := drainProcOp(t, op)
	// 3 driving rows × 2 procedure rows = 6 total rows.
	if len(rows) != 6 {
		t.Errorf("expected 6 rows, got %d", len(rows))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 6. Context cancellation
// ─────────────────────────────────────────────────────────────────────────────

func TestProcedureCallOp_ContextCancelled(t *testing.T) {
	t.Parallel()
	reg := makeReg([]string{"db"}, "labels", [][]expr.Value{
		{expr.StringValue("a"), expr.StringValue("b")},
	})
	op := exec.NewProcedureCallOp([]string{"db"}, "labels", nil, nil, nil, reg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()         // cancel immediately
	_ = op.Init(ctx) // Init may or may not check ctx; either result is acceptable
	var out exec.Row
	_, err := op.Next(&out)
	if err == nil {
		t.Fatal("expected context error, got nil")
	}
	_ = op.Close()
}
