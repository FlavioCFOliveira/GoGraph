package cypher

// refused_remove_effect_3003_test.go — regression gate for rmp #3003.
//
// A property removal — of a node property, of a relationship's per-pair
// property, or of a relationship instance's by-handle property — goes through a
// void lpg primitive: when a peer transaction holds an uncommitted write on the
// same property, the removal is refused (the transaction is doomed and its commit
// fails) but the statement carries on. The adapters used to judge the removal
// against the PRESENT state, which holds the peer's value, so the refused removal
// still recorded an undo inverse restoring the peer's value and still counted
// -properties 1. Both are now gated on the removal having written a version
// (effectMark/tookEffect), in both adapters.
//
// The undo log is read directly: the inverse is otherwise invisible, because T's
// MVCC abort withdraws whatever it re-applies (rmp #2994).

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

func refusedRemoveEngines3003() map[string]func(t *testing.T) *Engine {
	return map[string]func(t *testing.T) *Engine{
		"memory": func(t *testing.T) *Engine {
			eng := NewEngine(lpg.New[string, float64](adjlist.Config{}))
			t.Cleanup(func() { _ = eng.Close() })
			return eng
		},
		"wal": func(t *testing.T) *Engine {
			w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
			if err != nil {
				t.Fatalf("wal.Open: %v", err)
			}
			g := lpg.New[string, float64](adjlist.Config{})
			eng := NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewFloat64WeightCodec(),
			}))
			t.Cleanup(func() { _ = eng.Close(); _ = w.Close() })
			return eng
		},
	}
}

// refusedRemoveExec3003 runs q in tx and drains it.
func refusedRemoveExec3003(t *testing.T, tx *ExplicitTx, q string) {
	t.Helper()
	res, err := tx.Exec(q, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("%s: close: %v", q, err)
	}
}

func refusedRemoveSetup3003(t *testing.T, eng *Engine) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(),
		"CREATE (:Item {id:0, p:1})-[:R {p:1}]->(:Item {id:1})", nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
	}
	if err := res.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestRefusedRemove_NoInverse_3003: the peer writes p and does not commit; T
// removes p. T's removal is refused, so it must record no undo inverse, and T's
// commit must fail with a serialization conflict. The control removes p with no
// peer and records the inverses an effective removal owes.
func TestRefusedRemove_NoInverse_3003(t *testing.T) {
	t.Parallel()
	// inverses is what an EFFECTIVE removal records: one for a node property;
	// two for a relationship property, the per-pair store and the instance's
	// by-handle bag.
	removals := []struct {
		name, match, peer, remove string
		inverses                  int
	}{
		{"node_remove", "MATCH (n:Item {id:0})", "SET n.p = 2", "REMOVE n.p", 1},
		{"node_set_null", "MATCH (n:Item {id:0})", "SET n.p = 2", "SET n.p = null", 1},
		{"rel_remove", "MATCH ()-[n:R]->()", "SET n.p = 2", "REMOVE n.p", 2},
		{"rel_set_null", "MATCH ()-[n:R]->()", "SET n.p = 2", "SET n.p = null", 2},
	}
	for engName, open := range refusedRemoveEngines3003() {
		for _, rm := range removals {
			for _, withPeer := range []bool{true, false} {
				arm := "control"
				if withPeer {
					arm = "refused"
				}
				t.Run(engName+"/"+rm.name+"/"+arm, func(t *testing.T) {
					t.Parallel()
					ctx := context.Background()
					eng := open(t)
					refusedRemoveSetup3003(t, eng)

					var peer *ExplicitTx
					if withPeer {
						var err error
						if peer, err = eng.BeginTx(ctx); err != nil {
							t.Fatal(err)
						}
						refusedRemoveExec3003(t, peer, rm.match+" "+rm.peer)
					}
					tt, err := eng.BeginTx(ctx)
					if err != nil {
						t.Fatal(err)
					}
					before := len(tt.undo.inverses)
					refusedRemoveExec3003(t, tt, rm.match+" "+rm.remove)
					inverses := len(tt.undo.inverses) - before

					wantInverses := rm.inverses
					if withPeer {
						wantInverses = 0
					}
					if inverses != wantInverses {
						t.Errorf("the removal recorded %d undo inverses, want %d", inverses, wantInverses)
					}

					cerr := tt.Commit()
					if withPeer {
						if !errors.Is(cerr, ErrSerializationConflict) {
							t.Errorf("T's commit after a refused removal: %v, want a serialization conflict", cerr)
						}
						if err := peer.Rollback(); err != nil {
							t.Fatalf("peer rollback: %v", err)
						}
					} else if cerr != nil {
						t.Fatalf("control commit: %v", cerr)
					}
				})
			}
		}
	}
}

// TestRefusedRemove_NoCount_3003 drives each adapter's removal surface directly,
// with the statement counters armed. -properties is armed only on an autocommit
// statement, and an autocommit statement whose removal is refused fails without
// handing back a Result, so the adapter's own counter is the only place the
// miscount is visible.
func TestRefusedRemove_NoCount_3003(t *testing.T) {
	t.Parallel()
	surfaces := []struct {
		name string
		peer string
		del  func(m exec.GraphMutator, g *lpg.Graph[string, float64]) error
	}{
		{"node", "MATCH (n) WHERE n.p = 1 SET n.p = 2", func(m exec.GraphMutator, _ *lpg.Graph[string, float64]) error {
			return m.DelNodeProperty("n0", "p")
		}},
		{"rel_pair", "MATCH ()-[r]->() SET r.p = 2", func(m exec.GraphMutator, _ *lpg.Graph[string, float64]) error {
			return m.(interface {
				DelEdgeProperty(src, dst, key string) error
			}).DelEdgeProperty("n0", "n1", "p")
		}},
		{"rel_instance", "MATCH ()-[r]->() SET r.p = 2", func(m exec.GraphMutator, g *lpg.Graph[string, float64]) error {
			h := g.AppendEdgeHandles("n0", "n1", nil)
			return m.(interface {
				DelEdgePropertyOnInstance(src, dst string, handle uint64, key string) error
			}).DelEdgePropertyOnInstance("n0", "n1", h[0], "p")
		}},
	}
	for engName, open := range refusedRemoveEngines3003() {
		for _, sf := range surfaces {
			for _, withPeer := range []bool{true, false} {
				arm := "control"
				if withPeer {
					arm = "refused"
				}
				t.Run(engName+"/"+sf.name+"/"+arm, func(t *testing.T) {
					t.Parallel()
					ctx := context.Background()
					eng := open(t)
					g := eng.g
					for _, err := range []error{
						g.AddNode("n0"), g.AddNode("n1"), g.AddEdge("n0", "n1", 1),
						g.SetNodeProperty("n0", "p", lpg.Int64Value(1)),
						g.SetEdgeProperty("n0", "n1", "p", lpg.Int64Value(1)),
					} {
						if err != nil {
							t.Fatal(err)
						}
					}
					h := g.AppendEdgeHandles("n0", "n1", nil)
					if len(h) != 1 {
						t.Fatalf("setup: %d edge handles, want 1", len(h))
					}
					if err := g.SetEdgePropertyByHandle("n0", "n1", h[0], "p", lpg.Int64Value(1)); err != nil {
						t.Fatal(err)
					}
					if withPeer {
						peer, err := eng.BeginTx(ctx)
						if err != nil {
							t.Fatal(err)
						}
						refusedRemoveExec3003(t, peer, sf.peer)
						t.Cleanup(func() { _ = peer.Rollback() })
					}

					wtx := g.BeginVersionedTx()
					counters := &exec.QueryCounters{}
					undo := &undoLog{}
					var m exec.GraphMutator
					if eng.store != nil {
						stx, err := eng.store.BeginCtx(ctx)
						if err != nil {
							t.Fatal(err)
						}
						defer func() { _ = stx.Rollback() }()
						m = &walMutatorAdapter{g: g, tx: stx, eng: eng, counters: counters, undo: undo, wtx: wtx}
					} else {
						m = &lpgMutatorAdapter{g: g, eng: eng, counters: counters, undo: undo, wtx: wtx}
					}
					err := g.ApplyInVersionedTx(ctx, wtx, func(lpg.WriteTx) error { return sf.del(m, g) })
					wtx.Abandon()
					g.EndVersionedTx(wtx)
					if err != nil {
						t.Fatalf("removal: %v", err)
					}

					want := int64(1)
					if withPeer {
						want = 0
					}
					if counters.PropertiesRemoved != want {
						t.Errorf("-properties = %d, want %d", counters.PropertiesRemoved, want)
					}
				})
			}
		}
	}
}
