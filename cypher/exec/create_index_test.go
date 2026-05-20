package exec_test

import (
	"context"
	"testing"

	"go.uber.org/goleak"

	"gograph/cypher/exec"
	"gograph/graph/index"
)

func TestCreateIndexOp_Hash_CreatesIndex(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	op := exec.NewCreateIndexOp("my_idx", exec.ExecIndexHash, false, mgr)

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

	// Index must now be present.
	sub, err := mgr.GetIndex("my_idx")
	if err != nil {
		t.Fatalf("index not found after CREATE: %v", err)
	}
	if sub.Kind() != "hash" {
		t.Errorf("expected hash index, got %q", sub.Kind())
	}
}

func TestCreateIndexOp_BTree_CreatesIndex(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	op := exec.NewCreateIndexOp("btree_idx", exec.ExecIndexBTree, false, mgr)

	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err != nil {
		t.Fatalf("Next error: %v", err)
	}

	sub, _ := mgr.GetIndex("btree_idx")
	if sub.Kind() != "btree" {
		t.Errorf("expected btree index, got %q", sub.Kind())
	}
}

func TestCreateIndexOp_DuplicateErrors(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	createOne := func() {
		op := exec.NewCreateIndexOp("dup_idx", exec.ExecIndexHash, false, mgr)
		_ = op.Init(context.Background())
		var row exec.Row
		_, _ = op.Next(&row)
	}
	createOne()

	// Second create without IF NOT EXISTS must error.
	op := exec.NewCreateIndexOp("dup_idx", exec.ExecIndexHash, false, mgr)
	_ = op.Init(context.Background())
	var row exec.Row
	_, err := op.Next(&row)
	if err == nil {
		t.Fatal("expected error for duplicate index name")
	}
}

func TestCreateIndexOp_IfNotExists_Silent(t *testing.T) {
	defer goleak.VerifyNone(t)
	mgr := index.NewManager()
	first := exec.NewCreateIndexOp("x", exec.ExecIndexHash, false, mgr)
	_ = first.Init(context.Background())
	var row exec.Row
	_, _ = first.Next(&row)

	// Second create with IF NOT EXISTS must succeed silently.
	second := exec.NewCreateIndexOp("x", exec.ExecIndexHash, true, mgr)
	_ = second.Init(context.Background())
	_, err := second.Next(&row)
	if err != nil {
		t.Fatalf("IF NOT EXISTS should not error: %v", err)
	}
}
