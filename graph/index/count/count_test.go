package count

import (
	"sync"
	"testing"
)

func TestApply_EDCounts(t *testing.T) {
	s := New(0)
	// Two (:1)-[:10]->(:2) edges plus one (:1)-[:10]->(:3).
	apply := func(a, rt, b uint32) {
		s.Apply(EDelta(rt, 1))
		s.Apply(DDelta(a, rt, Out, 1))
		s.Apply(DDelta(b, rt, In, 1))
		s.Apply(TDelta(a, rt, b, 1))
	}
	apply(1, 10, 2)
	apply(1, 10, 2)
	apply(1, 10, 3)

	if got := s.CountE(10); got != 3 {
		t.Fatalf("E(10) = %d, want 3", got)
	}
	if got := s.CountD(1, 10, Out); got != 3 {
		t.Fatalf("D(1,10,OUT) = %d, want 3", got)
	}
	if got := s.CountD(2, 10, In); got != 2 {
		t.Fatalf("D(2,10,IN) = %d, want 2", got)
	}
	if got := s.CountD(3, 10, In); got != 1 {
		t.Fatalf("D(3,10,IN) = %d, want 1", got)
	}
	if got := s.CountT(1, 10, 2); got != 2 {
		t.Fatalf("T(1,10,2) = %d, want 2", got)
	}
	if got := s.CountT(1, 10, 3); got != 1 {
		t.Fatalf("T(1,10,3) = %d, want 1", got)
	}
	// Never-observed cells read as exact zero.
	if got := s.CountE(99); got != 0 {
		t.Fatalf("E(99) = %d, want 0", got)
	}
	if got := s.CountT(9, 9, 9); got != 0 {
		t.Fatalf("T(9,9,9) = %d, want 0", got)
	}
}

func TestApply_DeleteOnZeroFreesKey(t *testing.T) {
	s := New(0)
	s.Apply(EDelta(10, 1))
	s.Apply(TDelta(1, 10, 2, 1))
	s.Apply(EDelta(10, -1))
	s.Apply(TDelta(1, 10, 2, -1))

	if got := s.CountE(10); got != 0 {
		t.Fatalf("E(10) = %d, want 0 after cancel", got)
	}
	// The key must have been deleted (bounded growth), not left as a zero cell.
	sh := s.eShardOf(10)
	sh.mu.RLock()
	_, present := sh.e[10]
	sh.mu.RUnlock()
	if present {
		t.Fatalf("E(10) key still present after returning to zero")
	}
	tsh := s.tShardOf(triKey{1, 10, 2})
	tsh.mu.RLock()
	_, tpresent := tsh.t[triKey{1, 10, 2}]
	tsh.mu.RUnlock()
	if tpresent {
		t.Fatalf("T(1,10,2) key still present after returning to zero")
	}
}

func TestDirty_XScoped(t *testing.T) {
	s := New(0)
	if s.DDirty(7, In) || s.DDirty(7, Out) || s.TDirty(7, 7) {
		t.Fatalf("fresh store must be clean")
	}
	// Relabel of label 7 dirties the IN X-scoped cells only.
	s.MarkDirty(DirtyMark{Label: 7, Scope: DirtyDIn})
	s.MarkDirty(DirtyMark{Label: 7, Scope: DirtyTB})

	if !s.DDirty(7, In) {
		t.Fatalf("D(7,*,IN) should be dirty")
	}
	if s.DDirty(7, Out) {
		t.Fatalf("D(7,*,OUT) must stay clean (OUT-exact)")
	}
	// T is dirty when 7 is the b-position, clean when 7 is only the a-position
	// against an untouched b.
	if !s.TDirty(1, 7) {
		t.Fatalf("T(*,*,7) should be dirty via b-position")
	}
	if s.TDirty(7, 1) {
		t.Fatalf("T(7,*,1) must be clean: only b-position 7 was dirtied")
	}
	// RecomputeReset clears exactness.
	s.RecomputeReset()
	if s.DDirty(7, In) || s.TDirty(1, 7) {
		t.Fatalf("RecomputeReset must clear dirty flags")
	}
}

func TestRecomputeReset_ClearsCells(t *testing.T) {
	s := New(0)
	s.Apply(EDelta(10, 5))
	s.Apply(DDelta(1, 10, Out, 5))
	s.RecomputeReset()
	if s.CountE(10) != 0 || s.CountD(1, 10, Out) != 0 {
		t.Fatalf("RecomputeReset must clear all cells")
	}
}

// TestConcurrentReadsDuringSerialWrites exercises the documented contract:
// a single serialised writer with many concurrent readers must be race-free.
func TestConcurrentReadsDuringSerialWrites(t *testing.T) {
	s := New(0)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = s.CountE(10)
					_ = s.CountD(1, 10, Out)
					_ = s.CountT(1, 10, 2)
					_ = s.DDirty(1, In)
					_ = s.TDirty(1, 2)
				}
			}
		}()
	}
	// Single writer (the serialisation the engine barrier provides).
	for i := 0; i < 5000; i++ {
		s.Apply(EDelta(10, 1))
		s.Apply(TDelta(1, 10, 2, 1))
		s.Apply(EDelta(10, -1))
		s.Apply(TDelta(1, 10, 2, -1))
		if i%100 == 0 {
			s.MarkDirty(DirtyMark{Label: 1, Scope: DirtyDIn})
		}
	}
	close(stop)
	wg.Wait()
	if s.CountE(10) != 0 {
		t.Fatalf("E(10) = %d, want 0", s.CountE(10))
	}
}
