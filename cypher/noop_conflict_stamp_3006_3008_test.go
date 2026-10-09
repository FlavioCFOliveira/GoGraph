package cypher_test

// noop_conflict_stamp_3006_3008_test.go — regression gates for rmp #3006 and
// rmp #3008: two false serialization conflicts caused by conflict stamps taken by
// writes that change nothing.
//
//   - #3006: removing a relationship property the relationship does not carry
//     claimed the source node's whole adjacency entry before it looked for the
//     key, so it refused a concurrent writer of any other arc of that node, spent
//     a commit-record version, and made the durable path log a removal.
//   - #3008: a no-op `SET n.b = <same value>` under a schema declaring any
//     constraint took the node's constraint stamp, so a peer's `SET n.b` was
//     refused with a conflict in node constraint although no constraint names b.
//
// Every scenario runs on the in-memory engine and on the persisted store
// (store.Open), and each false-conflict arm sits beside a control that must stay
// refused.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// noopStampEngines returns the two engines every scenario runs on. The
// persisted one also returns its store directory and a close function the
// WAL-reading test calls before it opens the log.
func noopStampEngines() map[string]func(t *testing.T) (*cypher.Engine, string, func()) {
	return map[string]func(t *testing.T) (*cypher.Engine, string, func()){
		"memory": func(t *testing.T) (*cypher.Engine, string, func()) {
			eng := cypher.NewEngine(lpg.New[string, float64](adjlist.Config{}))
			closed := false
			closeFn := func() {
				if !closed {
					closed = true
					_ = eng.Close()
				}
			}
			t.Cleanup(closeFn)
			return eng, "", closeFn
		},
		"persisted": func(t *testing.T) (*cypher.Engine, string, func()) {
			dir := t.TempDir()
			o, err := store.Open(dir, openedOptions())
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			eng := cypher.NewEngineWithOpened(o)
			closed := false
			closeFn := func() {
				if !closed {
					closed = true
					_ = eng.Close()
					if err := o.Close(); err != nil {
						t.Errorf("store Close: %v", err)
					}
				}
			}
			t.Cleanup(closeFn)
			return eng, dir, closeFn
		},
	}
}

// noopStampAuto runs q as one autocommit statement and returns the first error
// the run, the drain or the close produced.
func noopStampAuto(eng *cypher.Engine, q string) error {
	res, err := eng.RunInTx(context.Background(), q, nil)
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

// noopStampExec runs q in tx and drains it, failing the test on any error.
func noopStampExec(t *testing.T, tx *cypher.ExplicitTx, q string) {
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

func noopStampMust(t *testing.T, eng *cypher.Engine, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if err := noopStampAuto(eng, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
}

// relRemovalSetup3006 builds a -[:R {p:1}]-> b and a -[:S]-> c, and interns the
// key `absent` on an unrelated node so the removal reaches the store.
func relRemovalSetup3006(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	noopStampMust(t, eng,
		"CREATE (a:Item {id:0})-[:R {p:1}]->(:Item {id:1}), (a)-[:S]->(:Item {id:2}), (:K {absent: 1})")
}

const removeAbsent3006 = "MATCH (:Item {id:0})-[r:R]->() REMOVE r.absent"

// TestRemoveAbsentRelProperty_NoFalseConflict_3006: T1 removes a key r does not
// carry and stays open; a peer then writes another arc of r's source node, or a
// property of the node, and commits. The peer must succeed, and so must T1.
func TestRemoveAbsentRelProperty_NoFalseConflict_3006(t *testing.T) {
	t.Parallel()
	peers := []struct{ name, q string }{
		{"other_arc_property", "MATCH (:Item {id:0})-[s:S]->() SET s.q = 1"},
		{"new_arc", "MATCH (a:Item {id:0}), (c:Item {id:2}) CREATE (a)-[:S]->(c)"},
		{"node_property", "MATCH (a:Item {id:0}) SET a.x = 1"},
	}
	for engName, open := range noopStampEngines() {
		for _, p := range peers {
			t.Run(engName+"/"+p.name, func(t *testing.T) {
				t.Parallel()
				eng, _, _ := open(t)
				relRemovalSetup3006(t, eng)
				t1, err := eng.BeginTx(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				noopStampExec(t, t1, removeAbsent3006)
				if err := noopStampAuto(eng, p.q); err != nil {
					_ = t1.Rollback()
					t.Fatalf("peer %q beside an open removal of an absent key: %v", p.q, err)
				}
				if err := t1.Commit(); err != nil {
					t.Fatalf("T1 commit: %v", err)
				}
			})
		}
	}
}

// TestRemoveAbsentRelProperty_RealConflictStaysRefused_3006: the key is absent
// from what T1 removes against, but a peer wrote it after T1's snapshot —
// uncommitted, or committed. T1's removal must still be refused, so T1's commit
// fails. In the peer_uncommitted_removal arm the STORED entry no longer carries
// p, because a peer's uncommitted removal heads it, while T1 still sees p; were T1
// admitted, the peer's rollback would bring p back under T1's acknowledged
// removal. At this level the by-handle removal also refuses that arm, so the
// per-pair primitive's own refusal is pinned in graph/lpg
// (TestDelEdgeProperty_AbsentKeyStillRefusedOverPeer_3006).
func TestRemoveAbsentRelProperty_RealConflictStaysRefused_3006(t *testing.T) {
	t.Parallel()
	const writeKey = "MATCH (:Item {id:0})-[r:R]->() SET r.absent = 5"
	const removeP = "MATCH (:Item {id:0})-[r:R]->() REMOVE r.p"
	arms := []struct {
		name, peer, t1 string
		peerCommits    bool // autocommit before T1 writes, else open until T1 commits
	}{
		{"peer_uncommitted", writeKey, removeAbsent3006, false},
		{"peer_committed_after_snapshot", writeKey, removeAbsent3006, true},
		{"peer_uncommitted_removal", removeP, removeP, false},
	}
	for engName, open := range noopStampEngines() {
		for _, arm := range arms {
			t.Run(engName+"/"+arm.name, func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				eng, _, _ := open(t)
				relRemovalSetup3006(t, eng)
				var peer *cypher.ExplicitTx
				t1, err := eng.BeginTx(ctx)
				if err != nil {
					t.Fatal(err)
				}
				// Pin T1's snapshot before the peer writes.
				noopStampExec(t, t1, "MATCH (n:Item) RETURN count(n)")
				if arm.peerCommits {
					if err := noopStampAuto(eng, arm.peer); err != nil {
						t.Fatalf("peer: %v", err)
					}
				} else {
					if peer, err = eng.BeginTx(ctx); err != nil {
						t.Fatal(err)
					}
					noopStampExec(t, peer, arm.peer)
				}
				noopStampExec(t, t1, arm.t1)
				cerr := t1.Commit()
				if !errors.Is(cerr, cypher.ErrSerializationConflict) {
					t.Errorf("T1 commit over a peer's write of the key: %v, want a serialization conflict", cerr)
				}
				if peer != nil {
					if err := peer.Rollback(); err != nil {
						t.Errorf("peer rollback: %v", err)
					}
				}
			})
		}
	}
}

// TestRemoveAbsentRelProperty_LogsNoRemoval_3006: on the persisted store, a
// committed removal of an absent key appends no per-pair removal record to the
// WAL; the control removal of a present key appends exactly one.
func TestRemoveAbsentRelProperty_LogsNoRemoval_3006(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		key  string
		want int
	}{{"absent", 0}, {"p", 1}} {
		t.Run(c.key, func(t *testing.T) {
			t.Parallel()
			eng, dir, closeFn := noopStampEngines()["persisted"](t)
			relRemovalSetup3006(t, eng)
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			noopStampExec(t, tx, "MATCH (:Item {id:0})-[r:R]->() REMOVE r."+c.key)
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			closeFn()
			if got := countWALOps3006(t, filepath.Join(dir, "wal"), txn.OpDelEdgeProperty); got != c.want {
				t.Errorf("REMOVE r.%s logged %d OpDelEdgeProperty records, want %d", c.key, got, c.want)
			}
		})
	}
}

// countWALOps3006 counts the WAL frames at path whose op kind is kind.
func countWALOps3006(t *testing.T, path string, kind txn.OpKind) (n int) {
	t.Helper()
	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	for f := range r.Frames() {
		if len(f.Payload) >= 2 && txn.OpKind(f.Payload[1]) == kind {
			n++
		}
	}
	if err := r.TailError(); err != nil {
		t.Fatalf("WAL iteration stopped at %d: %v", r.TailOffset(), err)
	}
	return n
}

// constraintStampSetup3008 is the #3008 reproduction's schema: a UNIQUE
// constraint on (:U, u) that names neither the label Item nor the key b.
func constraintStampSetup3008(t *testing.T, eng *cypher.Engine) {
	t.Helper()
	noopStampMust(t, eng,
		"CREATE (:Item:L {id:0, b:'b00', u:'u0'}), (:Item {id:1})",
		"CREATE CONSTRAINT FOR (n:U) REQUIRE n.u IS UNIQUE")
}

// TestNoOpSet_TakesNoConstraintStamp_3008: T writes Item 1, then sets a property
// of Item 0 to the value it already has, and stays open; a peer then sets that
// property to a new value. Nothing T did changes Item 0, so the peer must commit,
// and so must T. The constrained_key arm repeats it on u, the key the UNIQUE
// constraint names: a SET that moves no value moves no reservation either, so it
// pins the effect half of the fix. The unconstrained_label_loss arm is an
// EFFECTIVE write of a label no constraint names, so it pins the scope half.
func TestNoOpSet_TakesNoConstraintStamp_3008(t *testing.T) {
	t.Parallel()
	arms := []struct{ name, noop, peer string }{
		{"unconstrained_key", "MATCH (n:Item {id:0}) SET n.b = 'b00'", "MATCH (n:Item {id:0}) SET n.b = 'b01'"},
		{"constrained_key", "MATCH (n:Item {id:0}) SET n.u = 'u0'", "MATCH (n:Item {id:0}) SET n.u = 'u1'"},
		{"unconstrained_label_loss", "MATCH (n:Item {id:0}) REMOVE n:L", "MATCH (n:Item {id:0}) SET n.b = 'b01'"},
	}
	for engName, open := range noopStampEngines() {
		for _, a := range arms {
			t.Run(engName+"/"+a.name, func(t *testing.T) {
				t.Parallel()
				eng, _, _ := open(t)
				constraintStampSetup3008(t, eng)
				tx, err := eng.BeginTx(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				noopStampExec(t, tx, "MATCH (n:Item {id:1}) SET n.z = 1")
				noopStampExec(t, tx, a.noop)
				if err := noopStampAuto(eng, a.peer); err != nil {
					_ = tx.Rollback()
					t.Fatalf("peer %q beside T's %q: %v", a.peer, a.noop, err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatalf("T commit: %v", err)
				}
			})
		}
	}
}

// TestConstraintStamp_RealWriteSkewStaysRefused_3008: the two halves of the
// UNIQUE invariant on (:U, u) written by two transactions — T sets u on a node
// that lacks U, a peer gives that node U — must still collide on the node's
// constraint stamp, in either order of arrival. The scoped stamp keys on the
// WRITTEN label or key, not on the node's current labels, which is what keeps
// this pair refused.
func TestConstraintStamp_RealWriteSkewStaysRefused_3008(t *testing.T) {
	t.Parallel()
	const setKey, gainLabel = "MATCH (n:Item {id:0}) SET n.u = 'u9'", "MATCH (n:Item {id:0}) SET n:U"
	for engName, open := range noopStampEngines() {
		for _, arm := range []struct{ name, first, second string }{
			{"key_then_label", setKey, gainLabel},
			{"label_then_key", gainLabel, setKey},
		} {
			t.Run(engName+"/"+arm.name, func(t *testing.T) {
				t.Parallel()
				eng, _, _ := open(t)
				constraintStampSetup3008(t, eng)
				tx, err := eng.BeginTx(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				noopStampExec(t, tx, arm.first)
				perr := noopStampAuto(eng, arm.second)
				_ = tx.Rollback()
				if !errors.Is(perr, cypher.ErrSerializationConflict) || !strings.Contains(perr.Error(), "node constraint") {
					t.Fatalf("peer %q beside an open %q: %v, want a serialization conflict in node constraint", arm.second, arm.first, perr)
				}
			})
		}
	}
}
