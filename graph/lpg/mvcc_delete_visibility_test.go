package lpg

// mvcc_delete_visibility_test.go — the present-time delete/label-index seam
// (rmp #2687).
//
// Layer: short.
//
// # The invariant under test, stated once
//
// For a PRESENT-TIME reader — one with no pinned snapshot — the authority on
// whether a node exists is the tombstone bitmap: [Graph.NodeExistsAsOf] with a
// nil snapshot is literally `!g.IsTombstoned(id)`. The label bitmap is a
// candidate set derived from it, and [exec.NodeByLabelScan] emits every member
// of that candidate set without consulting any tombstone. So the two must agree
// AT THE SAME INSTANT, in BOTH directions:
//
//   - ARM 1, the over-report. A node the tombstone bitmap already called dead
//     BEFORE a read began must not be reported by that read. Violating it hands
//     a deleted node to a live query — the ACID Consistency break of Compliance
//     Mandate 2.
//   - ARM 2, the under-report — the MIRROR. A node the tombstone bitmap still
//     called alive AFTER a read finished must have been reported by that read.
//     Violating it silently loses a row, which no later predicate can recover.
//
// Both arms are checked on EVERY sample by the same probe, because a repair
// that closes one by opening the other is not a repair. That is not a
// hypothetical: hoisting the naive bitmap strip above the tombstone flip drove
// ARM 1 to zero and ARM 2 to 154 violations in 8338 reads.
//
// # Why the brackets are sound rather than merely plausible
//
// A race probe's workload here is MONOTONE: within a run the tombstone set only
// ever grows (the retirement probes) or only ever shrinks (the revival probe).
// That is what makes a one-sided sample conclusive:
//
//   - `before`, loaded BEFORE the bitmap read, is a subset of the dead set at
//     every later instant, so a member of it was dead for the whole read;
//   - a node absent from `after`, loaded AFTER the bitmap read, was absent from
//     the dead set at every earlier instant, so it was alive for the whole read.
//
// The label bag survives tombstoning — only the bitmap entry is retired — so
// every node in the population carries the label for the whole run and "should
// be reported" reduces to "is alive".
//
// # Non-vacuity is COUNTED, not asserted by inspection
//
// Each arm records how many samples were in a position to fail it, and the test
// fails when either count is zero. A probe that never sampled a member proves
// nothing, which is the failure mode that let this defect live under existing
// coverage: the pre-existing gate test deliberately publishes a node as dead
// only once RemoveNode has RETURNED, which excludes the window by construction.

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// ── the race probe ───────────────────────────────────────────────────────────

// deleteVisibilityTally is the probe's accounting. Every field is a count, so a
// run reports how much it looked at as well as what it found.
type deleteVisibilityTally struct {
	reads       atomic.Int64
	deadVisible atomic.Int64 // ARM 1 violations
	liveMissing atomic.Int64 // ARM 2 violations
	arm1Armed   atomic.Int64 // samples that COULD have failed ARM 1
	arm2Armed   atomic.Int64 // samples that COULD have failed ARM 2
}

// deleteVisibilityRounds is the round count, overridable so the same probe runs
// cheap in the short layer and long enough to resolve a rare window when a
// verdict is being taken. GOGRAPH_DELETEVIS_ROUNDS=400 is the setting the
// rmp #2687 measurements were taken at.
func deleteVisibilityRounds(def int) int {
	if s := os.Getenv("GOGRAPH_DELETEVIS_ROUNDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// deleteVisibilityFixture seeds a fresh graph with `population` nodes all
// carrying "Retired" and drains the substrate, so a round starts with the churn
// gate at zero rather than on the seeding's own leftovers.
func deleteVisibilityFixture(t *testing.T, round, population int) (
	*Graph[string, float64], LabelID, []string, []graph.NodeID,
) {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	keys := make([]string, population)
	ids := make([]graph.NodeID, population)
	for i := range keys {
		keys[i] = fmt.Sprintf("r%d-%d", round, i)
		if err := g.AddNode(keys[i]); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(keys[i], "Retired"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		id, ok := g.adj.Mapper().Lookup(keys[i])
		if !ok {
			t.Fatalf("Lookup(%q) missed after AddNode", keys[i])
		}
		ids[i] = id
	}
	g.ReclaimNow()
	return g, g.reg.Intern("Retired"), keys, ids
}

// runDeleteVisibilityProbe drives `rounds` independent graphs. In each round the
// population is moved one node at a time by `drive` while `readers` goroutines
// sample the label bitmap and bracket every sample against the tombstone bitmap
// on both sides.
//
// `prepare` runs before the readers start; `growing` says which way the dead set
// moves, which is what selects the sound half of each bracket.
func runDeleteVisibilityProbe(
	t *testing.T,
	rounds, population, readers int,
	growing bool,
	prepare func(g *Graph[string, float64], keys []string, ids []graph.NodeID),
	drive func(g *Graph[string, float64], key string, id graph.NodeID),
) *deleteVisibilityTally {
	t.Helper()
	tally := &deleteVisibilityTally{}

	for round := 0; round < rounds; round++ {
		g, lid, keys, ids := deleteVisibilityFixture(t, round, population)
		if prepare != nil {
			prepare(g, keys, ids)
		}

		var (
			stop atomic.Bool
			wg   sync.WaitGroup
		)
		warm := tally.reads.Load()
		for r := 0; r < readers; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					before := g.tombstones.Load()
					got := g.LabelBitmapAsOf(lid, nil)
					after := g.tombstones.Load()
					tally.reads.Add(1)
					if growing {
						checkRetirementSample(tally, ids, before, after, got)
					} else {
						checkRevivalSample(tally, ids, before, after, got)
					}
				}
			}()
		}

		// WARM UP before the first move. Without it the round is over before the
		// reader goroutines are scheduled at all: the first measurement of this
		// probe took 11 reads against 64 retirements, too few to call a rate from.
		for tally.reads.Load() < warm+int64(readers) {
			runtime.Gosched()
		}
		for i, k := range keys {
			drive(g, k, ids[i])
			// The window under test is a few hundred nanoseconds wide. Yielding
			// between moves lets a reader land in it rather than relying on
			// preemption, and costs the probe nothing else.
			runtime.Gosched()
		}
		stop.Store(true)
		wg.Wait()
		if err := g.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	return tally
}

// checkRetirementSample judges one sample of a workload whose dead set only
// GROWS.
func checkRetirementSample(
	tally *deleteVisibilityTally, ids []graph.NodeID, before, after, got *roaring64.Bitmap,
) {
	// ARM 1 — dead before the read began, reported by it.
	if before != nil && !before.IsEmpty() && !got.IsEmpty() {
		tally.arm1Armed.Add(1)
		if bad := roaring64.And(before, got); !bad.IsEmpty() {
			tally.deadVisible.Add(int64(bad.GetCardinality()))
		}
	}
	// ARM 2 — alive after the read finished, not reported by it.
	armed, missing := false, int64(0)
	for _, id := range ids {
		if after != nil && after.Contains(uint64(id)) {
			continue
		}
		armed = true
		if !got.Contains(uint64(id)) {
			missing++
		}
	}
	if armed {
		tally.arm2Armed.Add(1)
		tally.liveMissing.Add(missing)
	}
}

// checkRevivalSample judges one sample of a workload whose dead set only
// SHRINKS. The two brackets swap sides: `after` is the conclusive one for
// deadness and `before` for liveness.
func checkRevivalSample(
	tally *deleteVisibilityTally, ids []graph.NodeID, before, after, got *roaring64.Bitmap,
) {
	// ARM 1 — still dead after the read finished, therefore dead throughout it,
	// and reported by it.
	if after != nil && !after.IsEmpty() && !got.IsEmpty() {
		tally.arm1Armed.Add(1)
		if bad := roaring64.And(after, got); !bad.IsEmpty() {
			tally.deadVisible.Add(int64(bad.GetCardinality()))
		}
	}
	// ARM 2 — alive before the read began, therefore alive throughout it, and
	// not reported by it.
	armed, missing := false, int64(0)
	for _, id := range ids {
		if before != nil && before.Contains(uint64(id)) {
			continue
		}
		armed = true
		if !got.Contains(uint64(id)) {
			missing++
		}
	}
	if armed {
		tally.arm2Armed.Add(1)
		tally.liveMissing.Add(missing)
	}
}

// reportDeleteVisibility fails on any violation, and on either arm having been
// unable to fail.
func reportDeleteVisibility(t *testing.T, name string, tally *deleteVisibilityTally) {
	t.Helper()
	t.Logf("%s: %d reads; ARM 1 armed on %d, ARM 2 armed on %d; "+
		"ARM 1 violations %d, ARM 2 violations %d",
		name, tally.reads.Load(), tally.arm1Armed.Load(), tally.arm2Armed.Load(),
		tally.deadVisible.Load(), tally.liveMissing.Load())
	if tally.reads.Load() == 0 {
		t.Fatalf("%s: no read completed, so nothing was asserted", name)
	}
	if tally.arm1Armed.Load() == 0 {
		t.Fatalf("%s: no sample ever saw a non-empty dead set alongside a non-empty "+
			"answer, so ARM 1 could not have failed and this probe is vacuous", name)
	}
	if tally.arm2Armed.Load() == 0 {
		t.Fatalf("%s: no sample ever saw a live member, so ARM 2 could not have "+
			"failed and this probe is vacuous", name)
	}
	if n := tally.deadVisible.Load(); n != 0 {
		t.Errorf("%s: ARM 1 — %d times a present-time reader was handed a node the "+
			"tombstone bitmap had already called dead. NodeExistsAsOf(id, nil) is "+
			"!IsTombstoned(id), so this row is a deleted node reaching a live query "+
			"(rmp #2687).", name, n)
	}
	if n := tally.liveMissing.Load(); n != 0 {
		t.Errorf("%s: ARM 2 (MIRROR) — %d times a present-time reader FAILED to report "+
			"a node the tombstone bitmap still called alive. A lost row is the "+
			"direction no later predicate can recover (rmp #2687).", name, n)
	}
}

// ── the retirement paths that flip the tombstone under a live reader ─────────

// TestDeleteVisibility_PresentReaderAgreesWithTheTombstone is the reproduction
// probe for rmp #2687 and the mirror-image guard in one, over the two
// retirement paths that can run while a reader loops.
//
// MEASURED at GOGRAPH_DELETEVIS_ROUNDS=400, Apple M4, before the repair:
// ARM 1 reported 5991, 4966, 5108 and 5271 violations across four runs of the
// autocommit sub-test, and 0 after. ARM 2 reported 0 both times — and 154 under
// a deliberately mutated build that hoisted the bitmap strip above the flip for
// real, which is what proves it can fail at all.
func TestDeleteVisibility_PresentReaderAgreesWithTheTombstone(t *testing.T) {
	rounds := deleteVisibilityRounds(40)

	// PATH 1 — the direct Go API. tx is nil, so the death record is written in a
	// deferred call that runs after everything else; before rmp #2687 nothing at
	// all made the node a suspect until the strip.
	t.Run("RemoveNode", func(t *testing.T) {
		reportDeleteVisibility(t, "RemoveNode",
			runDeleteVisibilityProbe(t, rounds, 64, 4, true, nil,
				func(g *Graph[string, float64], key string, _ graph.NodeID) {
					g.RemoveNode(key)
				}))
	})

	// PATH 2 — the same retirement inside a versioned transaction, which is what
	// the Cypher DELETE / DETACH DELETE operators drive. This path claims the
	// death BEFORE the flip for conflict detection, so the node is already a
	// suspect; the sub-test exists to establish that rather than assume it.
	t.Run("RemoveNodeInTransaction", func(t *testing.T) {
		reportDeleteVisibility(t, "RemoveNodeInTransaction",
			runDeleteVisibilityProbe(t, rounds, 64, 4, true, nil,
				func(g *Graph[string, float64], key string, _ graph.NodeID) {
					if err := g.ApplyVersioned(func(tx WriteTx) error {
						g.Writer(tx).RemoveNode(key)
						return tx.Err()
					}); err != nil {
						t.Errorf("ApplyVersioned(RemoveNode %q): %v", key, err)
					}
				}))
	})
}

// TestDeleteVisibility_ReviveIsTheMirrorOfRetirement drives the same seam
// backwards: [Graph.revive] clears the tombstone and only then restores the
// label bitmaps, which is the shape that produces a LIVE node absent from the
// index if the correction cannot reach it.
func TestDeleteVisibility_ReviveIsTheMirrorOfRetirement(t *testing.T) {
	rounds := deleteVisibilityRounds(40)
	reportDeleteVisibility(t, "revive",
		runDeleteVisibilityProbe(t, rounds, 64, 4, false,
			func(g *Graph[string, float64], keys []string, _ []graph.NodeID) {
				for _, k := range keys {
					g.RemoveNode(k)
				}
				// Sweep, so the entries really have left the bitmap and the
				// revival has something to restore. Without this the probe would
				// pass on the deferral rather than on the restore.
				g.ReclaimNow()
			},
			func(g *Graph[string, float64], key string, _ graph.NodeID) {
				if err := g.AddNode(key); err != nil {
					t.Errorf("AddNode(%q) to revive: %v", key, err)
				}
			}))
}

// ── the retirement paths whose damage is PERMANENT, checked deterministically ─

// deadNodeReported is the one-line question every deterministic guard below
// asks: does a present-time label read report id?
func deadNodeReported(g *Graph[string, float64], lid LabelID, id graph.NodeID) bool {
	return g.LabelBitmapAsOf(lid, nil).Contains(uint64(id))
}

// TestDeleteVisibility_RestoreTombstonesRetiresTheEntries pins the second defect
// rmp #2687 carried: [Graph.RestoreTombstones] used to raise the churn gate for
// a divergent label and nothing else.
//
// Raising the gate only forces a reader onto the slow path; the slow path
// corrects over the SUSPECT set, and this path records no death instant, pushes
// no delta and — before the repair — deferred no removal, so the node was in
// none of the three sources. The reader took the slow path, found nothing to
// correct, and reported a deleted node. Permanently: the assertion after
// ReclaimNow is the one that made that measurable, because a drain is exactly
// what removes every unrelated suspect that might otherwise have masked it.
func TestDeleteVisibility_RestoreTombstonesRetiresTheEntries(t *testing.T) {
	g, lid := churnFixture(t, "Retired", "a")
	defer func() { _ = g.Close() }()
	id := mustID(t, g, "a")

	if !deadNodeReported(g, lid, id) {
		t.Fatal("setup: the live node is not reported, so the assertions below could " +
			"pass without the retirement having done anything")
	}
	g.RestoreTombstones([]graph.NodeID{id})
	if !g.IsTombstoned(id) {
		t.Fatal("setup: RestoreTombstones did not tombstone the node")
	}
	if deadNodeReported(g, lid, id) {
		t.Fatal("a present-time reader is handed a node RestoreTombstones has already " +
			"tombstoned: the churn gate is raised but the node is in no suspect " +
			"source, so the correction has nothing to visit (rmp #2687)")
	}
	// The drain is the discriminating half: it removes every suspect, so an
	// answer that survives it is the substrate's own and not some other write's
	// correction covering for this one.
	g.ReclaimNow()
	if deadNodeReported(g, lid, id) {
		t.Fatal("after a full reclaim a present-time reader still sees the node " +
			"RestoreTombstones tombstoned. This disagreement is permanent: no delta, " +
			"no death record and no deferred removal will ever revisit the entry " +
			"(rmp #2687)")
	}
	if g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("the reclaim swept the deferred removal but the raw bitmap still " +
			"carries the entry, so the answer above depends on a correction that has " +
			"nothing left to hold it up")
	}
}

// TestDeleteVisibility_LabellingADeadNodeDoesNotIndexIt pins the third shape:
// [Graph.setNodeLabelInfo] used to add the bitmap entry unconditionally, so
// labelling an already-tombstoned node put a dead node into the candidate set.
//
// The sequence is not hypothetical. It is what recovery replays: the
// self-sufficient path applies the snapshot's tombstone set BEFORE the
// snapshot's labels, the label capture walks the mapper — which retains
// tombstoned slots by contract — and the label bag survives tombstoning. So a
// reopened store served deleted nodes from a labelled scan, once the first
// reclaim had drained the label deltas that were masking it.
func TestDeleteVisibility_LabellingADeadNodeDoesNotIndexIt(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	id, ok := g.adj.Mapper().Lookup("a")
	if !ok {
		t.Fatal("Lookup missed after AddNode")
	}
	g.RestoreTombstones([]graph.NodeID{id})
	if err := g.SetNodeLabel("a", "Retired"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	lid := g.reg.Intern("Retired")

	if g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("labelling a tombstoned node put it into the raw label bitmap. The bag " +
			"must keep the label — revive restores from it — but the bitmap must not, " +
			"which is the contract stripLabelBitmaps exists to uphold (rmp #2687)")
	}
	if deadNodeReported(g, lid, id) {
		t.Fatal("a present-time reader is handed a tombstoned node that was labelled " +
			"after its retirement (rmp #2687)")
	}
	// The label delta keeps the node correctable for a while, so the drain is
	// again what discriminates: it is only after it that the entry would have
	// become permanent.
	g.ReclaimNow()
	if deadNodeReported(g, lid, id) {
		t.Fatal("after a full reclaim a present-time reader still sees the tombstoned " +
			"node that was labelled after its retirement. This is the state a store " +
			"reopen used to leave behind (rmp #2687)")
	}
	// And the label must still be on the node, so a revival can put it back.
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode to revive: %v", err)
	}
	if !deadNodeReported(g, lid, id) {
		t.Fatal("reviving the node did not restore its label-bitmap entry: the bag lost " +
			"the label, which turns the over-report this test closes into a lost row")
	}
}

// TestDeleteVisibility_TombstoneAbortedLeavesNoIndexedDeadNode records the state
// of the remaining retirement path — [Graph.tombstoneAborted], the withdrawal of
// an aborted CREATE — rather than assuming it.
//
// It is NOT affected, and the reason is measurable: the abort machinery
// withdraws the transaction's labels from the bag AND the index before it
// tombstones, so there is no entry left to disagree with the flip.
func TestDeleteVisibility_TombstoneAbortedLeavesNoIndexedDeadNode(t *testing.T) {
	g, lid := churnFixture(t, "Retired", "seed")
	defer func() { _ = g.Close() }()

	err := g.ApplyVersioned(func(tx WriteTx) error {
		wv := g.Writer(tx)
		if e := wv.AddNode("doomed"); e != nil {
			return e
		}
		if e := wv.SetNodeLabel("doomed", "Retired"); e != nil {
			return e
		}
		// Doomed exactly as a real serialization conflict does it.
		_ = tx.w.conflictErr(mvcc.StoreNodeLabels, ^uint64(0))
		return tx.w.err()
	})
	if err == nil {
		t.Fatal("the doomed transaction reported success, so Graph.tombstoneAborted " +
			"was never reached and this test asserts nothing")
	}
	id := mustID(t, g, "doomed")
	if !g.IsTombstoned(id) {
		t.Fatal("the aborted CREATE was not withdrawn, so this test drove the wrong path")
	}
	for _, when := range []string{"after the abort", "after a full reclaim"} {
		if g.nodeIdx.Has(uint32(lid), id) {
			t.Fatalf("%s the raw label bitmap still carries the withdrawn node", when)
		}
		if deadNodeReported(g, lid, id) {
			t.Fatalf("%s a present-time reader is handed the withdrawn node", when)
		}
		g.ReclaimNow()
	}
}

// TestDeleteVisibility_UndoReplayRetirementLeavesNoIndexedDeadNode covers the
// one retirement that reaches [Graph.removeNodeInfo] with the IMMEDIATE removal
// branch: the physical undo of a CREATE, which runs under
// [WriteTx.EnterUndo] and therefore cannot defer.
//
// It is the branch the repair deliberately left BELOW the tombstone flip, so
// what it must not do is leave a permanent disagreement.
func TestDeleteVisibility_UndoReplayRetirementLeavesNoIndexedDeadNode(t *testing.T) {
	g, lid := churnFixture(t, "Retired", "seed")
	defer func() { _ = g.Close() }()

	if err := g.ApplyVersioned(func(tx WriteTx) error {
		wv := g.Writer(tx)
		if e := wv.AddNode("created"); e != nil {
			return e
		}
		if e := wv.SetNodeLabel("created", "Retired"); e != nil {
			return e
		}
		// The physical undo of that CREATE, exactly as cypher's undo log replays
		// it: bracketed by EnterUndo/ExitUndo, so the index removal is immediate.
		tx.EnterUndo()
		wv.RemoveNode("created")
		tx.ExitUndo()
		return nil
	}); err != nil {
		t.Fatalf("ApplyVersioned: %v", err)
	}
	id := mustID(t, g, "created")
	if !g.IsTombstoned(id) {
		t.Fatal("the undo did not tombstone the node, so this test drove the wrong path")
	}
	g.ReclaimNow()
	if g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("the undo replay left the withdrawn node in the raw label bitmap")
	}
	if deadNodeReported(g, lid, id) {
		t.Fatal("after a full reclaim a present-time reader is handed a node an undo " +
			"replay tombstoned (rmp #2687)")
	}
}

// TestDeleteVisibility_TombstoneCounterNeverUnderCountsTheBitmap pins the
// publication order of the two halves of the tombstone set.
//
// [Graph.IsTombstoned] short-circuits on `tombstoneActive == 0`, and its comment
// claims that "a 0 observed here means no tombstone is committed". That claim
// was false for the width of one instruction: every flip stored the new bitmap
// FIRST and raised the counter second, so a reader could load a published bitmap
// that already carried the id while the counter still read zero — and be told by
// the authority itself that the node was alive.
//
// It matters beyond a probe's bracket. IsTombstoned IS
// `NodeExistsAsOf(id, nil)`, so during that window [Graph.correctBitmapOver]
// resolves a retired node as LIVE and keeps it in the reader's answer, which is
// the same ACID Consistency break by another route. It is also why the
// transactional arm of the probe above failed intermittently under -race — the
// only build slow enough to land inside two adjacent instructions — at 41
// violations in 8076 reads, while the same probe was clean over 1.3 million
// reads once the counter was raised first.
//
// The window opens only on the transition through zero, which is why the fixture
// is one node in a fresh graph and why the round count is high: each round gets
// exactly one chance. MEASURED before the repair: 19 disagreements in 2938
// samples.
func TestDeleteVisibility_TombstoneCounterNeverUnderCountsTheBitmap(t *testing.T) {
	const rounds = 3000
	var lies, checks atomic.Int64
	for round := 0; round < rounds; round++ {
		g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		if err := g.AddNode("a"); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		id, ok := g.adj.Mapper().Lookup("a")
		if !ok {
			t.Fatal("Lookup missed after AddNode")
		}
		var stop atomic.Bool
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				bm := g.tombstones.Load()
				if bm == nil || !bm.Contains(uint64(id)) {
					continue
				}
				// The published bitmap has retired the node. The accelerator the
				// whole engine reads existence through must agree, now.
				checks.Add(1)
				if !g.IsTombstoned(id) {
					lies.Add(1)
				}
				return
			}
		}()
		runtime.Gosched()
		g.RemoveNode("a")
		stop.Store(true)
		wg.Wait()
		if err := g.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	t.Logf("tombstone publication order: %d samples caught the published bitmap "+
		"carrying the id, %d of them disagreed with IsTombstoned",
		checks.Load(), lies.Load())
	if checks.Load() == 0 {
		t.Fatal("no sample ever observed the published bitmap carrying the id, so " +
			"the assertion below could not have failed and this test is vacuous")
	}
	if n := lies.Load(); n != 0 {
		t.Errorf("%d times the published tombstone bitmap had already retired the "+
			"node while IsTombstoned reported it alive. IsTombstoned is "+
			"NodeExistsAsOf(id, nil), so in that window the present-time authority "+
			"on existence contradicts the set it is derived from (rmp #2687).", n)
	}
}
