package exec_test

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	indexhash "github.com/FlavioCFOliveira/GoGraph/graph/index/hash"
)

func TestDropIndexOp_DropsIndex(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	_ = mgr.CreateIndex("drop_me", indexhash.New[string]())

	op := exec.NewDropIndexOp("drop_me", false, mgr, nil)
	if err := op.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	var row exec.Row
	ok, err := op.Next(&row)
	if err != nil {
		t.Fatalf("Next error: %v", err)
	}
	if ok {
		t.Fatal("DDL operator should emit no rows")
	}
	if err := op.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.GetIndex("drop_me"); err == nil {
		t.Fatal("index should be gone after DROP")
	}
}

func TestDropIndexOp_NotFound_Errors(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	op := exec.NewDropIndexOp("no_such_idx", false, mgr, nil)
	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err == nil {
		t.Fatal("expected error for missing index without IF EXISTS")
	}
}

func TestDropIndexOp_IfExists_Silent(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	op := exec.NewDropIndexOp("no_such_idx", true, mgr, nil)
	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err != nil {
		t.Fatalf("IF EXISTS should not error: %v", err)
	}
}
