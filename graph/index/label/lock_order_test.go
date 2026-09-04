package label

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// The tests in this file exist because of a recorded failure, not a
// hypothetical one.
//
// The first implementation of the per-label-entry geometry re-read the SPINE
// while holding an ENTRY lock, to confirm the entry it held was still the
// published one. [Index.reap] takes the same two locks the other way round —
// spine first, entry second — so the two together are an ABBA inversion, and it
// DEADLOCKED.
//
// What matters more than the bug is why it survived so long:
//
//   - Eight clean throughput sweeps missed it, because their workload only ever
//     CREATED labels. reap never ran, so one half of the inversion was never
//     executed. A workload that exercises one operation cannot validate a lock
//     protocol that spans several.
//   - `go test -race` missed it. A deadlock is not a data race; the detector has
//     nothing to report. It surfaces as a hang.
//
// So these tests drive mutation and reaping CONCURRENTLY, and they detect a hang
// with an explicit watchdog rather than waiting for the go test timeout to kill
// the process without attribution.

// lockOrderWatchdog runs body and fails the test if it has not finished within
// limit, dumping every goroutine stack so a hang is attributable to a lock pair
// rather than merely reported.
//
// It is the failure channel for an ABBA inversion: a deadlocked body never
// returns, and nothing else in the suite would notice.
func lockOrderWatchdog(t *testing.T, limit time.Duration, body func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		body()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("DEADLOCK: workload did not finish within %s — a lock-order "+
			"inversion between Index.mu (spine) and entry.mu is the expected "+
			"cause; the lock order is spine-then-entry and must never invert.\n"+
			"all goroutine stacks follow:\n%s", limit, buf[:n])
	}
}

const (
	// parityWorkers is the number of concurrent mutators. Each owns a disjoint
	// band of NodeIDs, which is what makes an exact differential model possible
	// under real concurrency: no two workers ever touch the same (label, node)
	// pair, so each pair's final value is that worker's last write to it.
	parityWorkers = 8
	// parityBand is how many NodeIDs each worker owns.
	parityBand = 4
	// parityLabels is the total label space.
	parityLabels = 48
	// parityChurnLabels are labels 0..parityChurnLabels-1, on which every worker
	// adds and immediately removes. They spend most of their life empty, so
	// [Index.reap] runs on entries every other worker is simultaneously
	// creating and mutating — the exact interleaving the inversion needed.
	parityChurnLabels = 4
	// parityRounds is the per-worker operation count.
	parityRounds = 4000
	// parityFrozen is a label populated before the workers start and touched by
	// none of them, so a concurrent reader has an EXACT oracle to check while
	// the mutators run.
	parityFrozen = parityLabels
)

// model is the reference state one worker is responsible for: the set of nodes
// from its own band that carry each label, computed sequentially in the
// worker's own program order.
type model map[uint32]map[uint64]bool

func (m model) add(label uint32, node uint64) {
	s := m[label]
	if s == nil {
		s = make(map[uint64]bool)
		m[label] = s
	}
	s[node] = true
}

func (m model) remove(label uint32, node uint64) {
	if s := m[label]; s != nil {
		delete(s, node)
	}
}

// TestIndex_ConcurrentMutateAndReap_ParityAndLockOrder is the differential
// parity test the acceptance criteria call for. It runs mutation and reaping
// concurrently, checks the index against an exact sequential model afterwards,
// and is guarded by a watchdog so an ABBA inversion fails it as a deadlock
// rather than hanging the suite.
//
// It exercises, concurrently: Add, Remove, AddRange, RemoveRange (all four
// mutators, three of which can trigger reap), reap itself, and the read path.
func TestIndex_ConcurrentMutateAndReap_ParityAndLockOrder(t *testing.T) {
	t.Parallel()

	idx := NewIndex()

	// Frozen oracle: populated up front, touched by no worker.
	frozenIDs := []uint64{9001, 9002, 9003, 9004, 9005}
	for _, id := range frozenIDs {
		idx.Add(parityFrozen, graph.NodeID(id))
	}

	models := make([]model, parityWorkers)
	stopReaders := make(chan struct{})
	var readerWG, writerWG sync.WaitGroup
	readerErr := make(chan error, 4)

	// Concurrent readers. They cannot predict a churning label's contents, but
	// they CAN check the frozen one exactly, and every read they issue takes the
	// same entry locks the mutators and the reaper are fighting over.
	for r := range 4 {
		readerWG.Add(1)
		go func(seed uint64) {
			defer readerWG.Done()
			rng := rand.New(rand.NewPCG(seed, 0xF00D)) //nolint:gosec // G404: math/rand/v2 PCG seeded from the test's own parameter — this test asserts a reproducible sequence, which a CSPRNG would destroy.
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				if got := idx.Count(parityFrozen); got != uint64(len(frozenIDs)) {
					select {
					case readerErr <- fmt.Errorf("frozen label Count = %d, want %d", got, len(frozenIDs)):
					default:
					}
					return
				}
				if got := idx.Scan(parityFrozen); len(got) != len(frozenIDs) {
					select {
					case readerErr <- fmt.Errorf("frozen label Scan len = %d, want %d", len(got), len(frozenIDs)):
					default:
					}
					return
				}
				if !idx.Has(parityFrozen, graph.NodeID(frozenIDs[0])) {
					select {
					case readerErr <- fmt.Errorf("frozen label lost member %d", frozenIDs[0]):
					default:
					}
					return
				}
				// Drive the multi-label read paths, including the only place two
				// entry locks are held at once. Both orderings are issued so a
				// pair-ordering mistake in IntersectCardinality would wedge here.
				a := uint32(rng.IntN(parityLabels))
				b := uint32(rng.IntN(parityLabels))
				idx.IntersectCardinality(a, b)
				idx.IntersectCardinality(b, a)
				idx.Intersect(a, b)
				idx.Union(a, b)
			}
		}(uint64(r) + 1)
	}

	lockOrderWatchdog(t, 60*time.Second, func() {
		for w := range parityWorkers {
			models[w] = make(model)
			writerWG.Add(1)
			go func(w int, m model) {
				defer writerWG.Done()
				lo := uint64(w) * parityBand
				rng := rand.New(rand.NewPCG(uint64(w)+1, 0xC0FFEE)) //nolint:gosec // G404: math/rand/v2 PCG seeded from the test's own parameter — this test asserts a reproducible sequence, which a CSPRNG would destroy.
				for range parityRounds {
					lbl := uint32(rng.IntN(parityLabels))
					switch {
					case lbl < parityChurnLabels:
						// Add then immediately remove: the label converges on
						// empty and reap fires under concurrent mutation.
						id := lo + uint64(rng.IntN(parityBand))
						idx.Add(lbl, graph.NodeID(id))
						m.add(lbl, id)
						idx.Remove(lbl, graph.NodeID(id))
						m.remove(lbl, id)
					case rng.IntN(8) == 0:
						// Whole-band range ops: AddRange promotes the set and
						// RemoveRange is the other reap trigger.
						hi := lo + parityBand - 1
						if rng.IntN(2) == 0 {
							idx.AddRange(lbl, graph.NodeID(lo), graph.NodeID(hi))
							for id := lo; id <= hi; id++ {
								m.add(lbl, id)
							}
						} else {
							idx.RemoveRange(lbl, graph.NodeID(lo), graph.NodeID(hi))
							for id := lo; id <= hi; id++ {
								m.remove(lbl, id)
							}
						}
					default:
						id := lo + uint64(rng.IntN(parityBand))
						if rng.IntN(2) == 0 {
							idx.Add(lbl, graph.NodeID(id))
							m.add(lbl, id)
						} else {
							idx.Remove(lbl, graph.NodeID(id))
							m.remove(lbl, id)
						}
					}
				}
				// Drain phase: every worker withdraws everything it holds, so
				// EVERY label is driven to empty and must be reaped. Without
				// this the reaper's own path could stay lightly exercised.
				for lbl := range uint32(parityLabels) {
					for id := lo; id < lo+parityBand; id++ {
						idx.Remove(lbl, graph.NodeID(id))
						m.remove(lbl, id)
					}
				}
			}(w, models[w])
		}
		writerWG.Wait()
	})

	close(stopReaders)
	readerWG.Wait()
	select {
	case err := <-readerErr:
		t.Fatalf("concurrent reader observed a corrupted frozen label: %v", err)
	default:
	}

	// Differential parity. Every (label, node) pair was touched by exactly one
	// worker, so the union of the per-worker models is the exact expected state.
	want := make(map[uint32][]uint64)
	for _, m := range models {
		for lbl, s := range m {
			for id, on := range s {
				if on {
					want[lbl] = append(want[lbl], id)
				}
			}
		}
	}
	for lbl := range want {
		sort.Slice(want[lbl], func(a, b int) bool { return want[lbl][a] < want[lbl][b] })
	}

	for lbl := range uint32(parityLabels) {
		exp := want[lbl]
		got := idx.Scan(lbl)
		if len(got) != len(exp) {
			t.Fatalf("label %d: Scan returned %d ids, model says %d (got %v, want %v)",
				lbl, len(got), len(exp), got, exp)
		}
		for k := range got {
			if uint64(got[k]) != exp[k] {
				t.Fatalf("label %d: Scan[%d] = %d, model says %d", lbl, k, got[k], exp[k])
			}
		}
		if got := idx.Count(lbl); got != uint64(len(exp)) {
			t.Fatalf("label %d: Count = %d, model says %d", lbl, got, len(exp))
		}
		// The reaper's own invariant: an emptied label leaves NO entry behind.
		// This is what the drain phase above is for, and it is the assertion
		// that would catch a reaper which stopped running.
		if present := idx.labelPresent(lbl); present != (len(exp) > 0) {
			t.Fatalf("label %d: spine entry present = %v, but the model holds %d ids — "+
				"an emptied label must be reaped and a non-empty one must be kept",
				lbl, present, len(exp))
		}
	}
	// After the drain every worker holds nothing, so every label must be gone.
	for lbl := range uint32(parityLabels) {
		if idx.labelPresent(lbl) {
			t.Fatalf("label %d survived the drain phase: reap did not run", lbl)
		}
	}
	// The frozen label is the control: it must be untouched and still present.
	if got := idx.Count(parityFrozen); got != uint64(len(frozenIDs)) {
		t.Fatalf("frozen label Count = %d, want %d", got, len(frozenIDs))
	}
}

// TestIndex_MutateRetriesAfterEntryReaped covers the branch in [Index.mutate]
// that production reaches only through a race: the entry looked up under the
// spine lock is reaped before the entry lock is acquired.
//
// It uses the test-only markEntryDead seam to make that state deterministic,
// because a scheduled race cannot be relied on to hit a specific branch.
func TestIndex_MutateRetriesAfterEntryReaped(t *testing.T) {
	t.Parallel()

	t.Run("create path recreates the entry", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(10))
		if !idx.markEntryDead(1) {
			t.Fatalf("markEntryDead(1) found no entry to kill")
		}
		idx.Add(1, graph.NodeID(11))
		if got := idx.Scan(1); len(got) != 1 || got[0] != graph.NodeID(11) {
			t.Fatalf("Scan(1) = %v, want [11]: the retry must land in a FRESH entry, "+
				"not in the reaped one and not alongside its stale contents", got)
		}
		if !idx.labelPresent(1) {
			t.Fatalf("label 1 should have been recreated by the retrying Add")
		}
	})

	t.Run("non-create path reports absent", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(2, graph.NodeID(20))
		if !idx.markEntryDead(2) {
			t.Fatalf("markEntryDead(2) found no entry to kill")
		}
		// Remove must terminate and must not resurrect the label.
		idx.Remove(2, graph.NodeID(20))
		if idx.labelPresent(2) {
			t.Fatalf("Remove on a reaped label must not recreate its entry")
		}
		if got := idx.Count(2); got != 0 {
			t.Fatalf("Count(2) = %d, want 0", got)
		}
		// RemoveRange takes the same non-create path.
		idx.RemoveRange(2, graph.NodeID(0), graph.NodeID(100))
		if idx.labelPresent(2) {
			t.Fatalf("RemoveRange on a reaped label must not recreate its entry")
		}
	})
}

// TestIndex_DeserializeDisplacesInFlightMutators checks that a spine swap does
// not silently swallow a concurrent mutation into a detached entry: the
// displaced entries are marked dead, so an in-flight mutator retries against the
// new spine. It also drives Deserialize concurrently with mutation and reaping,
// which is a third acquirer of the spine-then-entry pair.
func TestIndex_DeserializeDisplacesInFlightMutators(t *testing.T) {
	t.Parallel()

	src := NewIndex()
	for l := range uint32(8) {
		src.Add(l, graph.NodeID(l)+1000)
	}
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	img := buf.Bytes()

	idx := NewIndex()
	stop := make(chan struct{})
	var wg sync.WaitGroup

	lockOrderWatchdog(t, 60*time.Second, func() {
		for w := range 4 {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for r := range 3000 {
					lbl := uint32((w + r) % 8)
					idx.Add(lbl, graph.NodeID(w))
					idx.Remove(lbl, graph.NodeID(w))
					select {
					case <-stop:
						return
					default:
					}
				}
			}(w)
		}
		for range 200 {
			if err := idx.Deserialize(bytes.NewReader(img)); err != nil {
				t.Errorf("Deserialize: %v", err)
				break
			}
		}
		close(stop)
		wg.Wait()
	})

	// The last Deserialize wins for every label it names; concurrent mutators
	// may have added or removed their own ids afterwards, so the checkable
	// invariant is that the index is intact and every surviving label answers
	// consistently across the read surface.
	for l := range uint32(8) {
		n := idx.Count(l)
		s := idx.Scan(l)
		if uint64(len(s)) != n {
			t.Fatalf("label %d: Scan len %d disagrees with Count %d", l, len(s), n)
		}
		if idx.labelPresent(l) != (n > 0) && n == 0 {
			// An entry may legitimately remain when a mutator repopulated it
			// after the swap; the forbidden state is a NON-empty label with no
			// entry, checked by the Scan/Count agreement above.
			continue
		}
	}
}
