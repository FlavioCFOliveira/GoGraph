package count

import (
	"sync"
	"testing"
	"unsafe"
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
	//
	// The map is now the copy-on-write one the shard publishes, and it is read
	// under the EXCLUSIVE lock rather than the shared one: an increment holds the
	// shared lock (see [addCell]), so a shared hold here would no longer freeze
	// the shard. The assertion itself is unchanged — the key must be absent.
	sh := s.eShardOf(10)
	sh.mu.lock()
	_, present := sh.e.load()[10]
	sh.mu.unlock()
	if present {
		t.Fatalf("E(10) key still present after returning to zero")
	}
	tsh := s.tShardOf(triKey{1, 10, 2})
	tsh.mu.lock()
	_, tpresent := tsh.t.load()[triKey{1, 10, 2}]
	tsh.mu.unlock()
	if tpresent {
		t.Fatalf("T(1,10,2) key still present after returning to zero")
	}
}

// TestShard_LayoutSeparatesReadersFromTheLock pins the two layout properties the
// store's scaling rests on. Neither is expressible as a single size any more, so
// this test replaced TestShard_LayoutOneCacheLine, which asserted
// unsafe.Sizeof(shard{}) == cacheLine and could not survive the shard growing a
// per-P slot array.
//
// MEASURED, because the obvious claim is false: `Store` has an alignment of 8,
// not 128, so `&shards[0]` is NOT line-aligned — over 200 heap-allocated Stores
// it landed at offset 8 within the line 200/200 times. No shard ever occupies
// exactly one cache line. What actually prevents false sharing is that the STRIDE
// is a whole number of lines while each hot region is far smaller than one, so
// consecutive shards' hot fields are always a multiple of 128 bytes apart and
// therefore never share a line, whatever the base offset.
//
// The two properties:
//
//  1. the shard stride is a whole number of cache lines, so shard i's fields
//     never share a line with shard i+1's;
//  2. the four published-map pointers — which EVERY [Store.CountE] loads — are a
//     full line away from the lock's hot words. When they shared a line, every
//     shared acquire's read-modify-write invalidated the readers' line; moving
//     the lock off it was worth 1.22x on the hot mixed workload by itself.
func TestShard_LayoutSeparatesReadersFromTheLock(t *testing.T) {
	t.Parallel()
	if got := unsafe.Sizeof(shard{}); got%cacheLine != 0 {
		t.Fatalf("unsafe.Sizeof(shard{}) = %d, want a multiple of %d: the shard stride is no "+
			"longer a whole number of cache lines, so two shards' hot fields can land in one "+
			"line and writers to DIFFERENT shards false-share", got, cacheLine)
	}
	readEnd := unsafe.Offsetof(shard{}.t) + unsafe.Sizeof(shard{}.t)
	lockAt := unsafe.Offsetof(shard{}.mu)
	if lockAt/cacheLine == (readEnd-1)/cacheLine {
		t.Fatalf("the lock at offset %d shares cache line %d with the published-map pointers "+
			"(which end at offset %d): every shared acquire will invalidate the line every "+
			"concurrent reader loads the map pointer from", lockAt, lockAt/cacheLine, readEnd)
	}
	// One slot per core, each owning a whole line, is the whole point of the
	// readers-biased lock: two cores taking the shared hold must not meet.
	if got := unsafe.Sizeof(rbSlot{}); got != cacheLine {
		t.Fatalf("unsafe.Sizeof(rbSlot{}) = %d, want %d: two cores' shared-hold counters would "+
			"share a line, which reproduces the defect the slot array exists to remove", got, cacheLine)
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

// TestConcurrentReadsDuringSerialWrites drives many concurrent readers against
// ONE writer. Single-writer is this test's own CONSTRUCTION, not the store's
// contract: since rmp #2320 the engine's barrier is held SHARED and two writers
// mutate the store concurrently, which the package "Concurrency contract" section
// states and which hot_test.go and commutative_test.go exercise. What this test
// pins is the narrower thing — the lock-free read path stays race-free while a
// writer churns cells through create and unlink.
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
	// One writer. NOT the serialisation the engine barrier provides — it provides
	// none, and has not since rmp #2320 — but a deliberate isolation of the reader
	// path from writer-versus-writer interleaving, which hot_test.go covers.
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
