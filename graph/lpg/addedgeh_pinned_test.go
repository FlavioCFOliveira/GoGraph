package lpg_test

// addedgeh_pinned_test.go — a durable store commit does not duplicate a
// committed edge handle after an earlier op of its apply pinned the peer that
// wrote it as invisible (rmp #3032).
//
// A store commit's apply reads through ONE snapshot for all of its ops.
// OpRemoveNode's first strip classified a peer's commit record in flight, and
// the snapshot pinned it invisible. The peer then committed. OpAddEdgeH found the
// peer's handle in the stored entry, passed the adjacency admit check, asked the
// pinned snapshot whether the handle existed, was told no, and inserted a second
// slot with the same handle. Replay is idempotent on the handle, so recovery held
// one arc where memory held two. Found by SPIKE #3031.
//
// Layer: short.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func TestAddEdgeH_DoesNotReadAPinCarriedFromAnEarlierOp(t *testing.T) {
	type stx = txn.Tx[string, float64]
	const h = uint64(7)
	countH := func(g *lpg.Graph[string, float64]) int {
		n := 0
		g.WalkEdgeHandles(func(e lpg.EdgeHandleTriple) bool {
			if e.Handle == h {
				n++
			}
			return true
		})
		return n
	}
	// removed = the node the commit removes before its AddEdgeH. "z" carries the
	// peer's label, so the first strip classifies the peer in flight; "w" is a
	// control the peer never touches, so no pin exists.
	for _, removed := range []string{"z", "w"} {
		t.Run("remove "+removed, func(t *testing.T) {
			dir := t.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatal(err)
			}
			g := lpg.New[string, float64](adjlist.Config{})
			opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
			st := txn.NewStoreWithOptions[string, float64](g, w, opts)
			commit := func(f func(tx *stx) error) error {
				tx := st.Begin()
				if err := f(tx); err != nil {
					return err
				}
				return tx.Commit()
			}
			if err := commit(func(tx *stx) error {
				for _, n := range []string{"x", "y", "z", "w"} {
					if err := tx.AddNode(n); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			// Peer: label on z plus the handle-h arc, held in its durable step.
			inPeer, releasePeer, peerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				peerDone <- g.ApplyDurable(context.Background(), func(wtx lpg.WriteTx) error {
					wv := g.Writer(wtx)
					if err := wv.SetNodeLabel("z", "L"); err != nil {
						return err
					}
					_, err := wv.AddEdgeHIfAbsent("x", "y", 0, h)
					return err
				}, func() error {
					close(inPeer)
					<-releasePeer
					ptx := st.Begin()
					if err := ptx.SetNodeLabel("z", "L"); err != nil {
						return err
					}
					if err := ptx.AddEdgeWithHandle("x", "y", 0, h); err != nil {
						return err
					}
					return ptx.CommitWALOnly(0)
				})
			}()
			<-inPeer

			fired := false
			g.SetNodeRemovalEntryHookForTest(func() {
				if fired {
					return
				}
				fired = true
				close(releasePeer)
				if err := <-peerDone; err != nil {
					t.Errorf("peer commit: %v", err)
				}
			})
			if err := commit(func(tx *stx) error {
				if err := tx.RemoveNode(removed); err != nil {
					return err
				}
				return tx.AddEdgeWithHandle("x", "y", 0, h)
			}); err != nil {
				t.Fatalf("commit: %v", err)
			}
			g.SetNodeRemovalEntryHookForTest(nil)
			if !fired {
				t.Fatal("the removal never reached the seam: the interleaving was not driven")
			}
			mem := countH(g)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
			if err != nil {
				t.Fatal(err)
			}
			rec := countH(res.Graph)
			t.Logf("arcs carrying handle %d: memory=%d recovery=%d", h, mem, rec)
			if mem != rec {
				t.Errorf("memory holds %d arcs with handle %d, recovery holds %d", mem, h, rec)
			}
			if mem != 1 {
				t.Errorf("memory holds %d arcs with handle %d; want 1 (the handle is idempotent)", mem, h)
			}
		})
	}
}

// TestDelEdgeProperty_DoesNotReadAPinCarriedFromAnEarlierOp drives the same pin
// into OpDelEdgeProperty, the other store op that reads after an admit check.
// peerSets: the peer sets k (stored then holds k, so the no-op branch is not
// taken); otherwise the peer deletes a committed k (stored lacks k, a pinned
// snapshot still shows it). Memory must equal recovery either way.
func TestDelEdgeProperty_DoesNotReadAPinCarriedFromAnEarlierOp(t *testing.T) {
	type stx = txn.Tx[string, float64]
	for _, peerSets := range []bool{true, false} {
		name := "peer deletes k"
		if peerSets {
			name = "peer sets k"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatal(err)
			}
			g := lpg.New[string, float64](adjlist.Config{})
			opts := txn.Options[string, float64]{Codec: txn.NewStringCodec(), WeightCodec: txn.NewFloat64WeightCodec()}
			st := txn.NewStoreWithOptions[string, float64](g, w, opts)
			commit := func(f func(tx *stx) error) error {
				tx := st.Begin()
				if err := f(tx); err != nil {
					return err
				}
				return tx.Commit()
			}
			if err := commit(func(tx *stx) error {
				for _, n := range []string{"x", "y", "z"} {
					if err := tx.AddNode(n); err != nil {
						return err
					}
				}
				if err := tx.AddEdgeWithHandle("x", "y", 0, 9); err != nil {
					return err
				}
				if !peerSets {
					return tx.SetEdgeProperty("x", "y", "k", lpg.Int64Value(1))
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			inPeer, releasePeer, peerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			go func() {
				peerDone <- g.ApplyDurable(context.Background(), func(wtx lpg.WriteTx) error {
					wv := g.Writer(wtx)
					if err := wv.SetNodeLabel("z", "L"); err != nil {
						return err
					}
					if peerSets {
						return wv.SetEdgeProperty("x", "y", "k", lpg.Int64Value(2))
					}
					return wv.DelEdgeProperty("x", "y", "k")
				}, func() error {
					close(inPeer)
					<-releasePeer
					ptx := st.Begin()
					if err := ptx.SetNodeLabel("z", "L"); err != nil {
						return err
					}
					if peerSets {
						if err := ptx.SetEdgeProperty("x", "y", "k", lpg.Int64Value(2)); err != nil {
							return err
						}
					} else if err := ptx.DelEdgeProperty("x", "y", "k"); err != nil {
						return err
					}
					return ptx.CommitWALOnly(0)
				})
			}()
			<-inPeer
			fired := false
			g.SetNodeRemovalEntryHookForTest(func() {
				if fired {
					return
				}
				fired = true
				close(releasePeer)
				if err := <-peerDone; err != nil {
					t.Errorf("peer commit: %v", err)
				}
			})
			if err := commit(func(tx *stx) error {
				if err := tx.RemoveNode("z"); err != nil {
					return err
				}
				return tx.DelEdgeProperty("x", "y", "k")
			}); err != nil {
				t.Fatalf("commit: %v", err)
			}
			g.SetNodeRemovalEntryHookForTest(nil)
			if !fired {
				t.Fatal("seam not reached")
			}
			_, memHas := g.GetEdgeProperty("x", "y", "k")
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
			if err != nil {
				t.Fatal(err)
			}
			_, recHas := res.Graph.GetEdgeProperty("x", "y", "k")
			t.Logf("k present: memory=%v recovery=%v", memHas, recHas)
			if memHas != recHas {
				t.Errorf("memory has k=%v, recovery has k=%v", memHas, recHas)
			}
		})
	}
}
