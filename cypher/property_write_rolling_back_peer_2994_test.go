package cypher_test

// property_write_rolling_back_peer_2994_test.go — regression gate for rmp #2994.
//
// Both mutator adapters read a property's pre-image, and lpg judges a SET a no-op,
// against the PRESENT node state, which can hold a value written by a peer
// transaction that has not committed and is about to roll back. Two defects were
// suspected:
//
//   - the undo pre-image: T's inverse would restore the peer's rolled-back value;
//   - the no-op judgement: T's SET of the value the peer wrote would be judged a
//     no-op, write no version, and be lost when the peer rolls back.
//
// Both are refuted by the write-write conflict test, which runs on the property
// store's chain head BEFORE any presence or equality guard: for SET in
// [lpg.Graph] setNodePropertyInfo (graph/lpg/property.go:390, the rmp #2324 fix)
// and for REMOVE in delNodePropertyShared (property.go:614) and the exclusive
// body (property.go:532, rmp #2943). The peer's uncommitted version is the head
// and is not visible to T, so T is refused with a serialization conflict and
// dooms itself.
//
// A refused SET returns its error before the adapter records an undo, so no
// pre-image is kept. A refused REMOVE goes through a void primitive: the statement
// succeeds, the adapter records an inverse whose pre-image is the peer's value
// (cypher/undo_record.go recordDelNodeProperty), and the commit is refused by the
// doomed-transaction backstop. On T's rollback after the peer's, that inverse IS
// re-applied under T's own write context (a probe read the raw bag at 2 right
// after it ran), and T's MVCC abort then withdraws it, so no committed state ever
// carries the peer's value.
//
// The oracle is the committed value after the peer and T have both finished: it
// must be the value committed before either began, and T must never report a
// successful commit.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// peerEngine2994 builds one engine flavour seeded with node 0 holding p = 1 and
// node 1, the node T also writes for real so it is not a version-free transaction.
type peerEngine2994 struct {
	name string
	open func(t *testing.T) *cypher.Engine
}

var peerEngines2994 = []peerEngine2994{
	{name: "memory", open: func(t *testing.T) *cypher.Engine {
		g := lpg.New[string, float64](adjlist.Config{})
		eng := cypher.NewEngine(g)
		t.Cleanup(func() { _ = eng.Close() })
		return eng
	}},
	{name: "wal", open: func(t *testing.T) *cypher.Engine {
		w, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		g := lpg.New[string, float64](adjlist.Config{})
		st := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		})
		eng := cypher.NewEngineWithStore(st)
		t.Cleanup(func() { _ = eng.Close(); _ = w.Close() })
		return eng
	}},
}

// peerCase2994 is one interleaving: the peer's uncommitted write and T's write
// of the same property of node 0, whose committed value is 1.
type peerCase2994 struct {
	name, peer, t string
	// void marks a T write that goes through a void lpg primitive (a property
	// removal): its refusal dooms T without failing the statement, so it surfaces
	// only at commit (rmp #2354's backstop), never on the rollback arm.
	void bool
	// wrote is the value T's write leaves on n.p: what n.p must read if T ever
	// reports a successful commit, so a write judged a no-op and dropped is caught.
	wrote expr.Value
}

var peerCases2994 = []peerCase2994{
	// No-op suspicion: T writes the value the peer holds uncommitted.
	{name: "set_equal_to_peer", peer: "SET n.p = 2", t: "SET n.p = 2", wrote: expr.IntegerValue(2)},
	// Pre-image suspicion, SET: T's pre-image would be the peer's 2.
	{name: "set_other_than_peer", peer: "SET n.p = 2", t: "SET n.p = 3", wrote: expr.IntegerValue(3)},
	// Pre-image suspicion, REMOVE: the inverse would re-set the peer's 2.
	{name: "remove_over_peer_set", peer: "SET n.p = 2", t: "REMOVE n.p", void: true, wrote: expr.Null},
	{name: "set_null_over_peer_set", peer: "SET n.p = 2", t: "SET n.p = null", void: true, wrote: expr.Null},
	// The peer removed the key: T's SET of the committed value is judged against
	// an absent key, and T's REMOVE against an absent key is a no-op.
	{name: "set_committed_over_peer_remove", peer: "REMOVE n.p", t: "SET n.p = 1", wrote: expr.IntegerValue(1)},
	{name: "remove_over_peer_remove", peer: "REMOVE n.p", t: "REMOVE n.p", void: true, wrote: expr.Null},
}

func peerExec2994(tx *cypher.ExplicitTx, q string) error {
	res, err := tx.Exec(q, nil)
	if err != nil {
		return err
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		return err
	}
	return res.Close()
}

// committedP2994 reads node 0's p from a fresh autocommit read.
func committedP2994(t *testing.T, eng *cypher.Engine) expr.Value {
	t.Helper()
	const q = "MATCH (n:Item {id:0}) RETURN n.p"
	res, err := eng.Run(context.Background(), q, nil)
	rows := noopDrain(t, q, res, err)
	if len(rows) != 1 {
		t.Fatalf("%s: %d rows, want 1", q, len(rows))
	}
	return rows[0][0]
}

// TestPropertyWrite_RollingBackPeer_2994: the peer writes node 0's p and does not
// commit; T writes the same property; the peer rolls back; T commits or rolls
// back. T must be refused with a serialization conflict and the committed value
// must still be 1, the value committed before either began.
func TestPropertyWrite_RollingBackPeer_2994(t *testing.T) {
	t.Parallel()
	for _, e := range peerEngines2994 {
		for _, c := range peerCases2994 {
			for _, finish := range []string{"commit", "rollback"} {
				t.Run(e.name+"/"+c.name+"/"+finish, func(t *testing.T) {
					t.Parallel()
					ctx := context.Background()
					eng := e.open(t)
					noopRun(t, eng, "CREATE (:Item {id:0, p:1}), (:Item {id:1})")

					peer, err := eng.BeginTx(ctx)
					if err != nil {
						t.Fatal(err)
					}
					noopTxRun(t, peer, "MATCH (n:Item {id:0}) "+c.peer)

					tt, err := eng.BeginTx(ctx)
					if err != nil {
						t.Fatal(err)
					}
					noopTxRun(t, tt, "MATCH (n:Item {id:1}) SET n.z = 1")
					execErr := peerExec2994(tt, "MATCH (n:Item {id:0}) "+c.t)

					if err := peer.Rollback(); err != nil {
						t.Fatalf("peer rollback: %v", err)
					}

					var finErr error
					if finish == "commit" {
						finErr = tt.Commit()
					} else {
						finErr = tt.Rollback()
					}

					// Refutation path: T is refused because the peer's
					// uncommitted version heads the chain.
					switch {
					case !c.void:
						if !errors.Is(execErr, cypher.ErrSerializationConflict) {
							t.Errorf("T's SET was not refused with a serialization conflict: %v", execErr)
						}
					case execErr != nil:
						t.Errorf("T's removal failed the statement (%v); the void primitive dooms at commit", execErr)
					case finish == "commit" && !errors.Is(finErr, cypher.ErrSerializationConflict):
						t.Errorf("T's commit after a refused removal: %v, want a serialization conflict", finErr)
					}
					got := committedP2994(t, eng)
					if finish == "commit" && finErr == nil {
						t.Errorf("T committed over a peer's uncommitted write of the same property")
						if got != c.wrote {
							t.Errorf("T reported a successful commit of n.p = %v, yet n.p = %v: a lost update (rmp #2994)",
								c.wrote, got)
						}
					}
					if iv, ok := got.(expr.IntegerValue); !ok || iv != 1 {
						t.Errorf("committed n.p = %v (%T), want 1: a rolled-back peer value or a lost write survived (rmp #2994)",
							got, got)
					}
					// A later writer reads and writes the same property: a version
					// either transaction left behind would refuse it or feed it a
					// wrong value.
					noopRun(t, eng, "MATCH (n:Item {id:0}) SET n.p = n.p + 10")
					if got := committedP2994(t, eng); got != expr.IntegerValue(11) {
						t.Errorf("after a later n.p + 10: n.p = %v, want 11", got)
					}
				})
			}
		}
	}
}
