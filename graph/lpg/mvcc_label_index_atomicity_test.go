package lpg

// mvcc_label_index_atomicity_test.go — rmp #2326: the label bag and the label
// index transition together.
//
// Layer: short.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestLabelIndex_NeverMissesALabelTheBagHas pins the UNRECOVERABLE direction.
//
// # The window this closes
//
// Graph.setNodeLabelInfo used to write the bag under the shard lock, RELEASE it,
// and only then add to the label bitmap. Between the two, the bag says the label
// is PRESENT and the bitmap does not contain the node — and a present-time reader
// takes the raw bitmap whenever [Graph.labelBitmapNeedsFilter] is false, so it
// misses the node entirely. That is a LOST ROW, the direction the candidate-set
// discipline says nothing can recover from; the removal path had the mirror-image
// window, which over-reports instead.
//
// Before rmp #2308 the sweep ran under the visibility barrier and neither window
// was observable. With the vacuum on its own goroutine they are.
//
// The assertion is the implication that must hold at EVERY instant: if the bag
// carries the label, the bitmap contains the node. It is checked from a reader
// goroutine while a writer churns the same label, both under -race.
func assertLabelIndexNeverMissesABagLabel(t *testing.T, budget time.Duration) {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	const nodes = 1
	keys := make([]string, nodes)
	for i := range keys {
		keys[i] = string(rune('a'+i%26)) + string(rune('0'+i/26))
		if err := g.AddNode(keys[i]); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
	}
	lid := g.reg.Intern("L")

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// writerEpoch counts COMPLETED writer operations. It is what lets the reader
	// tell a genuine intermediate state from an ABA sequence; see the reader below.
	var writerEpoch atomic.Uint64
	// WRITER: churn the label on and off across the population.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			k := keys[i%nodes]
			if i%2 == 0 {
				_ = g.ApplyAtomically(func() error { return g.SetNodeLabel(k, "L") })
			} else {
				_ = g.ApplyAtomically(func() error { g.RemoveNodeLabel(k, "L"); return nil })
			}
			// Bumped AFTER the operation completes, so an epoch the reader finds
			// unchanged across its window means no operation FINISHED inside it —
			// while an operation still in flight, whose intermediate state is the
			// whole subject of this test, leaves the epoch untouched.
			writerEpoch.Add(1)
		}
	}()

	// READER: the implication must hold at every instant it samples.
	// Bounded by wall clock, not by iteration count: the window is a handful of
	// instructions wide, so detection is PROBABILISTIC and what matters is how long
	// the reader gets to sample it. Measured against the pre-fix ordering under -race:
	// 8 s detected 4/4 runs, 2 s only 3/8. Hence the two layers — see the callers.
	deadline := time.Now().Add(budget)
	violations := 0
	// sampled counts windows in which NO writer operation completed — the only
	// windows that witness one instant, and therefore the only ones this oracle may
	// judge. Asserted non-zero at the end: a guard that discards every sample is a
	// gate that cannot fail, which proves nothing.
	sampled, straddled := 0, 0
	for iter := 0; violations == 0; iter++ {
		if iter%1024 == 0 && time.Now().After(deadline) {
			break
		}
		k := keys[iter%nodes]
		id, ok := g.adj.Mapper().Lookup(k)
		if !ok {
			continue
		}
		// THE BAG IS READ TWICE, AROUND THE BITMAP (rmp #2332).
		//
		// This used to read the bag once and then the bitmap. A concurrent
		// RemoveNodeLabel landing between them clears BOTH, so the bag read returned
		// true, the bitmap read returned false, and the test reported a violation that
		// existed at no instant — measured at 1 failure in 20 runs on a build whose
		// invariant holds. The file used to claim it "cannot report a FALSE failure";
		// the invariant does hold at every instant, but the test was not sampling one.
		//
		// TWO FIXES WERE TRIED AND ARE BOTH WRONG, both because they made the gate
		// SOUND BUT BLIND — each detected the reintroduced rmp #2326 ordering in 0 of 8
		// runs:
		//
		//   - resolving the bitmap through a snapshot (LabelBitmapAsOf). That filters
		//     through the versioned label store, which CORRECTS the staleness that is
		//     the whole subject of the test.
		//   - taking both reads inside View. The writer mutates under ApplyAtomically,
		//     which holds the barrier exclusively, so a reader holding it shared cannot
		//     observe the intermediate state AT ALL — which is exactly what this file
		//     already says about the pre-rmp-#2308 world: "the visibility barrier hid
		//     both windows".
		//
		// So the read must stay present-time and barrier-free, and the pair cannot be
		// made atomic. What discriminates the two cases instead is the BAG: a false
		// positive requires a removal, and a removal clears the bag too, so the bag is
		// false by the time we look again. The real defect leaves the bag TRUE
		// throughout — it is the bitmap that has not caught up yet.
		//
		// THAT ARGUMENT IS TRUE ONLY OF A WINDOW HOLDING AT MOST ONE WRITER STEP, AND
		// THE WINDOW ROUTINELY HOLDS HUNDREDS (rmp #2371).
		//
		// Two completed operations — a removal AND a re-add — restore the bag to true,
		// so all three reads are honest at their own instants while the conjunction
		// describes a state that existed at none. Measured at HEAD with a writer-epoch
		// probe: 22.4 % of this reader's windows had TWO OR MORE writer operations
		// complete inside them, and the widest held 714. The writer toggles ONE node as
		// fast as it can and the reader takes three separately-locked reads, so this is
		// the common case, not a corner.
		//
		// THE EPOCH IS THE DISCRIMINATOR, and it costs nothing: two atomic loads. A
		// window in which no operation COMPLETED still contains any operation that is
		// mid-flight, which is exactly the rmp #2326 intermediate — the bag written
		// under the shard lock, the lock released, the bitmap not yet caught up. So the
		// guard removes the ABA reading without weakening detection.
		//
		// VALIDATED BY INJECTION, not by this argument: with the rmp #2326 ordering put
		// back (index maintenance moved after sh.mu.Unlock in setNodeLabelInfo), this
		// oracle reported 10, 23 and 36 violations in three 10 s runs and EVERY ONE had
		// a stable epoch — zero were attributed to a straddled window. The rate also
		// discriminates on its own: the real ordering defect fails 7 runs in 8 within
		// 0.06–0.38 s, whereas the flake reported in rmp #2371 was about 1 run in 10
		// over 2 s. Those are two orders of magnitude apart.
		before := writerEpoch.Load()
		inBag := g.HasNodeLabel(k, "L")
		inIdx := g.nodeIdx.Intersect(uint32(lid)).Contains(uint64(id))
		inBagAgain := g.HasNodeLabel(k, "L")
		if writerEpoch.Load() != before {
			// The three reads span more than one instant; they cannot witness an
			// instantaneous invariant either way.
			straddled++
			continue
		}
		sampled++
		if inBag && !inIdx && inBagAgain {
			violations++
			t.Errorf("node %s carries label L, before AND after reading the index, but is "+
				"ABSENT from L's bitmap, with NO writer operation completing between the "+
				"three reads: a present-time reader taking the raw bitmap loses the row "+
				"(rmp #2326)", k)
		}
	}
	close(stop)
	wg.Wait()

	// An oracle that judged nothing is not a passing oracle. If the writer were fast
	// enough that every window straddled an operation, the loop above would discard
	// every sample and report success having tested nothing (see rmp #2371 and the
	// "assert something was seen" rule).
	if sampled == 0 {
		t.Fatalf("no single-instant window was sampled in %s (%d straddled): the guard "+
			"discarded every observation, so this run tested nothing", budget, straddled)
	}
	t.Logf("sampled %d single-instant windows, discarded %d straddled ones (%.1f%%)",
		sampled, straddled, 100*float64(straddled)/float64(sampled+straddled))
}

// TestLabelIndex_NeverMissesALabelTheBagHas is the short-layer variant. Its 2 s
// budget detected the pre-fix ordering in only 3 of 8 runs, so a PASS here is not
// evidence the invariant holds — [graph/lpg] must stay under the 60 s per-package
// short-layer ceiling and this is what fits. It is worth its 2 s anyway; the
// load-bearing gate is the soak variant below.
//
// # It reported FALSE failures TWICE, and the second fix is the epoch
//
// The first attempt (rmp #2332) added the second bag read and this comment then
// claimed the false failures were gone. They were not: rmp #2371 measured the same
// test failing about 1 run in 10 at commit 21321e4e, which made `make ci` red
// intermittently and eroded the gate every other task depends on.
//
// The second bag read closes only the ONE-STEP window. The reader's three reads
// routinely straddle MANY completed writer operations — 22.4 % of windows hold two
// or more, the widest 714 — and a removal followed by a re-add restores the bag to
// true, so the conjunction can be reported over a state that existed at no instant.
// The writer-epoch guard in the helper closes that, and the injection evidence for
// why it is not merely a loosened assertion is recorded there.
//
// Not reproduced at HEAD in 55 consecutive runs (25 at load ≈ 2, 30 at load ≈ 20)
// with nothing in graph/lpg or graph/adjlist changed since it was measured — which
// is consistent with a rare sampling artefact and inconsistent with the ordering
// defect, whose signature is 7 failures in 8 runs inside 0.4 s.
func TestLabelIndex_NeverMissesALabelTheBagHas(t *testing.T) {
	assertLabelIndexNeverMissesABagLabel(t, 2*time.Second)
}
