package lpg_test

// remove_strip_pinned_test.go — a durable store removal strips a label or
// property that a peer committed AFTER the removal's first strip had already
// read it in flight (rmp #3030).
//
// The store's OpRemoveNode strips what its transaction sees, claims the node,
// then strips again so that whatever committed between the first read and the
// claim goes too. The first read classified the peer's in-flight version as
// invisible, and the transaction's snapshot PINS that verdict. The peer then
// committed ahead of the claim, so the claim's cross-check, which reads the
// head's stamp, passed; but the second strip read through the same snapshot,
// still saw the pinned verdict and skipped the value. Replay strips what the log
// holds at the removal's position, which includes the peer's write, so memory
// kept a label or property recovery did not, visible at the node's next
// revival. Found by store/txn TestDifferential_MemoryEqualsRecovery.
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

func TestRemoveNode_StripsAPeerCommitThatTheFirstStripReadInFlight(t *testing.T) {
	type stx = txn.Tx[string, float64]
	type gr = lpg.Graph[string, float64]
	shapes := []struct {
		name  string
		apply func(wv lpg.WriteView[string, float64]) error // the peer's in-memory write
		log   func(tx *stx) error                           // the same write, buffered for the WAL
		holds func(g *gr) bool
	}{
		{"label",
			func(wv lpg.WriteView[string, float64]) error { return wv.SetNodeLabel("x", "L1") },
			func(tx *stx) error { return tx.SetNodeLabel("x", "L1") },
			func(g *gr) bool { return g.HasNodeLabel("x", "L1") }},
		{"property",
			func(wv lpg.WriteView[string, float64]) error { return wv.SetNodeProperty("x", "p", lpg.Int64Value(1)) },
			func(tx *stx) error { return tx.SetNodeProperty("x", "p", lpg.Int64Value(1)) },
			func(g *gr) bool { _, ok := g.GetNodeProperty("x", "p"); return ok }},
	}
	for _, sh := range shapes {
		for _, dead := range []bool{false, true} {
			name := sh.name + "/live node"
			if dead {
				name = sh.name + "/dead node"
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
				if err := commit(func(tx *stx) error { return tx.AddNode("x") }); err != nil {
					t.Fatal(err)
				}
				if dead {
					if err := commit(func(tx *stx) error { return tx.RemoveNode("x") }); err != nil {
						t.Fatal(err)
					}
				}

				// The peer is a store commit held inside its durable step, its
				// write applied and uncommitted. Released, it logs the write and
				// publishes, as Tx.Commit does.
				inPeer, releasePeer, peerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					peerDone <- g.ApplyDurable(context.Background(), func(wtx lpg.WriteTx) error {
						return sh.apply(g.Writer(wtx))
					}, func() error {
						close(inPeer)
						<-releasePeer
						ptx := st.Begin()
						if err := sh.log(ptx); err != nil {
							return err
						}
						return ptx.CommitWALOnly(0)
					})
				}()
				<-inPeer

				// The removal's first strip reads the peer's write in flight. At
				// the seam between that strip and the claim the peer commits, so
				// it holds the lower WAL sequence.
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
				if err := commit(func(tx *stx) error { return tx.RemoveNode("x") }); err != nil {
					t.Fatalf("removal: %v", err)
				}
				g.SetNodeRemovalEntryHookForTest(nil)
				if !fired {
					t.Fatal("the removal never reached the seam: the interleaving was not driven")
				}
				// Revive x, so its labels and properties are observable again.
				if err := commit(func(tx *stx) error { return tx.AddNode("x") }); err != nil {
					t.Fatal(err)
				}
				if sh.holds(g) {
					t.Errorf("memory keeps the %s the peer committed ahead of the removal; replay strips it", sh.name)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
				if err != nil {
					t.Fatal(err)
				}
				if sh.holds(res.Graph) {
					t.Errorf("recovery keeps the %s", sh.name)
				}
			})
		}
	}
}
