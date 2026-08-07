//go:build race || gograph_debug

package lpg

// reentrancy_multiwriter_test.go — the barrier guard with MORE THAN ONE writer
// in flight (rmp #2301, audit finding E16).
//
// The guard held ONE writer goroutine id, which was sound only because visMu
// serialised writers. Sprint 334 retires that exclusion, and a single field
// then reports false results in both directions:
//
//   - the second stampWriter OVERWRITES the first's id, so the first's clearing
//     compare-and-swap fails and strands a stale entry that never goes away;
//   - a genuine writer-to-writer re-entry on the overwritten goroutine is NOT
//     detected, because its id is no longer the one recorded.
//
// Both are verified below against the single-field build, where
// TestBarrierGuard_TwoWritersAreBothTracked fails with "writer 1 is no longer
// tracked while it is still inside its bracket".
//
// These tests drive barrierGuard directly rather than through
// [Graph.ApplyAtomically], because ApplyAtomically still takes visMu
// exclusively — retiring that is rmp #2304 — so two concurrent writers cannot
// yet be produced through the public API. The guard is the unit under test and
// it is what must be correct BEFORE the barrier goes, not after.

import (
	"sync"
	"testing"
)

// TestBarrierGuard_TwoWritersAreBothTracked is the core property: two
// goroutines inside a write bracket at the same time are both recorded, and
// each one's exit clears only its own entry.
func TestBarrierGuard_TwoWritersAreBothTracked(t *testing.T) {
	var bg barrierGuard
	bg.init()

	var wg sync.WaitGroup
	// bothIn releases each writer only once the other has stamped, so the two
	// brackets genuinely overlap rather than merely interleaving.
	bothIn := make(chan struct{})
	var once sync.Once
	stamped := make(chan int64, 2)

	tracked := make([]bool, 2)
	wg.Add(2)
	for w := 0; w < 2; w++ {
		go func(w int) {
			defer wg.Done()
			gid := bg.checkWriter()
			if gid == 0 {
				t.Errorf("writer %d: goroutine id unavailable", w)
				return
			}
			bg.stampWriter(gid)
			stamped <- gid
			// Wait until both are inside.
			if len(stamped) == 2 {
				once.Do(func() { close(bothIn) })
			}
			<-bothIn

			// While BOTH are inside, this writer must still be tracked: a
			// second stamp must not have displaced the first.
			bg.writerMu.Lock()
			_, ok := bg.writers[gid]
			bg.writerMu.Unlock()
			tracked[w] = ok

			bg.clearWriter(gid)
		}(w)
	}
	// Release the pair once both have stamped, in case neither observed len==2.
	go func() {
		g1 := <-stamped
		g2 := <-stamped
		stamped <- g1
		stamped <- g2
		once.Do(func() { close(bothIn) })
	}()
	wg.Wait()

	for w, ok := range tracked {
		if !ok {
			t.Errorf("writer %d is no longer tracked while it is still inside its bracket: "+
				"a single-id guard was overwritten by the other writer, so this writer's "+
				"re-entry would go undetected and its clear would strand a stale id", w)
		}
	}
	// Both exits must have emptied the set — no stale entry survives.
	bg.writerMu.Lock()
	left := len(bg.writers)
	bg.writerMu.Unlock()
	if left != 0 {
		t.Fatalf("%d writer id(s) stranded after both brackets closed, want 0", left)
	}
}

// TestBarrierGuard_ReentryDetectedWhileAnotherWriterIsInFlight is the
// enforcement half: the guard must still catch a genuine self-re-entry on one
// goroutine while a DIFFERENT goroutine is also inside a bracket.
//
// With a single id this is exactly the case that went undetected — the other
// writer's stamp had displaced this goroutine's — and an undetected
// writer-to-writer re-entry is a deadlock in production.
func TestBarrierGuard_ReentryDetectedWhileAnotherWriterIsInFlight(t *testing.T) {
	var bg barrierGuard
	bg.init()

	// THIS goroutine enters its bracket FIRST, and the other writer stamps
	// AFTER it. That order is the whole point: with a single id the later stamp
	// DISPLACES this goroutine's, which is precisely when the guard stops being
	// able to see this goroutine's own re-entry. Stamping in the other order
	// leaves this goroutine's id as the one recorded and the test passes
	// against the defective build — it was written that way first, and was not
	// discriminating until the emulation run showed it passing.
	gid := bg.checkWriter()
	if gid == 0 {
		t.Skip("goroutine id unavailable in this runtime build")
	}
	bg.stampWriter(gid)
	defer bg.clearWriter(gid)

	// The other writer, parked inside its bracket for the duration.
	otherIn := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		og := bg.checkWriter()
		bg.stampWriter(og)
		close(otherIn)
		<-release
		bg.clearWriter(og)
	}()
	<-otherIn
	defer func() { close(release); wg.Wait() }()

	// Re-entering must panic, even though another writer is in flight.
	defer func() {
		if recover() == nil {
			t.Error("a writer re-entered its own bracket and the guard did not panic, " +
				"while another writer was in flight: the nested acquisition would deadlock " +
				"on visMu with nothing reporting why")
		}
	}()
	bg.checkWriter()
}
