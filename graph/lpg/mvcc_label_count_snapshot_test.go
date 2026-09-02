package lpg

import (
	"context"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// labelCountProbeBatch is the atomic write unit rmp #2688's oracle is stated
// against: internal/sim's ST7 commits one transaction per this many nodes, so a
// count that is NOT a multiple of it is a PARTIALLY APPLIED transaction — an
// Atomicity break, not merely a stale read. The seam below labels ONE node, so
// the number the defective build returns is 1 past a whole batch: exactly the
// 481-after-480 and 266-instead-of-265 signatures ST7 measured.
const labelCountProbeBatch = 5

// newLabelCountProbeGraph builds a graph with seeded labelled nodes, spare
// unlabelled ones for the racing write, and NO live history, and returns it with
// the label id. A drained substrate is a precondition: with the gate already up
// the EXACT branch never answers and nothing below could fail.
func newLabelCountProbeGraph(t *testing.T, seeded, spare int) (*Graph[string, float64], LabelID) {
	t.Helper()
	ctx := context.Background()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < seeded+spare; i++ {
		if err := g.AddNode(fmt.Sprintf("n%d", i)); err != nil {
			t.Fatalf("AddNode(n%d): %v", i, err)
		}
	}
	if err := g.ApplyVersionedCtx(ctx, func(tx WriteTx) error {
		w := g.Writer(tx)
		for i := 0; i < seeded; i++ {
			if err := w.SetNodeLabel(fmt.Sprintf("n%d", i), "P"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed labels: %v", err)
	}
	g.ReclaimNow()
	return g, g.reg.Intern("P")
}

// labelOne commits one node's label in its own transaction.
func labelOne(g *Graph[string, float64], idx int) error {
	return g.ApplyVersionedCtx(context.Background(), func(tx WriteTx) error {
		return g.Writer(tx).SetNodeLabel(fmt.Sprintf("n%d", idx), "P")
	})
}

// TestLabelCountExact_DeclinesWhenHistoryGoesLiveDuringTheCall is the rmp #2688
// regression gate.
//
// # The defect
//
// [Graph.LabelCountExact] sampled [Graph.labelBitmapNeedsFilter] ONCE, BEFORE
// reading the raw cardinality, and never re-checked. A write raises that gate
// ([nodeLabelShard.pushLabelDelta], under the label shard's write lock) BEFORE it
// touches the index ([Graph.setNodeLabelInfo] calls nodeIdx.Add afterwards), and
// a pinned snapshot forbids reclaiming the delta — so a gate reading taken
// before the write, paired with a cardinality read taken after it, returns a
// PRESENT-TIME number reported as EXACT for a snapshot that predates it.
// [label.Index.Count] takes its own read lock, so it lands BETWEEN the individual
// Add calls of a multi-node transaction and the number can be MID-BATCH.
//
// Measured end to end by internal/sim ST7 before the fix: two counts in ONE read
// transaction returned 480 then 481, and 266 then 265. With a batch size of 5
// neither 481 nor 266 is a whole transaction, and the DECREASE is this exact
// branch answering first and the filtered branch answering second.
//
// # Why this is driven and not raced
//
// A concurrent oracle CANNOT pin this window, which was measured rather than
// assumed: against the DEFECTIVE build a 3-reader race produced 110 exact
// answers and 0 violations in a whole run, and a 96-reader race did not finish.
// The window is a few nanoseconds between two atomic loads. This is the same
// conclusion, for the same function, that
// TestLabelIndexAddWindow_PresentTimeCountDeclinesWhileAnAddIsInFlight records
// for the present-time add window. So the write is driven INTO the window
// through [Graph.labelCountGateProbe], and the seam's own fire counter is
// asserted — a test that did not enter the window must fail, not pass quietly.
func TestLabelCountExact_DeclinesWhenHistoryGoesLiveDuringTheCall(t *testing.T) {
	const seeded = 2 * labelCountProbeBatch // a whole number of batches
	g, lid := newLabelCountProbeGraph(t, seeded, 8)

	snap := g.BeginRead()
	defer g.EndRead(snap)

	// Precondition: on a drained graph this snapshot gets an EXACT answer, so a
	// decline below is a CHANGE of behaviour and not the standing answer.
	if n, ok := g.LabelCountExact(lid, snap); !ok || n != seeded {
		t.Fatalf("precondition: a drained graph answered (%d, %v), want (%d, true); the window "+
			"under test is unreachable and this test cannot fail", n, ok, seeded)
	}

	fired := 0
	g.labelCountGateProbe = func() {
		if fired > 0 {
			return // once: the nested reads below must not re-enter
		}
		fired++
		if err := labelOne(g, seeded); err != nil {
			t.Errorf("seam write: %v", err)
		}
	}
	n, ok := g.LabelCountExact(lid, snap)
	g.labelCountGateProbe = nil

	if fired == 0 {
		t.Fatal("the seam never fired, so no write landed inside the call: this test did not " +
			"enter the window it exists to close")
	}
	if ok {
		t.Fatalf("LabelCountExact returned (%d, EXACT) for a snapshot pinned when the count was "+
			"%d, after a write landed INSIDE the call. %d is not a multiple of the batch size "+
			"%d, so it exposes a PARTIALLY APPLIED transaction (Atomicity) as well as a value "+
			"from after the snapshot (Isolation). The gate must be re-sampled after the "+
			"cardinality (rmp #2688).",
			n, seeded, n, labelCountProbeBatch)
	}

	// The CONTROL: the scan path, under the identical interleaving, is still
	// right. It samples its suspect set before AND after its clone
	// (rmp #2326/#2686), which is the property the count path was missing. If
	// this ever fails the defect is NOT confined to the count path.
	if got := int64(g.LabelBitmapAsOf(lid, snap).GetCardinality()); got != seeded {
		t.Fatalf("the SCAN control answered %d, want %d: the defect is not confined to the count "+
			"path and the fix must be widened", got, seeded)
	}

	// And the pessimism is CONFINED to the window: once the history the seam
	// created is drained, exact answers resume rather than the gate latching.
	g.EndRead(snap)
	g.ReclaimNow()
	after := g.BeginRead()
	defer g.EndRead(after)
	if n, ok := g.LabelCountExact(lid, after); !ok || n != seeded+1 {
		t.Fatalf("after draining, LabelCountExact answered (%d, %v), want (%d, true): the gate "+
			"latched instead of clearing", n, ok, seeded+1)
	}
}
