package lpg

// mvcc_label_churn_test.go — the per-label churn gate (rmp #2686).
//
// Layer: short.
//
// # What has to be proved, and why the shape of each test follows from it
//
// The gate lets a reader SKIP the suspect gathering and the bitmap correction
// when no label it scans has a live suspect. Skipping is only sound while the
// gate never under-counts, so every test here is built around one question: is
// the gate raised at the instant the thing it covers becomes observable?
//
// Two failure modes are therefore distinguished throughout:
//
//   - a test that passes because the gate was raised — what we want;
//   - a test that passes because the gate was ALREADY raised by something else.
//     Every deterministic guard below drains the substrate first and asserts the
//     counter reads ZERO before the action under test, so it cannot pass for the
//     second reason. That precondition is what makes the mutation of a single
//     raise site turn the test red.

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

// ── the table itself ─────────────────────────────────────────────────────────

// TestLabelChurn_TableGrowsByDoublingAndCountsExactly exercises the storage on
// its own: a label id far beyond the initial spine, a label id inside it, and
// concurrent raises and releases on both.
func TestLabelChurn_TableGrowsByDoublingAndCountsExactly(t *testing.T) {
	var lc labelChurn

	if lc.load(0) != 0 || lc.live(1<<20) {
		t.Fatal("an untouched table must read zero for every label, including one no chunk covers")
	}

	// A label far outside the initial spine forces several doublings at once.
	const far = LabelID(200_000)
	lc.raise(far)
	if !lc.live(far) {
		t.Fatalf("raise(%d) did not register", far)
	}
	if lc.live(far + 1) {
		t.Fatal("raising one label raised its neighbour: the counters are not per-label")
	}
	lc.release(far)
	if lc.live(far) {
		t.Fatalf("release(%d) did not return the counter to zero", far)
	}

	// Concurrent raises and releases across a spread of ids, half of which force
	// growth. The final state must be exactly zero everywhere.
	ids := []LabelID{0, 1, 255, 256, 257, 4095, 4096, 65_535, 131_072}
	const perID = 200
	var wg sync.WaitGroup
	for _, lid := range ids {
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func(lid LabelID) {
				defer wg.Done()
				for i := 0; i < perID; i++ {
					lc.raise(lid)
				}
				for i := 0; i < perID; i++ {
					lc.release(lid)
				}
			}(lid)
		}
	}
	wg.Wait()
	for _, lid := range ids {
		if got := lc.load(lid); got != 0 {
			t.Fatalf("label %d ended at %d, want 0: a raise or a release was lost, which is "+
				"either a permanent slow path or — in the losing direction — an under-count", lid, got)
		}
	}
}

// ── deterministic guards, one per raise site ─────────────────────────────────

// churnFixture returns a graph with `keys` interned, each carrying label, with
// the substrate DRAINED so that the churn gate for that label reads zero.
//
// The drain is the point: without it every assertion below would pass on the
// seeding's own leftover deltas rather than on the raise it is meant to test.
func churnFixture(t *testing.T, label string, keys ...string) (*Graph[string, float64], LabelID) {
	t.Helper()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range keys {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := g.SetNodeLabel(k, label); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", k, err)
		}
	}
	g.ReclaimNow()
	lid := g.reg.Intern(label)
	if got := g.labelChurn.load(lid); got != 0 {
		t.Fatalf("precondition: the churn gate for %q reads %d after a full reclaim, want 0. "+
			"Every assertion in this file would then pass on leftover churn rather than on "+
			"the raise it is testing.", label, got)
	}
	return g, lid
}

func mustID(t *testing.T, g *Graph[string, float64], key string) graph.NodeID {
	t.Helper()
	id, ok := g.adj.Mapper().Lookup(key)
	if !ok {
		t.Fatalf("node %q is not interned", key)
	}
	return id
}

// TestLabelChurnGate_LabelDeltaRaisesTheGate is the guard on the raise in
// [nodeLabelShard.pushLabelDelta].
//
// A label ADDED after a reader started must not reach that reader. The add puts
// the node into the raw bitmap immediately, so the only thing that keeps it out
// of the reader's answer is the correction — and the only thing that keeps the
// correction from being skipped is the delta's own churn hold.
func TestLabelChurnGate_LabelDeltaRaisesTheGate(t *testing.T) {
	g, _ := churnFixture(t, "Seed", "keeper")
	defer func() { _ = g.Close() }()
	if err := g.AddNode("late"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.ReclaimNow()

	lid := g.reg.Intern("Late")
	if got := g.labelChurn.load(lid); got != 0 {
		t.Fatalf("precondition: gate for Late reads %d, want 0", got)
	}

	snap := g.BeginRead()
	defer g.EndRead(snap)

	if err := g.SetNodeLabel("late", "Late"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	if got := g.labelChurn.load(lid); got == 0 {
		t.Fatal("the label add left the churn gate at zero, so a reader scanning Late will " +
			"take the raw bitmap and see a label that did not exist when it started")
	}
	id := mustID(t, g, "late")
	if !g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("precondition: the add should be in the RAW bitmap already, or the correction " +
			"has nothing to undo and this test proves nothing")
	}

	if bm := g.LabelBitmapAsOf(lid, snap); bm.Contains(uint64(id)) {
		t.Fatal("a snapshot reader saw a label added AFTER it started: the correction was " +
			"skipped because the per-label churn gate was not raised by the delta (rmp #2686)")
	}
}

// TestLabelChurnGate_DeferredIndexRemovalRaisesTheGate is the guard on the raise
// in [Graph.deferLabelIndexRemoval], isolated from the life record's hold.
//
// The isolation is deliberate and is the whole design of the test. A node retired
// through [Graph.removeNodeInfo] is held by TWO things at once — its death record
// and one deferred index removal per label — so dropping either raise alone
// leaves the other covering it. The life records are therefore reclaimed
// explicitly here, leaving the deferred removal as the only hold, which is
// exactly the state the vacuum reaches on its own: unitNodeLife is swept before
// unitIndexRemovals.
func TestLabelChurnGate_DeferredIndexRemovalRaisesTheGate(t *testing.T) {
	g, lid := churnFixture(t, "Doomed", "gone")
	defer func() { _ = g.Close() }()
	id := mustID(t, g, "gone")

	g.RemoveNode("gone")

	// Drop the life records, and only those. What is left holding the gate is the
	// deferred index removal the strip recorded.
	g.reclaimNodeLife(g.mvccClock.ReadTS())
	if got := g.nodeLifeActive.Load(); got != 0 {
		t.Fatalf("setup: %d life records survived the reclaim, so the life hold may still be "+
			"covering for the deferred removal's", got)
	}
	if got := g.labelChurn.load(lid); got == 0 {
		t.Fatal("with the life records gone the deferred index removal is the only thing that " +
			"can hold the gate up, and it reads zero: a reader will take the raw bitmap and " +
			"report a node that no longer exists (rmp #2686)")
	}
	if !g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("precondition: the removal is DEFERRED, so the entry must still be in the raw " +
			"bitmap or there is nothing for the correction to take out")
	}

	if bm := g.LabelBitmapAsOf(lid, nil); bm.Contains(uint64(id)) {
		t.Fatal("a present-time reader saw a REMOVED node in a label bitmap: the correction " +
			"was skipped because the deferred removal did not raise the per-label churn gate")
	}
}

// TestLabelChurnGate_ReviveRaisesTheGate is the guard on the raise in
// [Graph.noteNodeRevived], and it is the case the "a birth needs no per-label
// bump" argument does not cover.
//
// A revival is the one birth that puts a node back into label bitmaps without
// pushing a delta and without a deferred removal: [Graph.restoreLabelBitmaps]
// calls nodeIdx.Add directly, off the back of a bag that survived tombstoning. A
// reader older than the revival must still be told the node is gone, so the life
// record has to raise the gate itself.
func TestLabelChurnGate_ReviveRaisesTheGate(t *testing.T) {
	g, lid := churnFixture(t, "Back", "phoenix")
	defer func() { _ = g.Close() }()
	id := mustID(t, g, "phoenix")

	g.RemoveNode("phoenix")
	g.ReclaimNow()
	if g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("setup: the deferred removal should have been applied by the reclaim, leaving " +
			"the node out of the bitmap")
	}
	if got := g.labelChurn.load(lid); got != 0 {
		t.Fatalf("setup: the gate reads %d after a full reclaim, want 0", got)
	}

	// The reader's instant: the node is DEAD here.
	snap := g.BeginRead()
	defer g.EndRead(snap)

	if err := g.AddNode("phoenix"); err != nil {
		t.Fatalf("AddNode (revive): %v", err)
	}
	if !g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("setup: the revival should have restored the label bitmap entry, or there is " +
			"nothing for the correction to take out")
	}
	if got := g.labelChurn.load(lid); got == 0 {
		t.Fatal("the revival restored a label bitmap entry and left the churn gate at zero: a " +
			"reader older than the revival will take the raw bitmap and see a node that was " +
			"dead at its own instant (rmp #2686)")
	}

	if bm := g.LabelBitmapAsOf(lid, snap); bm.Contains(uint64(id)) {
		t.Fatal("a snapshot reader saw a node that was REMOVED as of its own instant: the " +
			"correction was skipped because the revival did not raise the per-label churn gate")
	}
}

// TestLabelChurnGate_QuietLabelIsNotCorrected is the control, and it is what
// makes the three tests above mean something.
//
// It asserts the gate actually CLOSES: with churn live on one label, a read of a
// DIFFERENT label must skip the correction. Without this, a gate wired to return
// true unconditionally would pass every other test in this file.
func TestLabelChurnGate_QuietLabelIsNotCorrected(t *testing.T) {
	g, quiet := churnFixture(t, "Quiet", "a", "b")
	defer func() { _ = g.Close() }()
	if err := g.AddNode("c"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	g.ReclaimNow()

	snap := g.BeginRead()
	defer g.EndRead(snap)

	// Churn on a label the read does not concern.
	if err := g.SetNodeLabel("c", "Busy"); err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	busy := g.reg.Intern("Busy")
	if !g.labelChurn.live(busy) {
		t.Fatal("setup: the write on Busy did not raise its own gate")
	}
	if g.labelChurn.live(quiet) {
		t.Fatal("a write on Busy raised the gate for Quiet: the gate is not per-label at all, " +
			"and the optimisation it exists for cannot engage")
	}
	if !g.labelBitmapNeedsFilter(snap) {
		t.Fatal("setup: the global gates should say a filter is needed, or the per-label gate " +
			"is not the thing being measured here")
	}

	bm := g.LabelBitmapAsOf(quiet, snap)
	if got := bm.GetCardinality(); got != 2 {
		t.Fatalf("Quiet has %d members, want 2", got)
	}
}

// ── the identity property, under concurrent mixed load ───────────────────────

// labelBitmapUngated is the pre-gate algorithm: sample the suspects on both
// sides of the clone and correct over the union, with no early exit at all.
//
// It is the reference the gated implementation must agree with. It deliberately
// does NOT reproduce HEAD's `labelBitmapNeedsFilter` early exit, so it corrects
// at least as often as HEAD did — agreeing with it is therefore a stronger
// statement than agreeing with HEAD.
func (g *Graph[N, W]) labelBitmapUngated(lid LabelID, s *Snapshot) *roaring64.Bitmap {
	pre := g.suspectNodes()
	bm := g.nodeIdx.Intersect(uint32(lid))
	pre = append(pre, g.suspectNodes()...)
	g.correctBitmapOver(bm, s, func(bag labelBag) bool { return bag.has(lid) }, pre)
	return bm
}

// labelBitmapOracle answers the same question from the versioned stores alone,
// without consulting the label index at all: every interned node that existed at
// s and carried lid at s.
//
// It is O(V) and therefore unusable in production, which is the entire reason the
// index and its correction exist. Here it is the ground truth both of the other
// two answers are measured against.
func (g *Graph[N, W]) labelBitmapOracle(lid LabelID, s *Snapshot) *roaring64.Bitmap {
	out := roaring64.New()
	g.adj.Mapper().Walk(func(id graph.NodeID, _ N) bool {
		if g.NodeExistsAsOf(id, s) && g.labelBagTest(id, s, func(bag labelBag) bool { return bag.has(lid) }) {
			out.Add(uint64(id))
		}
		return true
	})
	return out
}

// TestLabelChurnGate_GatedAnswerMatchesUngatedUnderMixedLoad drives readers
// against a writer and then, with the load stopped but the churn still live,
// requires the gated answer to equal the ungated correction and the O(V)
// versioned oracle exactly.
//
// # Why the exact comparison is made with the writer STOPPED
//
// Measured first, then designed around. Under concurrent writes the ungated
// correction does not agree WITH ITSELF: two back-to-back calls at one fixed
// snapshot differed on 72 of 240 comparisons, with no gate involved on either
// side. The corrected bitmap is a SUPERSET of the versioned truth by a margin
// that moves with the raw bitmap, which is the candidate-set discipline this
// package documents — a scan re-checks each row, so an extra member is tolerated
// and a missing one is not. Exact equality is therefore a property of the code
// only when nothing is writing, and an equality assertion taken under load would
// be measuring the writer's timing.
//
// So the test has two phases and asserts a different thing in each:
//
//   - UNDER LOAD, the invariant that has to hold at every instant: the gated
//     answer never LOSES a row the versioned stores say is there. That is the
//     unrecoverable direction, and it is what a skipped correction would cause.
//   - WITH THE WRITER STOPPED but the churn still pinned open by a long-lived
//     reader, exact equality with both references — including for a label the
//     writer never touched, which is the case the gate actually short-circuits.
//     The phase asserts its own preconditions so it cannot pass vacuously.
func TestLabelChurnGate_GatedAnswerMatchesUngatedUnderMixedLoad(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	defer func() { _ = g.Close() }()

	// The writer touches Alpha and Beta only. Gamma is seeded and then left
	// alone, so it is the label whose gate must stay shut while the other two
	// churn — the discriminating case for the whole optimisation.
	//
	// # No node is removed and re-created, and that is a measured constraint
	//
	// The node-life store is ONE RECORD DEEP per direction, so a second removal
	// overwrites the first and an old reader's existence answer genuinely moves
	// under it. A probe that let the writer cycle RemoveNode/AddNode on the same
	// key produced "lost rows" against the oracle with the churn gate LIVE on
	// both sides of the read (gateBefore=true, gateAfter=true, churn=184) — the
	// gate had nothing to do with them. So each node here is deleted at most once
	// and created at most once, which keeps the versioned answer at a fixed
	// snapshot stable and leaves any disagreement attributable.
	const (
		churned = 200
		doomed  = 100
		fresh   = 200
		still   = 50
	)
	hot := []string{"Alpha", "Beta"}
	seedLabelled := func(key, label string) {
		t.Helper()
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%q): %v", key, err)
		}
		if err := g.SetNodeLabel(key, label); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", key, err)
		}
	}
	for i := 0; i < churned; i++ {
		seedLabelled(fmt.Sprintf("n%d", i), hot[i%len(hot)])
	}
	for i := 0; i < doomed; i++ {
		seedLabelled(fmt.Sprintf("d%d", i), hot[i%len(hot)])
	}
	for i := 0; i < still; i++ {
		seedLabelled(fmt.Sprintf("g%d", i), "Gamma")
	}
	alpha, beta, gamma := g.reg.Intern("Alpha"), g.reg.Intern("Beta"), g.reg.Intern("Gamma")
	lids := []LabelID{alpha, beta, gamma}

	// Opened BEFORE the writer starts, and held to the end: it pins the
	// reclamation watermark below every write the workload makes, so phase two
	// runs with the churn still live rather than swept away.
	pin := g.BeginRead()
	defer g.EndRead(pin)

	var (
		stop      atomic.Bool
		writers   sync.WaitGroup
		readers   sync.WaitGroup
		readsDone atomic.Int64
	)

	writers.Add(1)
	go func() {
		defer writers.Done()
		nextDoomed, nextFresh := 0, 0
		for i := 0; !stop.Load(); i++ {
			key := fmt.Sprintf("n%d", i%churned)
			switch i % 4 {
			case 0:
				_ = g.SetNodeLabel(key, hot[(i+1)%len(hot)])
			case 1:
				g.RemoveNodeLabel(key, hot[(i+1)%len(hot)])
			case 2:
				// Each doomed node is retired exactly ONCE, and never revived.
				if nextDoomed < doomed {
					g.RemoveNode(fmt.Sprintf("d%d", nextDoomed))
					nextDoomed++
				}
			case 3:
				// Each fresh node is created exactly ONCE. The cap also bounds V,
				// which matters because the oracle below is O(V).
				if nextFresh < fresh {
					k := fmt.Sprintf("fresh%d", nextFresh)
					_ = g.AddNode(k)
					_ = g.SetNodeLabel(k, hot[nextFresh%len(hot)])
					nextFresh++
				}
			}
		}
	}()

	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for n := 0; n < 15; n++ {
				snap := g.BeginRead()
				for _, lid := range lids {
					got := g.LabelBitmapAsOf(lid, snap)
					oracle := g.labelBitmapOracle(lid, snap)
					if missing := roaring64.AndNot(oracle, got); !missing.IsEmpty() {
						t.Errorf("label %d: the gated answer LOST %v — every one of those "+
							"nodes existed and carried the label at the reader's own instant. "+
							"A missing member is a silently lost row, which no per-row "+
							"predicate can recover (rmp #2686).", lid, missing.ToArray())
						return
					}
					readsDone.Add(1)
				}
				g.EndRead(snap)
			}
		}()
	}

	readers.Wait()
	stop.Store(true)
	writers.Wait()

	if readsDone.Load() == 0 {
		t.Fatal("no read completed under load, so phase one never ran")
	}

	// ── phase two: identical answers, with the churn still live ──────────────
	//
	// The preconditions come first. Without them this phase would pass on a
	// quiesced graph where the ungated correction has nothing to do either, and
	// would prove nothing at all about the gate.
	suspects := g.suspectNodes()
	if len(suspects) == 0 {
		t.Fatal("no suspects survived the workload, so the ungated correction has nothing " +
			"to do and comparing the two answers is vacuous")
	}
	if !g.labelBitmapNeedsFilter(pin) {
		t.Fatal("the global gates say no filter is needed, so this phase measures nothing " +
			"about the per-label gate")
	}
	if !g.churnLive(oneLabel(alpha)) || !g.churnLive(oneLabel(beta)) {
		t.Fatal("the writer churned Alpha and Beta, so both gates must be live here")
	}
	if g.churnLive(oneLabel(gamma)) {
		t.Fatalf("Gamma was never written after seeding, yet its gate reads %d. The gate is "+
			"not discriminating by label, so the read it exists to short-circuit still "+
			"walks %d suspects.", g.labelChurn.load(gamma), len(suspects))
	}

	for _, lid := range lids {
		got := g.LabelBitmapAsOf(lid, pin)
		want := g.labelBitmapUngated(lid, pin)
		oracle := g.labelBitmapOracle(lid, pin)
		if !got.Equals(want) {
			t.Errorf("label %d: the GATED answer differs from the ungated correction.\n"+
				"gated only:   %v\nungated only: %v",
				lid, roaring64.AndNot(got, want).ToArray(), roaring64.AndNot(want, got).ToArray())
		}
		if !got.Equals(oracle) {
			t.Errorf("label %d: the gated answer differs from the O(V) versioned oracle.\n"+
				"gated only:  %v\noracle only: %v",
				lid, roaring64.AndNot(got, oracle).ToArray(), roaring64.AndNot(oracle, got).ToArray())
		}
	}
	t.Logf("phase one compared %d reads under load; phase two ran against %d live suspects "+
		"with Gamma's gate shut", readsDone.Load(), len(suspects))
}

// ── the four retirement paths ────────────────────────────────────────────────

// settledDead is the set of nodes whose retirement has FULLY COMPLETED, which is
// the only set a concurrent reader can be held to.
//
// # Why "settled" and not "tombstoned"
//
// [Graph.removeNodeInfo] flips the tombstone bitmap BEFORE it strips the label
// bitmaps and, on the autocommit path, before it records the death instant. In
// that window the node is dead, still in the label bitmap, and in NO suspect
// source, so no correction can reach it. Measured against a build with the churn
// gate short-circuited to always-live — HEAD's read path — that window produced 6
// violations across 8 runs of the same probe, so it is pre-existing and is not
// what this test is for. Publishing a key only once RemoveNode has returned
// removes the window from the assertion without weakening it: from that instant
// the node is dead, it is a suspect, and reporting it is unambiguously wrong.
type settledDead struct {
	mu  sync.Mutex
	ids []graph.NodeID
}

func (s *settledDead) publish(ids ...graph.NodeID) {
	s.mu.Lock()
	s.ids = append(s.ids, ids...)
	s.mu.Unlock()
}

func (s *settledDead) snapshot() *roaring64.Bitmap {
	out := roaring64.New()
	s.mu.Lock()
	for _, id := range s.ids {
		out.Add(uint64(id))
	}
	s.mu.Unlock()
	return out
}

// TestLabelChurnGate_NoReaderSeesADeadNode drives each of the four paths that can
// retire a node while readers loop over the label bitmap, and requires that no
// node whose retirement has completed is ever reported by one.
//
// The four paths are named in each sub-test because they feed the gate in four
// different places, and a hole in any one of them is a different defect.
func TestLabelChurnGate_NoReaderSeesADeadNode(t *testing.T) {
	const population = 120
	// readerCount is named so the warm-up below can wait for exactly as many
	// first reads as there are readers.
	const readerCount = 3

	// run seeds `population` nodes carrying Retired, drains the substrate so the
	// gate for Retired reads zero, starts three readers, and hands the driver a
	// set to publish settled deaths into.
	run := func(t *testing.T, path string,
		prepare func(t *testing.T, g *Graph[string, float64], keys []string, ids []graph.NodeID),
		retire func(t *testing.T, g *Graph[string, float64], keys []string, ids []graph.NodeID, dead *settledDead)) {
		t.Helper()
		keys := make([]string, population)
		for i := range keys {
			keys[i] = fmt.Sprintf("r%d", i)
		}
		g, lid := churnFixture(t, "Retired", keys...)
		defer func() { _ = g.Close() }()
		ids := make([]graph.NodeID, len(keys))
		for i, k := range keys {
			ids[i] = mustID(t, g, k)
		}
		if prepare != nil {
			prepare(t, g, keys, ids)
		}

		var (
			stop  atomic.Bool
			reads atomic.Int64
			wg    sync.WaitGroup
			dead  settledDead
		)
		for r := 0; r < readerCount; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stop.Load() {
					want := dead.snapshot()
					got := g.LabelBitmapAsOf(lid, nil)
					reads.Add(1)
					if bad := roaring64.And(want, got); !bad.IsEmpty() {
						t.Errorf("%s: a reader saw %v in the Retired bitmap after their "+
							"retirement had completed. The correction was skipped, so the "+
							"path did not raise the per-label churn gate (rmp #2686).",
							path, bad.ToArray())
						return
					}
				}
			}()
		}

		// WARM UP before the driver moves (rmp #2687). Without this the retirement
		// can complete before a single reader goroutine is scheduled, and the
		// non-vacuity guard below then fails the test with "the readers completed
		// no read" — not because anything is wrong, but because nothing ran.
		// MEASURED at GOMAXPROCS=1: 40 such failures in 10 runs, identically on
		// this build and on the one before rmp #2687 touched the delete path.
		// Bounded so a reader that returns early on a violation cannot hang the
		// test; the guard below still fails if the wait bought nothing.
		for deadline := time.Now().Add(5 * time.Second); reads.Load() < readerCount; {
			if time.Now().After(deadline) {
				break
			}
			runtime.Gosched()
		}

		retire(t, g, keys, ids, &dead)

		stop.Store(true)
		wg.Wait()
		if reads.Load() == 0 {
			t.Fatalf("%s: the readers completed no read, so nothing was asserted", path)
		}
		if dead.snapshot().IsEmpty() {
			t.Fatalf("%s: no retirement was ever published, so the assertion above could "+
				"not fail and this sub-test is vacuous", path)
		}

		// The deterministic close, with nothing else running.
		if bad := roaring64.And(dead.snapshot(), g.LabelBitmapAsOf(lid, nil)); !bad.IsEmpty() {
			t.Fatalf("%s: with the workload stopped, the Retired bitmap still reports the "+
				"retired nodes %v", path, bad.ToArray())
		}
		t.Logf("%s: %d reads asserted against %d settled retirements",
			path, reads.Load(), dead.snapshot().GetCardinality())
	}

	// PATH 1 — Graph.removeNodeInfo (lpg.go:3176). The ordinary delete: it records
	// a death instant and, through stripLabelBitmaps, defers one index removal per
	// label in the bag. Both hold the gate up.
	t.Run("removeNodeInfo", func(t *testing.T) {
		run(t, "removeNodeInfo", nil,
			func(t *testing.T, g *Graph[string, float64], keys []string, ids []graph.NodeID, dead *settledDead) {
				for i, k := range keys {
					g.RemoveNode(k)
					dead.publish(ids[i])
				}
			})
	})

	// PATH 2 — Graph.tombstoneAborted (mvcc_abort_sides.go:258). Withdrawing an
	// aborted CREATE: no death instant is recorded and no bitmap is stripped, and
	// it runs after the label withdrawal has dropped the gate the create raised.
	t.Run("tombstoneAborted", func(t *testing.T) {
		run(t, "tombstoneAborted", nil,
			func(t *testing.T, g *Graph[string, float64], _ []string, _ []graph.NodeID, dead *settledDead) {
				for batch := 0; batch < 4; batch++ {
					var made []string
					err := g.ApplyVersioned(func(tx WriteTx) error {
						wv := g.Writer(tx)
						for i := 0; i < 30; i++ {
							k := fmt.Sprintf("aborted-create-%d-%d", batch, i)
							if e := wv.AddNode(k); e != nil {
								return e
							}
							if e := wv.SetNodeLabel(k, "Retired"); e != nil {
								return e
							}
							made = append(made, k)
						}
						// Doomed exactly as a real serialization conflict does it.
						_ = tx.w.conflictErr(mvcc.StoreNodeLabels, ^uint64(0))
						return tx.w.err()
					})
					if err == nil {
						t.Fatal("the doomed transaction reported success, so no abort was " +
							"processed and Graph.tombstoneAborted was never reached")
					}
					for _, k := range made {
						id := mustID(t, g, k)
						if !g.IsTombstoned(id) {
							t.Fatalf("%q survived the withdrawal of the transaction that "+
								"created it, so this sub-test drove the wrong path", k)
						}
						dead.publish(id)
					}
				}
			})
	})

	// PATH 3 — Graph.reviveAborted (mvcc_abort_sides.go:277). Withdrawing an
	// aborted DELETE. The nodes are dead for the life of the transaction — which
	// is the window the in-flight assertion below covers — and alive again
	// afterwards, with no birth instant recorded.
	t.Run("reviveAborted", func(t *testing.T) {
		run(t, "reviveAborted", nil,
			func(t *testing.T, g *Graph[string, float64], keys []string, ids []graph.NodeID, dead *settledDead) {
				lid := g.reg.Intern("Retired")
				for batch := 0; batch < 4; batch++ {
					lo, hi := batch*30, batch*30+30
					err := g.ApplyVersioned(func(tx WriteTx) error {
						wv := g.Writer(tx)
						for _, k := range keys[lo:hi] {
							wv.RemoveNode(k)
						}
						// IN FLIGHT: the deletes are applied to the tombstone
						// bitmap already and their index removals are deferred, so
						// the entries are still there and only the correction can
						// take them out. Asserted from the driver rather than from
						// a reader goroutine because the window closes when this
						// bracket does.
						bm := g.LabelBitmapAsOf(lid, nil)
						for _, id := range ids[lo:hi] {
							if bm.Contains(uint64(id)) {
								t.Errorf("a reader saw node %d in the Retired bitmap while "+
									"the transaction that deleted it was still in flight", id)
								break
							}
						}
						_ = tx.w.conflictErr(mvcc.StoreNodeExistence, ^uint64(0))
						return tx.w.err()
					})
					if err == nil {
						t.Fatal("the doomed transaction reported success, so " +
							"Graph.reviveAborted was never reached")
					}
				}
				// The withdrawal must have put every one of them back, in the
				// tombstone set AND in the label bitmap. A node lost here is the
				// unrecoverable direction.
				got := g.LabelBitmapAsOf(lid, nil)
				for _, id := range ids {
					if g.IsTombstoned(id) {
						t.Fatalf("node %d is still tombstoned after the aborted delete was "+
							"withdrawn, so this sub-test drove the wrong path", id)
					}
					if !got.Contains(uint64(id)) {
						t.Fatalf("node %d was LOST from the label bitmap by an aborted delete "+
							"that was withdrawn: no per-row predicate can recover it", id)
					}
				}
				// Nothing is settled-dead on this path, so publish the one node the
				// close needs to be non-vacuous: a node retired the ordinary way.
				g.RemoveNode(keys[0])
				dead.publish(ids[0])
			})
	})

	// PATH 4 — Graph.RestoreTombstones (lpg.go:3965). The exported one-shot
	// recovery path: it records no death instant and strips no label bitmap, so a
	// node it retires while the index still carries it is a permanent
	// disagreement — which is why the gate is PINNED there rather than held.
	//
	// The prepare step gives every node a second label under a pinned reader. That
	// is what makes the sub-test discriminate: the Marker delta keeps the node in
	// the suspect set so a correction can reach it at all, and it raises the gate
	// for MARKER, not for Retired — so the only thing that can keep Retired's gate
	// up is RestoreTombstones' own pin.
	t.Run("RestoreTombstones", func(t *testing.T) {
		var hold *Snapshot
		var holder *Graph[string, float64]
		defer func() {
			if hold != nil {
				holder.EndRead(hold)
			}
		}()
		run(t, "RestoreTombstones",
			func(t *testing.T, g *Graph[string, float64], keys []string, _ []graph.NodeID) {
				holder = g
				hold = g.BeginRead()
				for _, k := range keys {
					if err := g.SetNodeLabel(k, "Marker"); err != nil {
						t.Fatalf("SetNodeLabel(Marker): %v", err)
					}
				}
				retired, marker := g.reg.Intern("Retired"), g.reg.Intern("Marker")
				if !g.churnLive(oneLabel(marker)) {
					t.Fatal("setup: the Marker writes did not raise Marker's gate")
				}
				if g.churnLive(oneLabel(retired)) {
					t.Fatalf("setup: Retired's gate reads %d before the restore, so the pin "+
						"under test would not be the thing holding it up",
						g.labelChurn.load(retired))
				}
			},
			func(t *testing.T, g *Graph[string, float64], _ []string, ids []graph.NodeID, dead *settledDead) {
				for batch := 0; batch < 4; batch++ {
					chunk := ids[batch*30 : batch*30+30]
					g.RestoreTombstones(chunk)
					dead.publish(chunk...)
				}
				if got := g.labelChurn.load(g.reg.Intern("Retired")); got == 0 {
					t.Fatal("RestoreTombstones left Retired's gate at zero while the nodes it " +
						"tombstoned are still in Retired's bitmap: a reader will take the raw " +
						"bitmap and report them (rmp #2686)")
				}
			})
	})
}

// ── isolated guards for the two abort-side pins, and for the death record ────

// TestLabelChurnGate_DeathRecordRaisesTheGate isolates the hold the DEATH record
// takes from the one the deferred index removal takes.
//
// The two normally cover for each other, so neither can be tested while the other
// is in place. Here the deferred removals are applied explicitly — which is the
// state the vacuum reaches on its own, since unitNodeLife is swept before
// unitIndexRemovals — leaving the death record as the only hold. The direction
// under test is the unrecoverable one: a reader OLDER than the death must still
// find the node, and the entry has already left the raw bitmap, so only the
// correction's add-back can give it to them.
func TestLabelChurnGate_DeathRecordRaisesTheGate(t *testing.T) {
	g, lid := churnFixture(t, "Doomed", "victim")
	defer func() { _ = g.Close() }()
	id := mustID(t, g, "victim")

	// The reader's instant: victim is ALIVE and carries Doomed here.
	snap := g.BeginRead()
	defer g.EndRead(snap)

	g.RemoveNode("victim")
	g.applyDeferredIndexRemovals(g.mvccClock.ReadTS())
	if got := g.idxPendingActive.Load(); got != 0 {
		t.Fatalf("setup: %d deferred removals survived, so their hold may still be covering "+
			"for the death record's", got)
	}
	if g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("setup: the deferred removal was supposed to be applied, taking the entry " +
			"out of the raw bitmap — there is nothing for the add-back to restore otherwise")
	}
	if got := g.labelChurn.load(lid); got == 0 {
		t.Fatal("with the deferred removals applied the death record is the only thing that " +
			"can hold the gate up, and it reads zero: a reader older than the death will take " +
			"the raw bitmap and LOSE the node (rmp #2686)")
	}

	if bm := g.LabelBitmapAsOf(lid, snap); !bm.Contains(uint64(id)) {
		t.Fatal("a reader whose snapshot predates the removal LOST the node from the label " +
			"bitmap: the add-back was skipped because the death record did not raise the " +
			"per-label churn gate. A missing member is a silently lost row.")
	}
}

// abortedRevivalFixture leaves key TOMBSTONED, absent from label's bitmap but
// still carrying it in the bag, and holding a live delta on a SECOND label.
//
// That second label is what makes the two pin tests below discriminate. It keeps
// the node in the suspect set — otherwise no correction could reach it whatever
// the gate said — while raising the gate for Marker and NOT for label, so the
// only thing that can hold label's gate up is the pin under test.
//
// The returned snapshot must be held for the life of the test: it pins the
// reclamation watermark below the Marker delta.
func abortedRevivalFixture(t *testing.T, key, label string) (*Graph[string, float64], LabelID, graph.NodeID, *Snapshot) {
	t.Helper()
	g, lid := churnFixture(t, label, key)
	id := mustID(t, g, key)

	g.RemoveNode(key)
	g.ReclaimNow()
	if g.nodeIdx.Has(uint32(lid), id) {
		t.Fatalf("setup: %q should have left %s's bitmap when the deferred removal was applied", key, label)
	}
	if got := g.labelChurn.load(lid); got != 0 {
		t.Fatalf("setup: %s's gate reads %d after a full reclaim, want 0", label, got)
	}

	hold := g.BeginRead()
	if err := g.SetNodeLabel(key, "Marker"); err != nil {
		g.EndRead(hold)
		t.Fatalf("SetNodeLabel(Marker): %v", err)
	}
	if !g.churnLive(oneLabel(g.reg.Intern("Marker"))) {
		g.EndRead(hold)
		t.Fatal("setup: the Marker write did not raise Marker's own gate")
	}
	if g.churnLive(oneLabel(lid)) {
		got := g.labelChurn.load(lid)
		g.EndRead(hold)
		t.Fatalf("setup: %s's gate reads %d before the abort, so the pin under test would "+
			"not be the thing holding it up", label, got)
	}
	return g, lid, id, hold
}

// TestLabelChurnGate_TombstoneAbortedPinsTheGate is the guard on the pin in
// [Graph.tombstoneAborted].
//
// The shape is a REVIVAL the abort machinery then withdraws: the revival restores
// the node's label bitmap entries with no delta and no deferred removal, and the
// withdrawal tombstones it again with no death instant. What is left is a
// tombstoned node sitting in a label bitmap that nothing will ever revisit — the
// one shape on this path where the gate has to be pinned rather than held.
func TestLabelChurnGate_TombstoneAbortedPinsTheGate(t *testing.T) {
	g, lid, id, hold := abortedRevivalFixture(t, "ghost", "Kept")
	defer func() { g.EndRead(hold); _ = g.Close() }()

	err := g.ApplyVersioned(func(tx WriteTx) error {
		wv := g.Writer(tx)
		if e := wv.AddNode("ghost"); e != nil {
			return e
		}
		_ = tx.w.conflictErr(mvcc.StoreNodeExistence, ^uint64(0))
		return tx.w.err()
	})
	if err == nil {
		t.Fatal("the doomed revival reported success, so Graph.tombstoneAborted was never reached")
	}

	if !g.IsTombstoned(id) {
		t.Fatal("setup: withdrawing the aborted revival should have re-tombstoned the node")
	}
	if !g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("setup: the revival should have restored the Kept bitmap entry, or there is " +
			"nothing for the correction to take out and this test proves nothing")
	}
	if got := g.labelChurn.load(lid); got == 0 {
		t.Fatal("tombstoneAborted left a dead node in Kept's bitmap with Kept's gate at zero: " +
			"a reader will take the raw bitmap and report a node that does not exist (rmp #2686)")
	}

	if bm := g.LabelBitmapAsOf(lid, nil); bm.Contains(uint64(id)) {
		t.Fatal("a reader saw a node that Graph.tombstoneAborted had killed: the correction " +
			"was skipped because the flip did not pin the per-label churn gate")
	}
}

// TestLabelChurnGate_ReviveAbortedPinsTheGate is the guard on the pin in
// [Graph.reviveAborted], and it tests the LOSING direction.
//
// The shape is an aborted DELETE of a node that was already tombstoned: the
// withdrawal brings it back to life with no birth instant and restores no label
// bitmap, so it ends alive, carrying the label in its bag, and absent from that
// label's bitmap. Only the correction's add-back can give the row back, and only
// the pin can keep the correction from being skipped.
func TestLabelChurnGate_ReviveAbortedPinsTheGate(t *testing.T) {
	g, lid, id, hold := abortedRevivalFixture(t, "sleeper", "Kept")
	defer func() { g.EndRead(hold); _ = g.Close() }()

	err := g.ApplyVersioned(func(tx WriteTx) error {
		wv := g.Writer(tx)
		wv.RemoveNode("sleeper")
		_ = tx.w.conflictErr(mvcc.StoreNodeExistence, ^uint64(0))
		return tx.w.err()
	})
	if err == nil {
		t.Fatal("the doomed delete reported success, so Graph.reviveAborted was never reached")
	}

	if g.IsTombstoned(id) {
		t.Fatal("setup: withdrawing the aborted delete should have revived the node")
	}
	if g.nodeIdx.Has(uint32(lid), id) {
		t.Fatal("setup: the revival records no bitmap entry, so Kept's should still be absent " +
			"— otherwise there is nothing for the add-back to restore")
	}
	if got := g.labelChurn.load(lid); got == 0 {
		t.Fatal("reviveAborted brought a node back alive with its Kept bitmap entry still " +
			"missing and Kept's gate at zero: a reader will take the raw bitmap and LOSE the " +
			"row, which no per-row predicate can recover (rmp #2686)")
	}

	if bm := g.LabelBitmapAsOf(lid, nil); !bm.Contains(uint64(id)) {
		t.Fatal("a reader LOST a live node from the label bitmap it belongs in: the add-back " +
			"was skipped because Graph.reviveAborted did not pin the per-label churn gate")
	}
}

// TestLabelChurnGate_ScopedHoldSpansTheTombstoneFlip is the guard on the hold
// [Graph.removeNodeInfo] takes across the WHOLE retirement.
//
// # The window it covers, and why nothing else does
//
// On the autocommit path the three steps of a retirement do not happen together:
// the tombstone bitmap flips first, the label bitmaps are stripped second — which
// is where the deferred removals take their holds — and the death record is
// written last, in a deferred call. Between the flip and the strip the node is
// dead, still in every label bitmap, and holding nothing. The scoped hold is what
// covers that instant.
//
// # Why the fixture looks like this
//
// The node has to be REACHABLE by the correction during the window, or the gate
// makes no difference: a node that is merely tombstoned is in no suspect source
// at all, and no reader can correct it whatever the gate says (that gap is
// pre-existing and is measured in [settledDead]). So every node here carries a
// second label whose delta is pinned open by a long-lived reader: that keeps it
// in the suspect set while leaving the SCANNED label's gate at zero, so the
// scoped hold is the only thing that can raise it.
//
// The window is microseconds wide inside one function call, so this drives it
// many times rather than constructing it. It is the one guard in this file that
// is probabilistic; the counter assertion beneath it is not.
func TestLabelChurnGate_ScopedHoldSpansTheTombstoneFlip(t *testing.T) {
	const population = 300
	keys := make([]string, population)
	for i := range keys {
		keys[i] = fmt.Sprintf("s%d", i)
	}
	g, lid := churnFixture(t, "Scanned", keys...)
	defer func() { _ = g.Close() }()
	ids := make([]graph.NodeID, len(keys))
	for i, k := range keys {
		ids[i] = mustID(t, g, k)
	}

	// Pinned open so the Marker deltas below are never reclaimed.
	hold := g.BeginRead()
	defer g.EndRead(hold)
	for _, k := range keys {
		if err := g.SetNodeLabel(k, "Marker"); err != nil {
			t.Fatalf("SetNodeLabel(Marker): %v", err)
		}
	}
	if g.churnLive(oneLabel(lid)) {
		t.Fatalf("setup: Scanned's gate reads %d before any retirement, so the scoped hold "+
			"would not be the thing raising it", g.labelChurn.load(lid))
	}
	if len(g.suspectNodes()) == 0 {
		t.Fatal("setup: the Marker writes left no suspects, so no correction could reach " +
			"these nodes during the window and this test cannot discriminate")
	}

	dead := func() *roaring64.Bitmap {
		out := roaring64.New()
		for _, id := range g.TombstonedIDs() {
			out.Add(uint64(id))
		}
		return out
	}

	var (
		stop  atomic.Bool
		wg    sync.WaitGroup
		reads atomic.Int64
		bad   atomic.Int64
	)
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				before := dead()
				got := g.LabelBitmapAsOf(lid, nil)
				after := dead()
				reads.Add(1)
				if v := roaring64.And(roaring64.And(before, after), got); !v.IsEmpty() {
					if bad.Add(1) == 1 {
						t.Errorf("a reader saw %v in the Scanned bitmap while they were "+
							"tombstoned both before and after the read. The retirement's "+
							"scoped churn hold did not span the tombstone flip, so the gate "+
							"was shut in the window between the flip and the bitmap strip "+
							"(rmp #2686).", v.ToArray())
					}
				}
			}
		}()
	}
	for _, k := range keys {
		g.RemoveNode(k)
	}
	stop.Store(true)
	wg.Wait()

	if reads.Load() == 0 {
		t.Fatal("the readers completed no read, so nothing was asserted")
	}
	if bm := g.LabelBitmapAsOf(lid, nil); !roaring64.And(dead(), bm).IsEmpty() {
		t.Fatalf("with the workload stopped, the Scanned bitmap still reports tombstoned "+
			"nodes %v", roaring64.And(dead(), bm).ToArray())
	}
	_ = ids
	t.Logf("%d reads asserted across %d retirements", reads.Load(), population)
}
