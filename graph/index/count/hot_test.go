package count

// hot_test.go — the differential that guards the copy-on-write increment path
// (rmp #2682).
//
// # What changed, and what could therefore break
//
// [addCell] used to do everything under one EXCLUSIVE per-shard hold: look the
// cell up, add to it, and unlink it if it reached zero. That made the whole
// sequence indivisible for free, and it is why the store anti-scaled — one hot
// relationship type puts every KindE cell on one shard ([Store.eShardOf] keys on
// the relationship type alone), so every writer AND every reader queued on one
// mutex.
//
// The increment now runs under the SHARED hold and only the unlink takes the
// exclusive one, which splits the sequence in two. Between the two holds another
// writer can move the counter, and a second writer can decide to unlink the same
// key. The tier-2 re-read in [addCell] is what closes that, and this file is what
// proves the re-read is right: the concurrent aggregate must be BIT-IDENTICAL to
// the serial one, cell for cell, including whether the key survives at all.
//
// # Why the oracle is a serial replay rather than a hand-written expected value
//
// A hand-written total would only assert arithmetic. Replaying the same delta
// multiset serially through a second store asserts the thing that actually
// matters: that the concurrent path and the sequential path — the sequential path
// being unchanged by #2682 and therefore the current implementation's answer —
// agree on the count, on the store's footprint, and on the full cell snapshot.
// A lost delta, a spurious unlink, or a resurrected cell each break at least one
// of the three.

import (
	"fmt"
	"maps"
	"sync"
	"sync/atomic"
	"testing"
)

// hotRelType is the single relationship type every writer in this file hammers.
// One type means one shard, which is precisely the collapse #2682 is about: the
// test would be far weaker on a spread key set, because the shards would absorb
// the contention that the protocol has to survive.
const hotRelType = uint32(4242)

// hotScript builds the per-writer delta slices for one scenario.
//
// Each writer's slice oscillates: a run of +1 followed by a run of -1, repeated.
// The oscillation is the point — it drives the shared counter through zero over
// and over, so the tier-2 unlink and the tier-3 re-insert run thousands of times
// against live concurrent increments rather than once at the end.
//
// netPerWriter is added as a tail so the whole run has a known, non-trivial
// resting value; netPerWriter == 0 leaves the store empty, which is the strictest
// case because it requires the key to be UNLINKED and not merely zeroed.
func hotScript(writers, cycles, netPerWriter int) [][]int64 {
	out := make([][]int64, writers)
	for w := range out {
		s := make([]int64, 0, 2*cycles+abs(netPerWriter))
		for range cycles {
			s = append(s, 1, -1)
		}
		for range abs(netPerWriter) {
			if netPerWriter > 0 {
				s = append(s, 1)
			} else {
				s = append(s, -1)
			}
		}
		out[w] = s
	}
	return out
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// applySerial replays every writer's slice on one goroutine, in writer order.
// This is the oracle: the sequential path is what #2682 did not change.
func applySerial(script [][]int64) *Store {
	s := New(0)
	for _, slice := range script {
		for _, d := range slice {
			s.Apply(EDelta(hotRelType, d))
		}
	}
	return s
}

// applyConcurrent replays the same slices, one goroutine per writer, with no
// coordination of any kind.
func applyConcurrent(script [][]int64) *Store {
	s := New(0)
	var wg sync.WaitGroup
	wg.Add(len(script))
	for _, slice := range script {
		go func() {
			defer wg.Done()
			for _, d := range slice {
				s.Apply(EDelta(hotRelType, d))
			}
		}()
	}
	wg.Wait()
	return s
}

// keyPresent reports whether the E family still holds a cell for rt. It reads the
// published map directly, because the question is about the map's STRUCTURE —
// whether the key was unlinked — which CountE deliberately cannot distinguish
// from a zero-valued cell.
func keyPresent(s *Store, rt uint32) bool {
	sh := s.eShardOf(rt)
	sh.mu.lock()
	defer sh.mu.unlock()
	_, ok := sh.e.load()[rt]
	return ok
}

// TestCountStore_HotTypeConcurrentMatchesSerial is the differential required by
// rmp #2682: totals under concurrent writers driving ONE hot relationship type
// must be identical to the totals the same deltas produce serially.
//
// It asserts three things, not one. The count alone would pass a store that
// leaked a zero-valued cell on every crossing; Cells and the snapshot are what
// make the footprint claim ([Store.Cells], design §2.3) part of the differential
// too.
func TestCountStore_HotTypeConcurrentMatchesSerial(t *testing.T) {
	cases := []struct {
		name         string
		writers      int
		cycles       int
		netPerWriter int
	}{
		// Resting value zero: the strictest case. Every delta must land AND the
		// key must end up unlinked, so a lost increment and a leaked cell are
		// both visible.
		{name: "net-zero/16w", writers: 16, cycles: 2000, netPerWriter: 0},
		// Resting value non-zero: the key must survive with an exact total, so a
		// spurious unlink that discarded a live counter is visible.
		{name: "net-positive/16w", writers: 16, cycles: 2000, netPerWriter: 7},
		// A negative resting value must be RETAINED, not clamped (rmp #2303).
		{name: "net-negative/8w", writers: 8, cycles: 1500, netPerWriter: -5},
		// Wider than the host's core count, to force preemption inside the
		// two-hold window that tier 2 opens.
		{name: "net-zero/64w", writers: 64, cycles: 500, netPerWriter: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := hotScript(tc.writers, tc.cycles, tc.netPerWriter)

			want := applySerial(script)
			got := applyConcurrent(script)

			wantCount, gotCount := want.CountE(hotRelType), got.CountE(hotRelType)
			if gotCount != wantCount {
				t.Errorf("CountE(%d) = %d under %d concurrent writers, want %d from the identical "+
					"serial replay: the concurrent increment path lost or duplicated a delta, so the "+
					"aggregate is not the one a serial schedule produces",
					hotRelType, gotCount, tc.writers, wantCount)
			}

			wantCells, gotCells := want.Cells(), got.Cells()
			if gotCells != wantCells {
				t.Errorf("Cells() = %d under %d concurrent writers, want %d from the identical serial "+
					"replay: the delete-on-zero unlink did not converge, so the store's footprint "+
					"depends on the schedule (design §2.3)",
					gotCells, tc.writers, wantCells)
			}

			wantPresent, gotPresent := keyPresent(want, hotRelType), keyPresent(got, hotRelType)
			if gotPresent != wantPresent {
				t.Errorf("E(%d) key present = %v under %d concurrent writers, want %v from the "+
					"identical serial replay: a cell at exactly zero must be unlinked and a non-zero "+
					"cell must be retained, whatever the interleaving",
					hotRelType, gotPresent, tc.writers, wantPresent)
			}

			wantSnap, gotSnap := want.Snapshot(), got.Snapshot()
			if !maps.Equal(wantSnap.E, gotSnap.E) {
				t.Errorf("Snapshot().E = %v under %d concurrent writers, want %v from the identical "+
					"serial replay", gotSnap.E, tc.writers, wantSnap.E)
			}
		})
	}
}

// TestCountStore_HotTypeConcurrentAllFamiliesMatchSerial extends the differential
// to D and T on the same hot relationship type.
//
// E, D and T take three different key types through the same generic
// [addCell], and only E is exercised above. A protocol error in the shared
// routine would show in all three, but a mistake in wiring one family's table
// pointer would show in that family only — which is exactly what this catches.
func TestCountStore_HotTypeConcurrentAllFamiliesMatchSerial(t *testing.T) {
	const (
		writers = 16
		cycles  = 1000
		label   = uint32(3)
		labelB  = uint32(5)
	)

	run := func(concurrent bool) *Store {
		s := New(0)
		work := func() {
			for range cycles {
				for _, sign := range []int64{1, -1} {
					s.Apply(EDelta(hotRelType, sign))
					s.Apply(DDelta(label, hotRelType, Out, sign))
					s.Apply(DDelta(labelB, hotRelType, In, sign))
					s.Apply(TDelta(label, hotRelType, labelB, sign))
				}
			}
			// One surviving edge per writer, so every family rests non-zero.
			s.Apply(EDelta(hotRelType, 1))
			s.Apply(DDelta(label, hotRelType, Out, 1))
			s.Apply(DDelta(labelB, hotRelType, In, 1))
			s.Apply(TDelta(label, hotRelType, labelB, 1))
		}
		if !concurrent {
			for range writers {
				work()
			}
			return s
		}
		var wg sync.WaitGroup
		wg.Add(writers)
		for range writers {
			go func() {
				defer wg.Done()
				work()
			}()
		}
		wg.Wait()
		return s
	}

	want, got := run(false), run(true)

	checks := []struct {
		name       string
		want, have int64
	}{
		{"CountE", want.CountE(hotRelType), got.CountE(hotRelType)},
		{"CountD/out", want.CountD(label, hotRelType, Out), got.CountD(label, hotRelType, Out)},
		{"CountD/in", want.CountD(labelB, hotRelType, In), got.CountD(labelB, hotRelType, In)},
		{"CountT", want.CountT(label, hotRelType, labelB), got.CountT(label, hotRelType, labelB)},
	}
	for _, c := range checks {
		if c.have != c.want {
			t.Errorf("%s = %d under %d concurrent writers, want %d from the identical serial replay",
				c.name, c.have, writers, c.want)
		}
	}
	if got.Cells() != want.Cells() {
		t.Errorf("Cells() = %d under %d concurrent writers, want %d from the identical serial replay",
			got.Cells(), writers, want.Cells())
	}
}

// TestCountStore_HotTypeConcurrentReadersSeeNoTornCell drives lock-free readers
// against the unlink/re-insert churn the hot path now performs without holding
// them off.
//
// The read path takes NO lock (rmp #2682), so it can observe a cell that a writer
// unlinks a moment later. That is a legal snapshot read, but only for the values
// the aggregate can legally hold: with every writer applying +1 then -1 in lock
// step, no reachable state has E above the writer count, and none is below the
// negative of it. A reader that saw anything outside that band would have read a
// counter that was never installed — the failure a torn or resurrected map would
// produce. The reader also COUNTS what it saw, so a reader that observed nothing
// at all cannot pass as evidence.
func TestCountStore_HotTypeConcurrentReadersSeeNoTornCell(t *testing.T) {
	const (
		writers = 8
		readers = 8
		cycles  = 20000
	)
	s := New(0)

	var writersWG, readersWG sync.WaitGroup
	stop := make(chan struct{})
	bad := make(chan string, readers)
	var observations atomic.Int64
	var sawNonZero atomic.Int64

	readersWG.Add(readers)
	for range readers {
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				v := s.CountE(hotRelType)
				observations.Add(1)
				if v != 0 {
					sawNonZero.Add(1)
				}
				if v > writers || v < -writers {
					select {
					case bad <- fmt.Sprintf("CountE = %d, outside the reachable band [%d, %d]", v, -writers, writers):
					default:
					}
					return
				}
			}
		}()
	}

	writersWG.Add(writers)
	for range writers {
		go func() {
			defer writersWG.Done()
			for range cycles {
				s.Apply(EDelta(hotRelType, 1))
				s.Apply(EDelta(hotRelType, -1))
			}
		}()
	}

	writersWG.Wait()
	close(stop)
	readersWG.Wait()

	select {
	case msg := <-bad:
		t.Fatal(msg)
	default:
	}

	// An oracle that cannot fail proves nothing: assert the readers actually ran
	// against live churn rather than against a store that was already quiescent.
	if got := observations.Load(); got == 0 {
		t.Fatal("no reader observation was taken: the band check could not have failed")
	}
	if got := sawNonZero.Load(); got == 0 {
		t.Errorf("every one of the %d reader observations read exactly 0: the readers never "+
			"overlapped a live increment, so this run did not exercise the lock-free read path "+
			"against concurrent unlink/re-insert", observations.Load())
	}

	if got := s.CountE(hotRelType); got != 0 {
		t.Errorf("CountE = %d after every writer applied a net zero, want 0", got)
	}
	if keyPresent(s, hotRelType) {
		t.Errorf("E(%d) key survived a net-zero run: the cell must be unlinked at exactly zero", hotRelType)
	}
}
