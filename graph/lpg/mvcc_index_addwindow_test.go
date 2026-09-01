package lpg

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// The guards for the label-index ADD window opened by rmp #2681.
//
// [Graph.setNodeLabelInfo] may apply a label's bitmap entry AFTER releasing the
// bag's shard lock, so for a few hundred nanoseconds the bag says the label is
// present and the raw bitmap does not. rmp #2308 named that direction
// unrecoverable — a present-time reader taking the raw bitmap misses the node
// entirely — so the two preconditions that make it recoverable are load-bearing,
// and each of these tests fails against a build without one of them:
//
//   - the write must be VERSIONED, so the node is in the suspect set and
//     [Graph.correctBitmapOver] can add the member back. Pinned by
//     TestLabelIndexAddWindow_BitmapReaderNeverLosesARow in its "unversioned"
//     case, which goes red against a build whose hoist ignores `versioned`.
//   - idxAddActive must force a present-time reader to filter, because the COUNT
//     path has no suspect correction to fall back on and would serve the raw,
//     short count. Pinned by
//     TestLabelIndexAddWindow_PresentTimeCountDeclinesWhileAnAddIsInFlight, which
//     goes red against a build whose [Graph.labelBitmapNeedsFilter] ignores
//     idxAddActive.

// addWindowNodes is the node population every case in this file labels, and
// addWindowRounds the number of DISTINCT labels each node is given. A fresh
// label per round matters: re-asserting a label a node already carries records
// no version and applies no bitmap entry, so a single round would leave the
// window open for a few milliseconds and the readers would mostly sample a
// finished graph.
const (
	addWindowNodes  = 3000
	addWindowRounds = 12
)

// addWindowKey names node i of that population.
func addWindowKey(i int) string { return fmt.Sprintf("awn%d", i) }

// addWindowLabel names the label written in round r.
func addWindowLabel(r int) string { return fmt.Sprintf("Hot%d", r) }

// seedAddWindowGraph interns addWindowNodes nodes carrying no label yet, and
// returns their ids in creation order.
func seedAddWindowGraph(t *testing.T, g *Graph[string, float64]) []graph.NodeID {
	t.Helper()
	ids := make([]graph.NodeID, addWindowNodes)
	for i := range addWindowNodes {
		key := addWindowKey(i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode(%s): %v", key, err)
		}
		id, ok := g.adj.Mapper().Lookup(key)
		if !ok {
			t.Fatalf("node %s was not interned", key)
		}
		ids[i] = id
	}
	return ids
}

// TestLabelIndexAddWindow_BitmapReaderNeverLosesARow asserts the invariant the
// hoisted bitmap add must not break: a reader that has ALREADY seen the label in
// a node's bag must find that node in the label bitmap it reads next.
//
// The bag read happens first and the bitmap read second, so the writer's bag
// effect is established as visible before the bitmap is consulted. A bitmap that
// then omits the node is a LOST ROW, which no later predicate can recover.
//
// The "unversioned" case is the mutation-validated one: with the versioning
// substrate disarmed no delta is recorded, so nothing puts the node in the
// suspect set and nothing can add the member back. The shipped code refuses to
// hoist such a write; a build that hoists it regardless fails here.
func TestLabelIndexAddWindow_BitmapReaderNeverLosesARow(t *testing.T) {
	cases := []struct {
		name     string
		disarmed bool
	}{
		{name: "versioned"},
		{name: "unversioned", disarmed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			if tc.disarmed {
				g.disarmMVCCForTest()
			}
			ids := seedAddWindowGraph(t, g)
			lids := make([]LabelID, addWindowRounds)
			for r := range addWindowRounds {
				lids[r] = g.reg.Intern(addWindowLabel(r))
			}

			const (
				writers = 16
				readers = 8
			)
			var (
				writersWG sync.WaitGroup
				readersWG sync.WaitGroup
				stop      atomic.Bool
				lost      atomic.Int64
				lostID    atomic.Int64
				seen      atomic.Int64 // bag-present observations the readers made
				peakOpen  atomic.Int64 // highest idxAddActive any reader observed
			)
			ctx := context.Background()

			for r := range readers {
				readersWG.Add(1)
				go func(seed int) {
					defer readersWG.Done()
					next := uint64(seed)*2654435761 + 1
					localPeak := int64(0)
					for !stop.Load() {
						next = next*6364136223846793005 + 1442695040888963407
						id := ids[int(next>>33)%len(ids)]
						lid := lids[int(next>>17)%len(lids)]
						if v := g.idxAddActive.Load(); v > localPeak {
							localPeak = v
						}
						// The bag is read FIRST: once it says the label is
						// there, the bitmap read that follows may not omit it.
						if !g.labelBagTest(id, nil, func(b labelBag) bool { return b.has(lid) }) {
							continue
						}
						seen.Add(1)
						if bm := g.LabelBitmapAsOf(lid, nil); !bm.Contains(uint64(id)) {
							lost.Add(1)
							lostID.Store(int64(id))
						}
					}
					for {
						old := peakOpen.Load()
						if localPeak <= old || peakOpen.CompareAndSwap(old, localPeak) {
							return
						}
					}
				}(r)
			}

			for w := range writers {
				writersWG.Add(1)
				go func(worker int) {
					defer writersWG.Done()
					for round := range addWindowRounds {
						name := addWindowLabel(round)
						for i := worker; i < len(ids); i += writers {
							key := addWindowKey(i)
							var err error
							if tc.disarmed {
								err = g.SetNodeLabel(key, name)
							} else {
								err = g.ApplyVersionedCtx(ctx, func(tx WriteTx) error {
									return g.Writer(tx).SetNodeLabel(key, name)
								})
							}
							if err != nil {
								t.Errorf("worker %d round %d: SetNodeLabel(%s, %s): %v", worker, round, key, name, err)
								return
							}
						}
					}
				}(w)
			}

			writersWG.Wait()
			stop.Store(true)
			readersWG.Wait()

			if lost.Load() != 0 {
				t.Fatalf("%d bitmap reads lost a row whose bag already carried the label (last: node id %d): "+
					"the label-index add window was observed", lost.Load(), lostID.Load())
			}
			if seen.Load() == 0 {
				t.Fatal("no reader ever observed the label in a bag: the test never reached its own assertion")
			}
			if !tc.disarmed && peakOpen.Load() < 2 {
				t.Fatalf("idxAddActive never exceeded %d, so no write took the hoisted path and the "+
					"window under test was never opened", peakOpen.Load())
			}
		})
	}
}

// TestLabelIndexAddWindow_PresentTimeCountDeclinesWhileAnAddIsInFlight pins the
// half of the window that the suspect correction cannot cover.
//
// [Graph.LabelCountExact] answers from the RAW bitmap whenever
// [Graph.labelBitmapNeedsFilter] says no filter is needed. Unlike a scan it has
// no object to re-check and takes no suspect walk, so an answer given while a
// hoisted add is still in flight is SHORT by the adds in flight — and it is
// short at every instant inside the call, which makes it a wrong answer rather
// than a stale one. idxAddActive is what makes it decline instead.
//
// The assertion is made against the counter directly rather than against a race,
// deliberately. The window is a few hundred nanoseconds wide and a reader cannot
// establish the true present-time population any faster than it can walk every
// bag, which is three orders of magnitude slower — so a concurrent oracle
// measures the walk, not the window, and stays green against a build with no
// gate at all (measured: 0 failures in 5 race-enabled runs). Driving the counter
// states the rule the window depends on and fails the moment the rule is
// dropped.
func TestLabelIndexAddWindow_PresentTimeCountDeclinesWhileAnAddIsInFlight(t *testing.T) {
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	if err := g.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	err := g.ApplyVersionedCtx(context.Background(), func(tx WriteTx) error {
		return g.Writer(tx).SetNodeLabel("a", "Hot")
	})
	if err != nil {
		t.Fatalf("SetNodeLabel: %v", err)
	}
	lid := g.reg.Intern("Hot")

	// Precondition: with nothing in flight a present-time count is exact, so the
	// assertion below is a change of behaviour and not the standing answer.
	if _, ok := g.LabelCountExact(lid, nil); !ok {
		t.Fatal("a quiescent graph declined an exact present-time count: the test cannot " +
			"tell the gate apart from the ordinary answer")
	}
	if g.labelBitmapNeedsFilter(nil) {
		t.Fatal("a quiescent graph wants a present-time filter: same problem")
	}

	// One label-index add claimed in the bag and not yet in the bitmap.
	g.idxAddActive.Add(1)
	if !g.labelBitmapNeedsFilter(nil) {
		t.Fatal("a present-time reader was not made to filter while a label-index add was in " +
			"flight: it would be served the raw bitmap, which is short by that add")
	}
	if n, ok := g.LabelCountExact(lid, nil); ok {
		t.Fatalf("LabelCountExact answered %d from the raw bitmap while a label-index add was "+
			"in flight: a count has nothing to re-check against, so a short answer is final", n)
	}
	g.idxAddActive.Add(-1)

	// And the gate clears, so the pessimism is confined to the window.
	if _, ok := g.LabelCountExact(lid, nil); !ok {
		t.Fatal("the present-time count stayed inexact after the add landed: the gate does not clear")
	}
}
