package index

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

type spySub struct {
	name    string
	applied atomic.Int64
}

func (s *spySub) Apply(_ Change) { s.applied.Add(1) }
func (s *spySub) Kind() string   { return "spy" }
func (s *spySub) Count() int64   { return s.applied.Load() }

func TestManager_CreateGetDrop(t *testing.T) {
	t.Parallel()
	m := NewManager()
	s := &spySub{name: "primary"}
	if err := m.CreateIndex("primary", s); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err := m.CreateIndex("primary", s); !errors.Is(err, ErrIndexExists) {
		t.Fatalf("duplicate should fail with ErrIndexExists, got %v", err)
	}
	got, err := m.GetIndex("primary")
	if err != nil || got != s {
		t.Fatalf("GetIndex: %v %v", got, err)
	}
	if err := m.DropIndex("primary"); err != nil {
		t.Fatalf("DropIndex: %v", err)
	}
	if err := m.DropIndex("primary"); !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("dropping missing index should fail with ErrIndexNotFound, got %v", err)
	}
	if m.Count() != 0 {
		t.Fatalf("Count = %d, want 0", m.Count())
	}
}

func TestManager_FanOut(t *testing.T) {
	t.Parallel()
	m := NewManager()
	a := &spySub{name: "a"}
	b := &spySub{name: "b"}
	if err := m.CreateIndex("a", a); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateIndex("b", b); err != nil {
		t.Fatal(err)
	}
	m.Apply(Change{Op: OpAddNodeLabel, Node: graph.NodeID(1), Label: 7})
	m.Apply(Change{Op: OpSetNodeProperty, Node: graph.NodeID(2), Property: 3, NewValue: "x"})
	if a.Count() != 2 || b.Count() != 2 {
		t.Fatalf("fan-out counts: a=%d b=%d, want 2/2", a.Count(), b.Count())
	}
}

func TestManager_ListIndexes(t *testing.T) {
	t.Parallel()
	m := NewManager()
	if err := m.CreateIndex("x", &spySub{}); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateIndex("y", &spySub{}); err != nil {
		t.Fatal(err)
	}
	got := m.ListIndexes()
	if len(got) != 2 {
		t.Fatalf("ListIndexes = %v", got)
	}
}

func TestManager_ApplyBatch(t *testing.T) {
	t.Parallel()
	m := NewManager()
	a := &spySub{}
	b := &spySub{}
	_ = m.CreateIndex("a", a)
	_ = m.CreateIndex("b", b)
	changes := []Change{
		{Op: OpAddNodeLabel, Node: graph.NodeID(1), Label: 1},
		{Op: OpAddNodeLabel, Node: graph.NodeID(2), Label: 1},
		{Op: OpAddNodeLabel, Node: graph.NodeID(3), Label: 1},
	}
	m.ApplyBatch(changes)
	if a.Count() != 3 || b.Count() != 3 {
		t.Fatalf("batch fan-out: a=%d b=%d, want 3/3", a.Count(), b.Count())
	}
}

func TestManager_ConcurrentApplyAndAdminDoNotBlockReaders(t *testing.T) {
	t.Parallel()
	m := NewManager()
	for k := 0; k < 4; k++ {
		_ = m.CreateIndex(string(rune('a'+k)), &spySub{})
	}
	var wg sync.WaitGroup
	const writers = 32
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < 1024; i++ {
				m.Apply(Change{Op: OpAddNodeLabel, Node: graph.NodeID(uint64(i)), Label: 1})
			}
		}()
	}
	wg.Wait()
	for _, name := range m.ListIndexes() {
		sub, _ := m.GetIndex(name)
		s, _ := sub.(*spySub)
		if s.Count() != int64(writers*1024) {
			t.Fatalf("subscriber %s count = %d, want %d", name, s.Count(), writers*1024)
		}
	}
}

func TestChange_IsEdgeChange(t *testing.T) {
	t.Parallel()
	if !(Change{Op: OpSetEdgeProperty}).IsEdgeChange() {
		t.Fatalf("OpSetEdgeProperty must be edge")
	}
	if (Change{Op: OpAddNodeLabel}).IsEdgeChange() {
		t.Fatalf("OpAddNodeLabel must not be edge")
	}
}
