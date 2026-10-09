package lpg_test

// remove_strip_race_test.go — a durable store removal strips every label and
// property committed before it, in memory exactly as replay does (ACID audit
// round 6).
//
// The store's OpRemoveNode strips the node's labels and properties as its
// transaction sees them, then removes the node. Replay strips what the log
// holds at the op's position. A label another commit adds between the strip's
// read and the removal's claim commits AHEAD of the removal, so replay strips
// it, while memory, whose strip had already read, kept it — and the node's
// next revival showed a label recovery did not have. Found by the randomised
// store differential; properties are stripped the same way.
//
// Layer: short.

import (
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func TestRemoveNode_StripsWhatCommitsBeforeTheClaim(t *testing.T) {
	type peerWrite struct {
		name  string
		write func(tx *txn.Tx[string, float64]) error
		holds func(g *lpg.Graph[string, float64]) bool
	}
	writes := []peerWrite{
		{"label", func(tx *txn.Tx[string, float64]) error { return tx.SetNodeLabel("x", "L1") },
			func(g *lpg.Graph[string, float64]) bool { return g.HasNodeLabel("x", "L1") }},
		{"property", func(tx *txn.Tx[string, float64]) error { return tx.SetNodeProperty("x", "p", lpg.Int64Value(1)) },
			func(g *lpg.Graph[string, float64]) bool { _, ok := g.GetNodeProperty("x", "p"); return ok }},
	}
	for _, pw := range writes {
		for _, dead := range []bool{false, true} {
			name := pw.name + "/live node"
			if dead {
				name = pw.name + "/dead node"
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
				commit := func(f func(tx *txn.Tx[string, float64]) error) error {
					tx := st.Begin()
					if err := f(tx); err != nil {
						return err
					}
					return tx.Commit()
				}
				if err := commit(func(tx *txn.Tx[string, float64]) error { return tx.AddNode("x") }); err != nil {
					t.Fatal(err)
				}
				if dead {
					if err := commit(func(tx *txn.Tx[string, float64]) error { return tx.RemoveNode("x") }); err != nil {
						t.Fatal(err)
					}
				}
				// A peer store commit writes x in the window between the
				// removal's strip and its claim, once. It runs on its own goroutine —
				// the removal's apply is in progress on this one — and the seam waits
				// for it: it takes the lower WAL sequence, so the log holds L1 AHEAD
				// of the removal.
				fired := false
				g.SetNodeRemovalEntryHookForTest(func() {
					if fired {
						return
					}
					fired = true
					done := make(chan error, 1)
					go func() {
						done <- commit(pw.write)
					}()
					if err := <-done; err != nil {
						t.Errorf("peer commit: %v", err)
					}
				})
				if err := commit(func(tx *txn.Tx[string, float64]) error { return tx.RemoveNode("x") }); err != nil {
					t.Fatal(err)
				}
				g.SetNodeRemovalEntryHookForTest(nil)
				if !fired {
					t.Fatal("the removal never reached the seam: the interleaving was not driven")
				}
				// Revive x, so its label bag is observable again.
				if err := commit(func(tx *txn.Tx[string, float64]) error { return tx.AddNode("x") }); err != nil {
					t.Fatal(err)
				}
				if pw.holds(g) {
					t.Errorf("the removal left a %s committed before its claim: memory keeps it, replay strips it", pw.name)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				res, err := recovery.Open[string, float64](dir, recovery.OptionsFromTxn(opts))
				if err != nil {
					t.Fatal(err)
				}
				if pw.holds(res.Graph) {
					t.Errorf("recovery keeps the %s", pw.name)
				}
			})
		}
	}
}
