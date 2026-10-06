package cypher_test

// detach_delete_aborted_stamp_2997_test.go — rmp #2997.
//
// A committed DETACH DELETE of a hub left incoming arcs in place under
// concurrent writers. The deleter removes the incoming arcs its SNAPSHOT lists;
// an arc a peer committed after that snapshot is not listed, and what must
// refuse the delete instead is the adjacency conflict stamp the peer's append
// left on the hub. A third transaction that appended to the hub after the peer's
// commit and then ABORTED overwrote that stamp, and the abort cleared the slot,
// taking the peer's commit with it: the deleter's claim on the hub found nothing
// and committed beside the arc.
//
// The test scripts that interleaving with explicit transactions — no goroutines.
// Before the fix the DETACH DELETE commits and one arc points at the dead hub;
// after it the deleter is refused with the typed serialization conflict. The
// control case, with no aborted appender, is refused in both builds, so a pass
// of the first case cannot come from the conflict model being disarmed.
//
// Layer: short.

import (
	"context"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

func TestDetachDelete_RefusedOverArcCommittedAfterSnapshot(t *testing.T) {
	cases := []struct {
		name string
		// abortedAppend runs an append onto the hub between the peer's commit and
		// the deleter's statement, then rolls it back.
		abortedAppend bool
	}{
		{name: "aborted_append_after_peer_commit", abortedAppend: true},
		{name: "control_no_aborted_append", abortedAppend: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			eng := cypher.NewEngine(g)
			defer func() { _ = eng.Close() }()
			runAll(t, eng, "CREATE (:Hub {id:1}), (:X {id:1}), (:X {id:2})")

			exec := func(tx *cypher.ExplicitTx, q string) error {
				res, err := tx.ExecAny(q, nil)
				if err != nil {
					return err
				}
				for res.Next() { // intentional drain
				}
				derr := res.Err()
				_ = res.Close()
				return derr
			}

			// The deleter's snapshot predates the peer's arc.
			del, err := eng.BeginTx(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := exec(del, "MATCH (h:Hub {id:1}) RETURN h"); err != nil {
				t.Fatalf("deleter read: %v", err)
			}

			// The peer commits an arc into the hub.
			runAll(t, eng, "MATCH (x:X {id:1}), (h:Hub {id:1}) CREATE (x)-[:R]->(h)")

			if c.abortedAppend {
				// A later transaction sees the peer's commit, appends onto the hub —
				// stamping over the peer's commit — and rolls back.
				ab, err := eng.BeginTx(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if err := exec(ab, "MATCH (x:X {id:2}), (h:Hub {id:1}) CREATE (x)-[:R]->(h)"); err != nil {
					t.Fatalf("aborted appender: %v", err)
				}
				if err := ab.Rollback(); err != nil {
					t.Fatalf("aborted appender rollback: %v", err)
				}
			}

			delErr := exec(del, "MATCH (h:Hub {id:1}) DETACH DELETE h")
			if delErr == nil {
				// The conflict may legally surface at COMMIT instead of the statement.
				delErr = del.Commit()
			} else {
				_ = del.Rollback()
			}
			if !errors.Is(delErr, mvcc.ErrSerializationConflict) {
				t.Errorf("DETACH DELETE over an arc committed after its snapshot: err=%v, "+
					"want ErrSerializationConflict (rmp #2997)", delErr)
			}

			g.ReclaimNow()
			g.ReclaimNow()
			live, dead := adjacencyArcs(g)
			if dead != 0 {
				t.Fatalf("%d adjacency entries touch a dead node after the DETACH DELETE "+
					"(live arcs %d): a node delete committed beside a live arc (rmp #2997)", dead, live)
			}
			if live != 1 {
				t.Fatalf("live arcs = %d, want 1: the peer's committed arc must survive the refused delete", live)
			}
		})
	}
}
