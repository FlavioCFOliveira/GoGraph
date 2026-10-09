package lpg

// mvcc_label_bitmap_cancel_3010_test.go — rmp #3010 and #3011.
//
// The MVCC correction of a label bitmap is O(suspects). rmp #3010 made it
// observe a context, with one hard rule: a cancelled correction returns the
// context's error and NEVER a partially corrected bitmap. rmp #3011 replaced the
// sort that deduplicated the suspects with a bitset for dense id ranges; the
// two must agree exactly.
//
// Layer: short. Race-clean.

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// flipCtx reports no error for its first `after` Err calls and
// context.Canceled from then on, so a test can land a cancellation at an exact
// check inside a correction rather than at a wall-clock instant.
type flipCtx struct {
	context.Context //nolint:containedctx // the type IS a context: it overrides Err only
	after           int32
	calls           atomic.Int32
}

func (c *flipCtx) Err() error {
	if c.calls.Add(1) > c.after {
		return context.Canceled
	}
	return nil
}

// churnedLabelGraph returns a graph whose label L holds `committed` committed
// nodes visible to the returned snapshot and `uncommitted` more created by a
// transaction that stays open for the life of the test, so every read of L at
// the snapshot must correct the raw bitmap over the uncommitted nodes.
func churnedLabelGraph(t *testing.T, committed, uncommitted int) (*Graph[string, float64], LabelID, *Snapshot) {
	t.Helper()
	ctx := context.Background()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		for i := range committed {
			k := fmt.Sprintf("c%d", i)
			if err := g.Writer(tx).AddNode(k); err != nil {
				return err
			}
			if err := g.Writer(tx).SetNodeLabel(k, "L"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	g.ReclaimNow()
	snap := g.BeginRead()
	t.Cleanup(func() { g.EndRead(snap) })
	hold := g.BeginVersionedTx()
	t.Cleanup(func() {
		hold.Abandon()
		g.EndVersionedTx(hold)
	})
	if err := g.ApplyInVersionedTx(ctx, hold, func(tx WriteTx) error {
		for i := range uncommitted {
			k := fmt.Sprintf("u%d", i)
			if err := g.Writer(tx).AddNode(k); err != nil {
				return err
			}
			if err := g.Writer(tx).SetNodeLabel(k, "L"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("uncommitted: %v", err)
	}
	lid, ok := g.reg.Lookup("L")
	if !ok {
		t.Fatal("label L not interned")
	}
	return g, lid, snap
}

// TestLabelBitmapAsOfContext_CancelledNeverReturnsAPartialBitmap lands the
// cancellation at each of the correction's checks in turn — after the first
// suspect sample, after deduplication, and inside the per-suspect walk, where the
// private copy is already half corrected — and asserts the same outcome at every
// one: no bitmap, the context's error, the shared index image untouched, and an
// uncancelled read afterwards still exact.
func TestLabelBitmapAsOfContext_CancelledNeverReturnsAPartialBitmap(t *testing.T) {
	const committed, uncommitted = 100, 2 * correctCtxStride
	g, lid, snap := churnedLabelGraph(t, committed, uncommitted)

	rawBefore := g.nodeIdx.BitmapShared(uint32(lid)).GetCardinality()
	if rawBefore != committed+uncommitted {
		t.Fatalf("precondition: raw index holds %d, want %d", rawBefore, committed+uncommitted)
	}
	for _, tc := range []struct {
		name  string
		after int32
	}{
		{"after the first sample", 0},
		{"after deduplication", 1},
		{"inside the per-suspect walk", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &flipCtx{Context: context.Background(), after: tc.after}
			bm, err := g.LabelBitmapAsOfContext(c, lid, snap)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			if bm != nil {
				t.Fatalf("a cancelled correction returned a bitmap of %d members; it must return none",
					bm.GetCardinality())
			}
			if got := c.calls.Load(); got != tc.after+1 {
				t.Fatalf("the context was read %d times; the cancellation was meant to land on read %d",
					got, tc.after+1)
			}
			if got := g.nodeIdx.BitmapShared(uint32(lid)).GetCardinality(); got != rawBefore {
				t.Fatalf("the shared index image changed from %d to %d: the abandoned correction "+
					"mutated a bitmap it did not own", rawBefore, got)
			}
			n, err := g.LabelCountAsOfContext(&flipCtx{Context: context.Background(), after: tc.after}, lid, snap)
			if !errors.Is(err, context.Canceled) || n != 0 {
				t.Fatalf("LabelCountAsOfContext = (%d, %v), want (0, context.Canceled)", n, err)
			}
		})
	}
	bm, err := g.LabelBitmapAsOfContext(context.Background(), lid, snap)
	if err != nil {
		t.Fatalf("uncancelled read: %v", err)
	}
	if got := bm.GetCardinality(); got != committed {
		t.Fatalf("uncancelled read after the aborts holds %d members, want the snapshot's %d", got, committed)
	}
	if got := g.LabelBitmapAsOf(lid, snap).GetCardinality(); got != committed {
		t.Fatalf("LabelBitmapAsOf holds %d members, want %d", got, committed)
	}
}

// TestLabelBitmapAsOfContext_QuietPathIgnoresTheContext pins that a read with
// nothing to correct does not consult the context at all, so a read-only
// workload pays nothing for it — and is therefore answered even when cancelled.
func TestLabelBitmapAsOfContext_QuietPathIgnoresTheContext(t *testing.T) {
	g, lid, snap := churnedLabelGraph(t, 10, 0)
	c := &flipCtx{Context: context.Background(), after: 0}
	bm, err := g.LabelBitmapAsOfContext(c, lid, snap)
	if err != nil || bm == nil || bm.GetCardinality() != 10 {
		t.Fatalf("quiet read = (%v, %v), want 10 members and no error", bm, err)
	}
	if got := c.calls.Load(); got != 0 {
		t.Fatalf("the quiet path read the context %d times, want 0", got)
	}
}

// TestDedupSuspects_MatchesSortCompact holds the bitset path and the sort path
// to the same answer: the distinct ids, ascending. Dense unions take the bitset,
// sparse ones the sort; both are generated, with duplicates, in random order.
func TestDedupSuspects_MatchesSortCompact(t *testing.T) {
	r := rand.New(rand.NewPCG(3010, 3011)) //nolint:gosec // a seeded, reproducible test input, not a secret
	for _, tc := range []struct {
		name       string
		n, k, span int
	}{
		{"empty", 0, 1, 1},
		{"one", 1, 3, 1},
		{"dense small", 8, 4, 8},
		{"dense", 5000, 4, 6000},
		{"dense at a high base", 3000, 2, 3000},
		{"sparse", 50, 2, 1 << 40},
		{"just sparse", 64, 1, 64*64*2 + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := graph.NodeID(r.Uint64N(1 << 50))
			ids := make([]graph.NodeID, 0, tc.n*tc.k)
			distinct := make([]graph.NodeID, tc.n)
			for i := range distinct {
				distinct[i] = base + graph.NodeID(r.Uint64N(uint64(tc.span)))
			}
			for range tc.k {
				r.Shuffle(len(distinct), func(i, j int) { distinct[i], distinct[j] = distinct[j], distinct[i] })
				ids = append(ids, distinct...)
			}
			want := slices.Clone(ids)
			slices.Sort(want)
			want = slices.Compact(want)
			if got := dedupSuspects(ids); !slices.Equal(got, want) {
				t.Fatalf("dedupSuspects returned %d ids, want %d distinct ascending ids", len(got), len(want))
			}
		})
	}
}

// TestLabelsCountBound_NeverUnderCounts holds the conjunction bound to its
// contract: exact and equal to the exact count with no history live, and never
// below the exact count — with exact reported false — while uncommitted nodes
// carry both labels.
func TestLabelsCountBound_NeverUnderCounts(t *testing.T) {
	ctx := context.Background()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	mk := func(tx WriteTx, k string, labels ...string) error {
		if err := g.Writer(tx).AddNode(k); err != nil {
			return err
		}
		for _, l := range labels {
			if err := g.Writer(tx).SetNodeLabel(k, l); err != nil {
				return err
			}
		}
		return nil
	}
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		for i := range 20 {
			if err := mk(tx, fmt.Sprintf("ab%d", i), "A", "B"); err != nil {
				return err
			}
			if err := mk(tx, fmt.Sprintf("a%d", i), "A"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	g.ReclaimNow()
	a, _ := g.reg.Lookup("A")
	b, _ := g.reg.Lookup("B")
	lids := []LabelID{a, b}
	snap := g.BeginRead()
	defer g.EndRead(snap)

	n, exact, ok := g.LabelsCountBound(lids, snap)
	if !ok || !exact || n != 20 {
		t.Fatalf("quiet LabelsCountBound = (%d, %v, %v), want (20, true, true)", n, exact, ok)
	}
	hold := g.BeginVersionedTx()
	defer func() {
		hold.Abandon()
		g.EndVersionedTx(hold)
	}()
	if err := g.ApplyInVersionedTx(ctx, hold, func(tx WriteTx) error {
		for i := range 30 {
			if err := mk(tx, fmt.Sprintf("u%d", i), "A", "B"); err != nil {
				return err
			}
		}
		return g.Writer(tx).RemoveNodeLabel("ab0", "B")
	}); err != nil {
		t.Fatalf("uncommitted: %v", err)
	}
	want, _ := g.LabelsCountExact(lids, snap)
	n, exact, ok = g.LabelsCountBound(lids, snap)
	if !ok || exact || n < want {
		t.Fatalf("churned LabelsCountBound = (%d, %v, %v), want an inexact bound >= the exact %d", n, exact, ok, want)
	}
	if want != 20 {
		t.Fatalf("precondition: the snapshot's exact conjunction is %d, want 20", want)
	}
	if _, _, ok := g.LabelsCountBound(lids[:1], snap); ok {
		t.Fatal("a single label is not a conjunction; ok must be false")
	}
}
