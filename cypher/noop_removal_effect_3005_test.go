package cypher

// noop_removal_effect_3005_test.go — regression gate for rmp #3005.
//
// Two side effects of a property removal were still decided on a presence probe
// of the PRESENT state instead of on the removal having written a version:
//
//   - a relationship-property removal queued its index change unconditionally, so
//     removing a property the relationship does not carry queued one;
//   - a node-property removal bumped the statistics' Δ and delete counters on
//     `had`, which for a refused removal (a peer's uncommitted write heads the
//     chain) describes the peer's value.
//
// Both are now gated on effectMark/tookEffect, in both adapters.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestNoOpRelPropertyRemoval_QueuesNoIndexChange_3005: removing a property the
// relationship does not carry queues no index change; removing one it carries
// queues exactly one (the control).
func TestNoOpRelPropertyRemoval_QueuesNoIndexChange_3005(t *testing.T) {
	t.Parallel()
	for engName, open := range refusedRemoveEngines3003() {
		for _, c := range []struct {
			key  string
			want int
		}{{"q", 0}, {"p", 1}} {
			t.Run(engName+"/"+c.key, func(t *testing.T) {
				t.Parallel()
				eng := open(t)
				// q is interned (a node carries it) so the removal reaches the store.
				refusedRemoveSetup3003(t, eng)
				res, err := eng.RunInTx(context.Background(), "MATCH (n:Item {id:1}) SET n.q = 1", nil)
				if err != nil {
					t.Fatal(err)
				}
				for res.Next() {
				}
				if err := res.Close(); err != nil {
					t.Fatal(err)
				}

				tx, err := eng.BeginTx(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback() }()
				before := tx.buf.Len()
				refusedRemoveExec3003(t, tx, "MATCH ()-[r:R]->() REMOVE r."+c.key)
				if got := tx.buf.Len() - before; got != c.want {
					t.Errorf("REMOVE r.%s queued %d index changes, want %d", c.key, got, c.want)
				}
			})
		}
	}
}

// TestRefusedNodeRemoval_LeavesStatisticsUnchanged_3005: the peer writes p and
// does not commit; T removes p and is refused. The statistics for (:P, p) must not
// move. The control removes p with no peer and moves Δ and the delete counter by
// one each.
func TestRefusedNodeRemoval_LeavesStatisticsUnchanged_3005(t *testing.T) {
	t.Parallel()
	for engName, open := range map[string]func(t *testing.T) *Engine{
		"memory": func(t *testing.T) *Engine {
			g := lpg.New[string, float64](adjlist.Config{})
			for _, err := range []error{
				g.AddNode("n0"), g.SetNodeLabel("n0", "P"), g.SetNodeProperty("n0", "p", lpg.Int64Value(1)),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			eng := NewEngine(g)
			t.Cleanup(func() { _ = eng.Close() })
			return eng
		},
		"wal": func(t *testing.T) *Engine {
			eng := refusedRemoveEngines3003()["wal"](t)
			for _, err := range []error{
				eng.g.AddNode("n0"), eng.g.SetNodeLabel("n0", "P"), eng.g.SetNodeProperty("n0", "p", lpg.Int64Value(1)),
			} {
				if err != nil {
					t.Fatal(err)
				}
			}
			return eng
		},
	} {
		for _, withPeer := range []bool{true, false} {
			arm := "control"
			if withPeer {
				arm = "refused"
			}
			t.Run(engName+"/"+arm, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				eng := open(t)
				if err := eng.RefreshStatistics(ctx); err != nil {
					t.Fatal(err)
				}
				if withPeer {
					peer, err := eng.BeginTx(ctx)
					if err != nil {
						t.Fatal(err)
					}
					refusedRemoveExec3003(t, peer, "MATCH (n:P) SET n.p = 2")
					t.Cleanup(func() { _ = peer.Rollback() })
				}
				st, ok := lookupStats(liveResolver(eng), "P", "p")
				if !ok {
					t.Fatal("setup: no statistics bundle for (:P, p)")
				}
				delta, deletes := st.Delta(), st.Deletes()

				tx, err := eng.BeginTx(ctx)
				if err != nil {
					t.Fatal(err)
				}
				refusedRemoveExec3003(t, tx, "MATCH (n:P) REMOVE n.p")
				_ = tx.Rollback()

				want := int64(1)
				if withPeer {
					want = 0
				}
				if d := st.Delta() - delta; d != want {
					t.Errorf("Δ moved by %d, want %d", d, want)
				}
				if d := st.Deletes() - deletes; d != want {
					t.Errorf("deletes moved by %d, want %d", d, want)
				}
			})
		}
	}
}
