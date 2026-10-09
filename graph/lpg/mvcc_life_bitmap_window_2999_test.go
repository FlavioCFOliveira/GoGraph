package lpg

// mvcc_life_bitmap_window_2999_test.go — regression for rmp #2999.
//
// [Graph.NodeExistsAsOf] falls back to the tombstone bitmap for a node with no
// life record. It used to read the bitmap AFTER releasing the life shard lock,
// so a removal could record its death and flip the bitmap in between: the
// reader saw no record and a dead bitmap, and a node alive at its instant read
// as gone. A CSR built at that instant dropped every arc of the node, so a count
// repeated inside one read transaction moved.
//
// The window is a few instructions wide, so this is a targeted stress rather
// than a scripted interleaving: writers delete and roll back nodes that
// readers, pinned before every write, must keep seeing. Against the unfixed read
// it failed 8 runs out of 8, with 50 to 69 lost reads per 300 ms run.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

func TestNodeExistsAsOf_BitmapReadUnderTheLifeLock_2999(t *testing.T) {
	const nodes, writers, readers = 8, 4, 4
	const budget = 300 * time.Millisecond
	ctx := context.Background()
	g := New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	t.Cleanup(func() { _ = g.Close() })
	keys := make([]string, nodes)
	for i := range keys {
		keys[i] = fmt.Sprintf("n%d", i)
	}
	if err := g.ApplyVersioned(func(tx WriteTx) error {
		for _, k := range keys {
			if err := g.Writer(tx).AddNode(k); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ids := make([]graph.NodeID, nodes)
	for i, k := range keys {
		ids[i], _ = g.adj.Mapper().Lookup(k)
	}
	snap := g.BeginRead()
	defer g.EndRead(snap)
	g.ReclaimNow() // no life record left: every read takes the bitmap fallback
	for _, id := range ids {
		if b, hb, _, hd := lifePair(g, id); hb || hd {
			t.Fatalf("precondition: node %d still has a life record (born at %d)", id, b.at())
		}
	}

	var stop atomic.Bool
	var lost, reads, cycles atomic.Int64
	var wg sync.WaitGroup
	for r := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := r; !stop.Load(); i++ {
				reads.Add(1)
				if !g.NodeExistsAsOf(ids[i%nodes], snap) {
					lost.Add(1)
				}
			}
		}()
	}
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; !stop.Load(); i += writers {
				k := keys[i%nodes]
				tx := g.BeginVersionedTx()
				_ = g.ApplyInVersionedTx(ctx, tx, func(wtx WriteTx) error {
					_, _ = g.Writer(wtx).RemoveNode(k)
					return nil
				})
				tx.EnterUndo()
				_ = g.ApplyInVersionedTx(ctx, tx, func(wtx WriteTx) error {
					_ = g.Writer(wtx).Revive(k)
					return nil
				})
				tx.ExitUndo()
				tx.Abandon()
				g.EndVersionedTx(tx)
				cycles.Add(1)
			}
		}()
	}
	time.Sleep(budget)
	stop.Store(true)
	wg.Wait()
	if cycles.Load() == 0 || reads.Load() == 0 {
		t.Fatalf("nothing ran: %d write cycles, %d reads", cycles.Load(), reads.Load())
	}
	if n := lost.Load(); n != 0 {
		t.Errorf("a reader pinned before every write lost a live node %d times in %d reads (%d rolled-back deletes)",
			n, reads.Load(), cycles.Load())
	}
}
