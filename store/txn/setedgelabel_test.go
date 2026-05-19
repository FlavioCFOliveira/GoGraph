package txn

import (
	"errors"
	"testing"
)

// TestTx_SetEdgeLabel_AppliedOnCommit covers the SetEdgeLabel
// transactional buffer: the underlying SetEdgeLabel call happens
// only after Commit, and only when the edge has been added in the
// same transaction (or already exists in the store).
func TestTx_SetEdgeLabel_AppliedOnCommit(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openStore(t)
	defer cleanup()

	tx := s.Begin()
	if err := tx.AddEdge("alice", "bob"); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := tx.SetEdgeLabel("alice", "bob", "KNOWS"); err != nil {
		t.Fatalf("SetEdgeLabel: %v", err)
	}
	// Before commit the graph is untouched.
	if s.Graph().HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatal("uncommitted SetEdgeLabel must not be visible")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if !s.Graph().HasEdgeLabel("alice", "bob", "KNOWS") {
		t.Fatal("committed SetEdgeLabel did not reach the graph")
	}
}

// TestTx_SetEdgeLabel_AfterFinished verifies that calling
// SetEdgeLabel on a finished transaction returns ErrTxFinished
// without any side-effect on the underlying graph.
func TestTx_SetEdgeLabel_AfterFinished(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openStore(t)
	defer cleanup()

	tx := s.Begin()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if err := tx.SetEdgeLabel("a", "b", "KNOWS"); !errors.Is(err, ErrTxFinished) {
		t.Fatalf("SetEdgeLabel after Rollback returned %v, want ErrTxFinished", err)
	}
	if s.Graph().HasEdgeLabel("a", "b", "KNOWS") {
		t.Fatal("SetEdgeLabel after Rollback leaked through to the graph")
	}
}

// TestTx_SetEdgeLabel_NoEdge documents the apply-time contract: the
// underlying SetEdgeLabel is a no-op when the edge does not exist in
// the store at apply time. The transaction itself still commits.
func TestTx_SetEdgeLabel_NoEdge(t *testing.T) {
	t.Parallel()
	s, _, cleanup := openStore(t)
	defer cleanup()

	tx := s.Begin()
	if err := tx.SetEdgeLabel("ghost-src", "ghost-dst", "KNOWS"); err != nil {
		t.Fatalf("SetEdgeLabel: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if s.Graph().HasEdgeLabel("ghost-src", "ghost-dst", "KNOWS") {
		t.Fatal("SetEdgeLabel on missing edge must apply as no-op")
	}
}
