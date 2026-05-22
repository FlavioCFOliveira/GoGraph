package exec

import (
	"context"
	"errors"
	"testing"
)

func TestSingleRow_EmitsExactlyOneEmptyRow(t *testing.T) {
	op := NewSingleRowOperator()
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer func() {
		if err := op.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	var row Row
	ok, err := op.Next(&row)
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if !ok {
		t.Fatal("first Next: ok=false, want true")
	}
	if len(row) != 0 {
		t.Fatalf("first Next: row len=%d, want 0", len(row))
	}

	ok, err = op.Next(&row)
	if err != nil {
		t.Fatalf("second Next: %v", err)
	}
	if ok {
		t.Fatal("second Next: ok=true, want false (exhausted)")
	}
}

func TestSingleRow_InitResetsState(t *testing.T) {
	op := NewSingleRowOperator()
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init #1: %v", err)
	}
	var row Row
	if ok, _ := op.Next(&row); !ok {
		t.Fatal("first Init: Next returned false")
	}
	if ok, _ := op.Next(&row); ok {
		t.Fatal("first Init: Next returned true after exhaustion")
	}

	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init #2: %v", err)
	}
	ok, err := op.Next(&row)
	if err != nil {
		t.Fatalf("post-reinit Next: %v", err)
	}
	if !ok {
		t.Fatal("post-reinit Next: ok=false, want true (Init must reset done)")
	}
}

func TestSingleRow_NextHonoursCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	op := NewSingleRowOperator()
	if err := op.Init(ctx); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cancel()

	var row Row
	ok, err := op.Next(&row)
	if ok {
		t.Fatal("Next on cancelled ctx: ok=true, want false")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next on cancelled ctx: err=%v, want context.Canceled", err)
	}
}

func TestSingleRow_CloseIsNoop(t *testing.T) {
	op := NewSingleRowOperator()
	if err := op.Init(context.Background()); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close #1: %v", err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close #2 (must be idempotent): %v", err)
	}
}
