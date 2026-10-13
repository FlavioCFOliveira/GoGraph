package cypher_test

// prop_remove_lost_update_test.go — regression tests for rmp #2943, a LOST UPDATE
// on the property stores: a committed property removal could write NOTHING when
// a concurrent, still-open transaction had already removed the same property.
//
// # The defect
//
//	T: BEGIN; MATCH (n) REMOVE n.s     (eager removal, uncommitted)
//	P: MATCH (n) REMOVE n.s            (autocommit) → SUCCESS
//	T: ROLLBACK                        → its undo restores n.s
//	final: n.s carries its old value, yet P was told its removal committed.
//
// P's delete found the key already absent from the LIVE bag (T's pending removal)
// and returned as a no-op before the write-write conflict test, so it recorded no
// version and raised no conflict. The node-label store had the identical defect
// (rmp #2354) and was fixed by running the conflict test before any presence
// guard; the node-property delete and the per-handle relationship-property delete
// kept the guard.
//
// # What these tests pin
//
// Every shape runs on BOTH wirings (in-memory engine and WAL-backed store) and
// with BOTH endings of the first transaction T:
//
//   - T rolls back: when P committed, the final state must be the state P wrote;
//     otherwise P must have been refused with a retryable
//     [mvcc.ErrSerializationConflict].
//   - T commits: P and T wrote the same object concurrently, so snapshot
//     isolation's first-updater-wins rule forbids both committing.
//
// The positive controls pin that the fix does not over-refuse: writers on
// DIFFERENT nodes both commit, and a transaction re-removing a property it
// removed ITSELF is accepted.
//
// The openCypher TCK does not cover concurrent transactions, so nothing here is —
// or is claimed as — TCK coverage.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// lostRmEngine builds an engine on the named wiring and runs setup, each
// statement in its own autocommit transaction.
func lostRmEngine(t *testing.T, walBacked bool, setup ...string) *cypher.Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	var eng *cypher.Engine
	if walBacked {
		wr, err := wal.Open(filepath.Join(t.TempDir(), "wal"))
		if err != nil {
			t.Fatalf("wal.Open: %v", err)
		}
		t.Cleanup(func() { _ = wr.Close() })
		eng = cypher.NewEngineWithStore(txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
			Codec:       txn.NewStringCodec(),
			WeightCodec: txn.NewFloat64WeightCodec(),
		}))
	} else {
		eng = cypher.NewEngine(g)
	}
	for _, q := range setup {
		if err := lostRmAutocommit(eng, q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	return eng
}

// lostRmAutocommit runs q as one autocommit statement and drains it, returning
// the first error from either the call or the result.
func lostRmAutocommit(eng *cypher.Engine, q string) error {
	r, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		return err
	}
	for r.Next() { // draining is the point
	}
	if rerr := r.Err(); rerr != nil {
		_ = r.Close()
		return rerr
	}
	return r.Close()
}

// lostRmScalar runs q and returns the printed value of column v of its single row.
func lostRmScalar(t *testing.T, eng *cypher.Engine, q string) string {
	t.Helper()
	r, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	defer func() { _ = r.Close() }()
	rows := 0
	var got string
	for r.Next() {
		rows++
		got = fmt.Sprint(r.Record()["v"])
	}
	if err := r.Err(); err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	if rows != 1 {
		t.Fatalf("%q: %d rows, want 1", q, rows)
	}
	return got
}

// lostRmShape is one concurrent-write shape: T runs tOp and stays open, P runs
// pOp as an autocommit statement, then T ends.
type lostRmShape struct {
	name  string
	setup []string
	tOp   string
	pOp   string
	// check returns column v; want is its value when P committed and T rolled back.
	check string
	want  string
	// pWrites is false for a P statement that changes nothing (SET n += {}), for
	// which both committing is not a lost update.
	pWrites bool
}

func lostRmShapes() []lostRmShape {
	const node = `CREATE (:L {id: 1, s: 'a'})`
	const only = `CREATE (:L {s: 'a'})`
	return []lostRmShape{
		{
			name: "node/remove-after-pending-remove", setup: []string{node},
			tOp: `MATCH (n:L) REMOVE n.s`, pOp: `MATCH (n:L) REMOVE n.s`,
			check: `MATCH (n:L) RETURN n.s IS NULL AS v`, want: "true", pWrites: true,
		},
		{
			name: "node/remove-after-pending-remove-of-only-property", setup: []string{only},
			tOp: `MATCH (n:L) REMOVE n.s`, pOp: `MATCH (n:L) REMOVE n.s`,
			check: `MATCH (n:L) RETURN n.s IS NULL AS v`, want: "true", pWrites: true,
		},
		{
			name: "node/set-null-after-pending-remove", setup: []string{node},
			tOp: `MATCH (n:L) REMOVE n.s`, pOp: `MATCH (n:L) SET n.s = null`,
			check: `MATCH (n:L) RETURN n.s IS NULL AS v`, want: "true", pWrites: true,
		},
		{
			name: "node/set-same-value-after-pending-remove", setup: []string{node},
			tOp: `MATCH (n:L) REMOVE n.s`, pOp: `MATCH (n:L) SET n.s = 'a'`,
			check: `MATCH (n:L) RETURN n.s AS v`, want: `"a"`, pWrites: true,
		},
		{
			name: "node/remove-after-pending-set", setup: []string{node},
			tOp: `MATCH (n:L) SET n.s = 'b'`, pOp: `MATCH (n:L) REMOVE n.s`,
			check: `MATCH (n:L) RETURN n.s IS NULL AS v`, want: "true", pWrites: true,
		},
		{
			name: "node/remove-after-pending-set-of-new-key", setup: []string{node},
			tOp: `MATCH (n:L) SET n.t = 'x'`, pOp: `MATCH (n:L) REMOVE n.t`,
			check: `MATCH (n:L) RETURN n.t IS NULL AS v`, want: "true", pWrites: true,
		},
		{
			name: "node/replace-empty-after-pending-remove-of-only-property", setup: []string{only},
			tOp: `MATCH (n:L) REMOVE n.s`, pOp: `MATCH (n:L) SET n = {}`,
			check: `MATCH (n:L) RETURN size(keys(n)) AS v`, want: "0", pWrites: true,
		},
		{
			name: "node/replace-empty-after-pending-remove", setup: []string{node},
			tOp: `MATCH (n:L) REMOVE n.s`, pOp: `MATCH (n:L) SET n = {}`,
			check: `MATCH (n:L) RETURN size(keys(n)) AS v`, want: "0", pWrites: true,
		},
		{
			name: "node/merge-empty-after-pending-remove", setup: []string{only},
			tOp: `MATCH (n:L) REMOVE n.s`, pOp: `MATCH (n:L) SET n += {}`,
			check: `MATCH (n:L) RETURN n.s AS v`, want: `"a"`, pWrites: false,
		},
		{
			name: "rel/remove-after-pending-remove-of-only-property", setup: []string{`CREATE (:A)-[:R {s: 'a'}]->(:B)`},
			tOp: `MATCH ()-[r:R]->() REMOVE r.s`, pOp: `MATCH ()-[r:R]->() REMOVE r.s`,
			check: `MATCH ()-[r:R]->() RETURN r.s IS NULL AS v`, want: "true", pWrites: true,
		},
		{
			name: "rel/remove-after-pending-remove", setup: []string{`CREATE (:A)-[:R {k: 1, s: 'a'}]->(:B)`},
			tOp: `MATCH ()-[r:R]->() REMOVE r.s`, pOp: `MATCH ()-[r:R]->() REMOVE r.s`,
			check: `MATCH ()-[r:R]->() RETURN r.s IS NULL AS v`, want: "true", pWrites: true,
		},
		{
			name: "rel/remove-after-pending-set", setup: []string{`CREATE (:A)-[:R {s: 'a'}]->(:B)`},
			tOp: `MATCH ()-[r:R]->() SET r.s = 'b'`, pOp: `MATCH ()-[r:R]->() REMOVE r.s`,
			check: `MATCH ()-[r:R]->() RETURN r.s IS NULL AS v`, want: "true", pWrites: true,
		},
		{
			name: "rel/set-same-value-after-pending-remove", setup: []string{`CREATE (:A)-[:R {s: 'a'}]->(:B)`},
			tOp: `MATCH ()-[r:R]->() REMOVE r.s`, pOp: `MATCH ()-[r:R]->() SET r.s = 'a'`,
			check: `MATCH ()-[r:R]->() RETURN r.s AS v`, want: `"a"`, pWrites: true,
		},
		{
			name: "rel/replace-empty-after-pending-remove-of-only-property", setup: []string{`CREATE (:A)-[:R {s: 'a'}]->(:B)`},
			tOp: `MATCH ()-[r:R]->() REMOVE r.s`, pOp: `MATCH ()-[r:R]->() SET r = {}`,
			check: `MATCH ()-[r:R]->() RETURN size(keys(r)) AS v`, want: "0", pWrites: true,
		},
	}
}

// lostRmRun executes one shape and reports P's error and T's end error.
func lostRmRun(t *testing.T, eng *cypher.Engine, s *lostRmShape, commitT bool) (pErr, tEnd error) {
	t.Helper()
	tx, err := eng.BeginTx(context.Background())
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := execInTx(tx, s.tOp); err != nil {
		_ = tx.Rollback()
		t.Fatalf("T %q: %v", s.tOp, err)
	}
	pErr = lostRmAutocommit(eng, s.pOp)
	if commitT {
		tEnd = tx.Commit()
	} else {
		tEnd = tx.Rollback()
	}
	return pErr, tEnd
}

// TestPropRemove_PeerRollbackDoesNotLoseACommittedWrite pins rmp #2943 with T
// rolling back: P either was refused retryably or its write is the final state.
func TestPropRemove_PeerRollbackDoesNotLoseACommittedWrite(t *testing.T) {
	for _, walBacked := range []bool{false, true} {
		for _, s := range lostRmShapes() {
			t.Run(fmt.Sprintf("wal=%v/%s", walBacked, s.name), func(t *testing.T) {
				eng := lostRmEngine(t, walBacked, s.setup...)
				pErr, tEnd := lostRmRun(t, eng, &s, false)
				if tEnd != nil {
					t.Fatalf("T rollback: %v", tEnd)
				}
				if pErr != nil {
					if !errors.Is(pErr, mvcc.ErrSerializationConflict) {
						t.Fatalf("P refused with a non-retryable error: %v", pErr)
					}
					return
				}
				if got := lostRmScalar(t, eng, s.check); got != s.want {
					t.Fatalf("P %q committed, but after T's rollback %q = %s, want %s: P's committed write was lost",
						s.pOp, s.check, got, s.want)
				}
			})
		}
	}
}

// TestPropRemove_ConcurrentWritersOfOneObjectDoNotBothCommit pins rmp #2943
// with T committing: two transactions that wrote the same object concurrently
// may not both commit (first-updater-wins).
func TestPropRemove_ConcurrentWritersOfOneObjectDoNotBothCommit(t *testing.T) {
	for _, walBacked := range []bool{false, true} {
		for _, s := range lostRmShapes() {
			if !s.pWrites {
				continue
			}
			t.Run(fmt.Sprintf("wal=%v/%s", walBacked, s.name), func(t *testing.T) {
				eng := lostRmEngine(t, walBacked, s.setup...)
				pErr, tEnd := lostRmRun(t, eng, &s, true)
				if pErr != nil && !errors.Is(pErr, mvcc.ErrSerializationConflict) {
					t.Fatalf("P refused with a non-retryable error: %v", pErr)
				}
				if tEnd != nil && !errors.Is(tEnd, mvcc.ErrSerializationConflict) {
					t.Fatalf("T commit failed with a non-retryable error: %v", tEnd)
				}
				if pErr == nil && tEnd == nil {
					t.Fatalf("T %q and P %q wrote the same object concurrently and BOTH committed", s.tOp, s.pOp)
				}
			})
		}
	}
}

// TestPropRemove_DisjointAndSelfWritesAreAccepted is the positive control: the
// fix must not refuse writes snapshot isolation allows.
func TestPropRemove_DisjointAndSelfWritesAreAccepted(t *testing.T) {
	for _, walBacked := range []bool{false, true} {
		t.Run(fmt.Sprintf("wal=%v/different-nodes", walBacked), func(t *testing.T) {
			eng := lostRmEngine(t, walBacked, `CREATE (:L {id: 1, s: 'a'}), (:L {id: 2, s: 'a'})`)
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			if err := execInTx(tx, `MATCH (n:L {id: 1}) REMOVE n.s`); err != nil {
				t.Fatalf("T: %v", err)
			}
			if err := lostRmAutocommit(eng, `MATCH (n:L {id: 2}) REMOVE n.s`); err != nil {
				t.Fatalf("P on a different node refused: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("T commit: %v", err)
			}
			if got := lostRmScalar(t, eng, `MATCH (n:L) WHERE n.s IS NULL RETURN count(n) AS v`); got != "2" {
				t.Fatalf("nodes without s = %s, want 2", got)
			}
		})
		t.Run(fmt.Sprintf("wal=%v/different-relationships", walBacked), func(t *testing.T) {
			eng := lostRmEngine(t, walBacked, `CREATE (:A)-[:R {id: 1, s: 'a'}]->(:B), (:C)-[:R {id: 2, s: 'a'}]->(:D)`)
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			if err := execInTx(tx, `MATCH ()-[r:R {id: 1}]->() REMOVE r.s`); err != nil {
				t.Fatalf("T: %v", err)
			}
			if err := lostRmAutocommit(eng, `MATCH ()-[r:R {id: 2}]->() REMOVE r.s`); err != nil {
				t.Fatalf("P on a different relationship refused: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("T commit: %v", err)
			}
		})
		t.Run(fmt.Sprintf("wal=%v/self-reremove", walBacked), func(t *testing.T) {
			eng := lostRmEngine(t, walBacked, `CREATE (:L {s: 'a'})-[:R {s: 'a'}]->(:B)`)
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			for _, q := range []string{
				`MATCH (n:L) REMOVE n.s`, `MATCH (n:L) REMOVE n.s`, `MATCH (n:L) SET n = {}`,
				`MATCH ()-[r:R]->() REMOVE r.s`, `MATCH ()-[r:R]->() REMOVE r.s`, `MATCH ()-[r:R]->() SET r = {}`,
			} {
				if err := execInTx(tx, q); err != nil {
					t.Fatalf("own re-write %q refused: %v", q, err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		})
		t.Run(fmt.Sprintf("wal=%v/remove-absent-key-uncontended", walBacked), func(t *testing.T) {
			eng := lostRmEngine(t, walBacked, `CREATE (:L {id: 1})`)
			if err := lostRmAutocommit(eng, `MATCH (n:L) REMOVE n.never`); err != nil {
				t.Fatalf("uncontended remove of an absent key refused: %v", err)
			}
		})
	}
}
