package lpg

// mvcc_label_departed_2999_test.go — regression for rmp #2999.
//
// [Graph.LabelBitmapAsOf] corrects the shared label image only over the
// suspects it samples before and after acquiring it. An aborted CREATE puts its
// node into the image and withdraws it — node, label delta, life record, gate
// hold — entirely between the two samples, so neither names it while the
// acquired image still holds it. A reader older than that node then counted it,
// transiently: measured by a targeted stress of example 37's L16 workload.

import (
	"context"
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestLabelBitmapAsOf_AbortedCreateBetweenTheSamples_2999(t *testing.T) {
	for _, tc := range []struct {
		name string
		// gateLive keeps the label's churn gate raised across the read by an
		// unrelated in-flight label write, so the read takes the corrected path
		// rather than the quiet one.
		gateLive bool
	}{{"quiet gate", false}, {"live gate", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			t.Cleanup(func() { _ = g.Close() })
			for _, k := range []string{"a", "b"} {
				if err := g.ApplyVersioned(func(tx WriteTx) error {
					if err := g.Writer(tx).AddNode(k); err != nil {
						return err
					}
					return g.Writer(tx).SetNodeLabel(k, "L")
				}); err != nil {
					t.Fatalf("seed %s: %v", k, err)
				}
			}
			lid, _ := g.reg.Lookup("L")
			snap := g.BeginRead()
			defer g.EndRead(snap)
			g.ReclaimNow() // the seed's records are in every reader's past

			if tc.gateLive {
				hold := g.BeginVersionedTx()
				defer g.EndVersionedTx(hold)
				if err := g.ApplyInVersionedTx(ctx, hold, func(tx WriteTx) error {
					return g.Writer(tx).RemoveNodeLabel("b", "L")
				}); err != nil {
					t.Fatalf("hold: %v", err)
				}
			}
			if live := g.churnLive(oneLabel(lid)); live != tc.gateLive {
				t.Fatalf("precondition: churn gate live=%v, want %v", live, tc.gateLive)
			}

			var acquired *roaring64.Bitmap
			got, _ := g.labelBitmapAsOfFiltered(context.Background(), snap, oneLabel(lid), func() (*roaring64.Bitmap, bool) {
				// A transaction creates a labelled node, the image is acquired
				// while it is a member, and the transaction aborts — all
				// between the pre-acquire and the post-acquire samples.
				x := g.BeginVersionedTx()
				if err := g.ApplyInVersionedTx(ctx, x, func(tx WriteTx) error {
					if err := g.Writer(tx).AddNode("n"); err != nil {
						return err
					}
					return g.Writer(tx).SetNodeLabel("n", "L")
				}); err != nil {
					t.Fatalf("create: %v", err)
				}
				acquired = g.nodeIdx.BitmapShared(uint32(lid))
				x.Abandon()
				g.EndVersionedTx(x)
				return acquired, false
			}, func(bag labelBag) bool { return bag.has(lid) })

			n, ok := g.adj.Mapper().Lookup("n")
			if !ok || !acquired.Contains(uint64(n)) {
				t.Fatalf("precondition: the acquired image must hold the aborted node (interned=%v)", ok)
			}
			if g.nodeIdx.BitmapShared(uint32(lid)).Contains(uint64(n)) {
				t.Fatal("precondition: the abort must have taken the node out of the index")
			}
			if got.Contains(uint64(n)) {
				t.Errorf("a reader older than an aborted CREATE counts its node: %v", got.ToArray())
			}
			a, _ := g.adj.Mapper().Lookup("a")
			b, _ := g.adj.Mapper().Lookup("b")
			if !got.Contains(uint64(a)) || !got.Contains(uint64(b)) {
				t.Errorf("the correction lost a committed member: %v", got.ToArray())
			}
		})
	}
}
