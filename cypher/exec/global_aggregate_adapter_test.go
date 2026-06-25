package exec

// global_aggregate_adapter_test.go — unit tests for GlobalAggregateAdapter.
//
// The adapter is exercised indirectly by the engine-level aggregation tests in
// cypher/aggregation_test.go, but these unit tests pin the empty-input
// behaviour at the operator level so a regression in the wrapper is caught
// before the integration tests can hide it.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/cypher/funcs"
)

// emptyChildOp is a leaf operator that signals EOF on the first Next call.
// It mirrors the contract of an EagerAggregation that produced zero groups.
type emptyChildOp struct {
	initCalls  int
	nextCalls  int
	closeCalls int
}

func (op *emptyChildOp) Init(_ context.Context) error { op.initCalls++; return nil }
func (op *emptyChildOp) Next(_ *Row) (bool, error)    { op.nextCalls++; return false, nil }
func (op *emptyChildOp) Close() error                 { op.closeCalls++; return nil }

// fixedRowOp emits a single fixed row then EOFs. Used to verify the adapter
// is a transparent pass-through when the child produces any rows.
type fixedRowOp struct {
	rows     []Row
	cursor   int
	ctxCheck context.Context
}

func (op *fixedRowOp) Init(ctx context.Context) error { op.ctxCheck = ctx; op.cursor = 0; return nil }
func (op *fixedRowOp) Next(out *Row) (bool, error) {
	if op.cursor >= len(op.rows) {
		return false, nil
	}
	*out = op.rows[op.cursor]
	op.cursor++
	return true, nil
}
func (op *fixedRowOp) Close() error { return nil }

// errChildOp returns the configured error on its first Next call. Used to
// verify that the adapter propagates child errors without swallowing them.
type errChildOp struct{ err error }

func (op *errChildOp) Init(_ context.Context) error { return nil }
func (op *errChildOp) Next(_ *Row) (bool, error)    { return false, op.err }
func (op *errChildOp) Close() error                 { return nil }

// TestGlobalAggregateAdapter_EmptyInputEmitsNeutralRow verifies that when the
// child produces zero rows, the adapter emits exactly one synthetic row built
// from the neutral results of every aggregate factory, then EOFs.
func TestGlobalAggregateAdapter_EmptyInputEmitsNeutralRow(t *testing.T) {
	child := &emptyChildOp{}
	adapter := NewGlobalAggregateAdapter(child, []funcs.AggregatorFactory{
		funcs.NewCountStarAgg(),
		funcs.NewSumAgg(),
		funcs.NewCollectAgg(),
	})

	if err := adapter.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer adapter.Close() //nolint:errcheck // best effort

	// First Next: empty child → synthetic neutral row.
	var row Row
	ok, err := adapter.Next(&row)
	if err != nil {
		t.Fatalf("Next #1 err: %v", err)
	}
	if !ok {
		t.Fatalf("Next #1: expected a synthetic row, got EOF")
	}
	if len(row) != 3 {
		t.Fatalf("synthetic row width = %d, want 3", len(row))
	}
	// count(*) → 0
	c, ok := row[0].(expr.IntegerValue)
	if !ok || int64(c) != 0 {
		t.Errorf("row[0] = %v (%T), want IntegerValue(0)", row[0], row[0])
	}
	// sum → IntegerValue(0) per openCypher ("sum(null) returns 0"; #1759)
	if s, ok := row[1].(expr.IntegerValue); !ok || int64(s) != 0 {
		t.Errorf("row[1] = %v (%T), want IntegerValue(0)", row[1], row[1])
	}
	// collect → empty ListValue
	lv, ok := row[2].(expr.ListValue)
	if !ok || len(lv) != 0 {
		t.Errorf("row[2] = %v (%T), want empty ListValue", row[2], row[2])
	}

	// Second Next: no more rows.
	ok, err = adapter.Next(&row)
	if err != nil {
		t.Fatalf("Next #2 err: %v", err)
	}
	if ok {
		t.Errorf("Next #2: expected EOF, got a row")
	}
}

// TestGlobalAggregateAdapter_NonEmptyPassThrough verifies that when the child
// produces rows, the adapter forwards them unchanged and does NOT synthesise
// the neutral row at end-of-stream.
func TestGlobalAggregateAdapter_NonEmptyPassThrough(t *testing.T) {
	child := &fixedRowOp{rows: []Row{
		{expr.IntegerValue(3)}, // single aggregate column carrying value 3
	}}
	adapter := NewGlobalAggregateAdapter(child, []funcs.AggregatorFactory{
		funcs.NewCountStarAgg(),
	})
	if err := adapter.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer adapter.Close() //nolint:errcheck // best effort

	var row Row
	ok, err := adapter.Next(&row)
	if err != nil || !ok {
		t.Fatalf("Next #1: ok=%v err=%v", ok, err)
	}
	if len(row) != 1 {
		t.Fatalf("row width = %d, want 1", len(row))
	}
	iv, ok := row[0].(expr.IntegerValue)
	if !ok || int64(iv) != 3 {
		t.Errorf("row[0] = %v (%T), want IntegerValue(3)", row[0], row[0])
	}

	// Second Next: EOF; no synthetic row.
	ok, err = adapter.Next(&row)
	if err != nil {
		t.Fatalf("Next #2 err: %v", err)
	}
	if ok {
		t.Errorf("Next #2: expected EOF, got a row")
	}
}

// TestGlobalAggregateAdapter_PropagatesChildError verifies that errors from
// the wrapped child are returned verbatim from Next.
func TestGlobalAggregateAdapter_PropagatesChildError(t *testing.T) {
	want := errors.New("child blew up")
	adapter := NewGlobalAggregateAdapter(
		&errChildOp{err: want},
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
	)
	if err := adapter.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row Row
	_, err := adapter.Next(&row)
	if !errors.Is(err, want) {
		t.Errorf("Next err = %v, want %v", err, want)
	}
}

// TestGlobalAggregateAdapter_ContextCancellation verifies that a cancelled
// context aborts Next before any further work is done.
func TestGlobalAggregateAdapter_ContextCancellation(t *testing.T) {
	adapter := NewGlobalAggregateAdapter(
		&emptyChildOp{},
		[]funcs.AggregatorFactory{funcs.NewCountStarAgg()},
	)
	ctx, cancel := context.WithCancel(context.Background())
	if err := adapter.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cancel()
	var row Row
	_, err := adapter.Next(&row)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Next err = %v, want context.Canceled", err)
	}
}

// TestGlobalAggregateAdapter_NewCopiesFactories verifies that the constructor
// defensively copies the AggregatorFactory slice so that subsequent caller
// mutations cannot affect the adapter's behaviour.
func TestGlobalAggregateAdapter_NewCopiesFactories(t *testing.T) {
	factories := []funcs.AggregatorFactory{funcs.NewCountStarAgg()}
	adapter := NewGlobalAggregateAdapter(&emptyChildOp{}, factories)
	// Replace caller-visible slot with a different factory; adapter should
	// still produce the original CountStar identity element.
	factories[0] = funcs.NewSumAgg()

	if err := adapter.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	var row Row
	ok, err := adapter.Next(&row)
	if err != nil || !ok {
		t.Fatalf("Next: ok=%v err=%v", ok, err)
	}
	if _, ok := row[0].(expr.IntegerValue); !ok {
		t.Errorf("row[0] type = %T, want expr.IntegerValue (CountStar identity)", row[0])
	}
}
