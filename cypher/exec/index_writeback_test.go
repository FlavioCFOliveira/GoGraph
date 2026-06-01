package exec_test

// index_writeback_test.go — unit tests for IndexBuffer (task-276).
//
// Coverage:
//   - Commit fans all buffered changes to index.Manager.
//   - Rollback discards buffered changes without touching the manager.
//   - Commit with nil mgr does not panic.

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// spySubscriber records every Apply call for later inspection.
type spySubscriber struct {
	mu      sync.Mutex
	changes []index.Change
}

func (s *spySubscriber) Apply(c index.Change) {
	s.mu.Lock()
	s.changes = append(s.changes, c)
	s.mu.Unlock()
}

func (s *spySubscriber) Kind() string { return "spy" }

func (s *spySubscriber) snapshot() []index.Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]index.Change, len(s.changes))
	copy(out, s.changes)
	return out
}

func threeChanges() []index.Change {
	return []index.Change{
		{Op: index.OpAddNodeLabel, Node: graph.NodeID(1), Label: 10},
		{Op: index.OpSetNodeProperty, Node: graph.NodeID(2), Property: 5},
		{Op: index.OpRemoveNodeLabel, Node: graph.NodeID(3), Label: 20},
	}
}

// TestIndexBuffer_Commit verifies that Commit fans all buffered changes to
// every subscriber registered with the manager.
func TestIndexBuffer_Commit(t *testing.T) {
	mgr := index.NewManager()
	spy := &spySubscriber{}
	if err := mgr.CreateIndex("spy", spy); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	buf := &exec.IndexBuffer{}
	for _, c := range threeChanges() {
		buf.Enqueue(c)
	}
	if buf.Len() != 3 {
		t.Fatalf("expected Len 3 before Commit, got %d", buf.Len())
	}

	buf.Commit(mgr)

	if buf.Len() != 0 {
		t.Errorf("expected Len 0 after Commit, got %d", buf.Len())
	}
	got := spy.snapshot()
	want := threeChanges()
	if len(got) != len(want) {
		t.Fatalf("spy received %d changes, want %d", len(got), len(want))
	}
	for i, g := range got {
		w := want[i]
		if g.Op != w.Op || g.Node != w.Node || g.Label != w.Label || g.Property != w.Property {
			t.Errorf("change[%d]: got %+v, want %+v", i, g, w)
		}
	}
}

// TestIndexBuffer_Rollback verifies that Rollback discards changes without
// calling the manager.
func TestIndexBuffer_Rollback(t *testing.T) {
	mgr := index.NewManager()
	spy := &spySubscriber{}
	if err := mgr.CreateIndex("spy", spy); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	buf := &exec.IndexBuffer{}
	for _, c := range threeChanges() {
		buf.Enqueue(c)
	}

	buf.Rollback()

	if buf.Len() != 0 {
		t.Errorf("expected Len 0 after Rollback, got %d", buf.Len())
	}
	got := spy.snapshot()
	if len(got) != 0 {
		t.Errorf("spy received %d changes after Rollback, want 0", len(got))
	}
}

// TestIndexBuffer_CommitNilMgr verifies that Commit with a nil manager does
// not panic and resets the buffer.
func TestIndexBuffer_CommitNilMgr(t *testing.T) {
	buf := &exec.IndexBuffer{}
	for _, c := range threeChanges() {
		buf.Enqueue(c)
	}

	// Must not panic.
	buf.Commit(nil)

	if buf.Len() != 0 {
		t.Errorf("expected Len 0 after Commit(nil), got %d", buf.Len())
	}
}
