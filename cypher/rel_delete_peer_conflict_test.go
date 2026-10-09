package cypher_test

// rel_delete_peer_conflict_test.go — regression tests for rmp #2986, a LOST
// DELETE of a relationship: a Cypher DELETE of a relationship another
// transaction had already removed was a silent no-op.
//
// # The defect
//
//	T1: BEGIN; MATCH ()-[r:R {id: 1}]->() DELETE r   (eager removal, uncommitted)
//	T2: BEGIN; MATCH ()-[r:R {id: 1}]->() DELETE r   (its snapshot still sees r)
//	T2: COMMIT                                       → SUCCESS
//	T1: ROLLBACK                                     → r is restored
//	final: r exists, yet T2 was told its delete committed.
//
// The DELETE operator probed the stored adjacency for the bound handle, found
// it absent (T1's pending removal), took that for "already deleted by this
// statement" (rmp #2940) and returned without reaching the engine's
// first-updater-wins check, so T2 recorded no conflict.
//
// # What these tests pin
//
// Every shape runs on BOTH wirings (in-memory engine and WAL-backed store):
//
//   - T1 rolls back: T2 is refused with [mvcc.ErrSerializationConflict] and the
//     relationship survives — no committed delete is lost.
//   - T1 commits: exactly one delete commits, and the relationship is gone.
//   - T1 commits BEFORE T2 deletes (after T2's snapshot): T2 is refused.
//
// The positive control pins that a transaction deleting the same relationship
// twice by itself (an undirected match binds it on two rows; one statement
// deletes the same bound value twice) is still accepted.
//
// The openCypher TCK does not cover concurrent transactions, so nothing here is —
// or is claimed as — TCK coverage.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/mvcc"
)

const relDelPeerSetup = `CREATE (a:N {name: 'a'})-[:R {id: 1}]->(b:N {name: 'b'}), (a)-[:R {id: 2}]->(b), (a)-[:R {id: 3}]->(a)`

// relDelPeerTargets are the relationships each shape deletes: a parallel edge
// of a multigraph pair, and a self-loop.
var relDelPeerTargets = []struct {
	name string
	id   int
}{
	{"parallel-edge", 1},
	{"self-loop", 3},
}

func relDelPeerQuery(id int) string {
	return fmt.Sprintf(`MATCH (:N {name: 'a'})-[r:R {id: %d}]->() DELETE r RETURN count(*) AS c`, id)
}

func relDelPeerCount(id int) string {
	return fmt.Sprintf(`MATCH ()-[r:R {id: %d}]->() RETURN count(r) AS v`, id)
}

// relDelPeerRefused asserts that T2 was refused with a serialization conflict,
// either at its DELETE statement or at its commit.
func relDelPeerRefused(t *testing.T, stmtErr, commitErr error) {
	t.Helper()
	if stmtErr == nil && commitErr == nil {
		t.Fatalf("T2 deleted a relationship a concurrent transaction removed, and committed: the delete was a silent no-op")
	}
	for _, err := range []error{stmtErr, commitErr} {
		if err != nil && !errors.Is(err, mvcc.ErrSerializationConflict) {
			t.Fatalf("T2 refused with a non-retryable error: %v", err)
		}
	}
}

// TestRelDelete_PeerRollbackDoesNotLoseACommittedDelete_2986 is the reported
// shape: T1 removes, T2 removes and commits, T1 rolls back.
func TestRelDelete_PeerRollbackDoesNotLoseACommittedDelete_2986(t *testing.T) {
	for _, walBacked := range []bool{false, true} {
		for _, tg := range relDelPeerTargets {
			t.Run(fmt.Sprintf("wal=%v/%s", walBacked, tg.name), func(t *testing.T) {
				ctx := context.Background()
				eng := lostRmEngine(t, walBacked, relDelPeerSetup)
				t1, t2 := beginTwo(t, ctx, eng)
				if err := execInTx(t1, relDelPeerQuery(tg.id)); err != nil {
					t.Fatalf("T1 delete: %v", err)
				}
				stmtErr := execInTx(t2, relDelPeerQuery(tg.id))
				var commitErr error
				if stmtErr == nil {
					commitErr = t2.Commit()
				}
				if err := t1.Rollback(); err != nil {
					t.Fatalf("T1 rollback: %v", err)
				}
				relDelPeerRefused(t, stmtErr, commitErr)
				if got := lostRmScalar(t, eng, relDelPeerCount(tg.id)); got != "1" {
					t.Fatalf("relationship id %d count = %s after T2 was refused and T1 rolled back, want 1", tg.id, got)
				}
				if got := lostRmScalar(t, eng, `MATCH ()-[r:R]->() RETURN count(r) AS v`); got != "3" {
					t.Fatalf("relationship count = %s, want 3", got)
				}
			})
		}
	}
}

// TestRelDelete_ConcurrentDeletesCommitExactlyOnce_2986 ends T1 with a commit:
// exactly one of the two deletes may commit.
func TestRelDelete_ConcurrentDeletesCommitExactlyOnce_2986(t *testing.T) {
	for _, walBacked := range []bool{false, true} {
		for _, tg := range relDelPeerTargets {
			t.Run(fmt.Sprintf("wal=%v/%s", walBacked, tg.name), func(t *testing.T) {
				ctx := context.Background()
				eng := lostRmEngine(t, walBacked, relDelPeerSetup)
				t1, t2 := beginTwo(t, ctx, eng)
				if err := execInTx(t1, relDelPeerQuery(tg.id)); err != nil {
					t.Fatalf("T1 delete: %v", err)
				}
				stmtErr := execInTx(t2, relDelPeerQuery(tg.id))
				var commitErr error
				if stmtErr == nil {
					commitErr = t2.Commit()
				}
				t1Err := t1.Commit()
				if t1Err != nil && !errors.Is(t1Err, mvcc.ErrSerializationConflict) {
					t.Fatalf("T1 commit failed with a non-retryable error: %v", t1Err)
				}
				t2OK := stmtErr == nil && commitErr == nil
				if t2OK == (t1Err == nil) {
					t.Fatalf("T1 committed=%v, T2 committed=%v: exactly one delete must commit", t1Err == nil, t2OK)
				}
				relDelPeerRefused(t, stmtErr, commitErr)
				if got := lostRmScalar(t, eng, relDelPeerCount(tg.id)); got != "0" {
					t.Fatalf("relationship id %d count = %s after one delete committed, want 0", tg.id, got)
				}
				if got := lostRmScalar(t, eng, `MATCH ()-[r:R]->() RETURN count(r) AS v`); got != "2" {
					t.Fatalf("relationship count = %s, want 2", got)
				}
			})
		}
	}
}

// TestRelDelete_DeleteAfterPeerCommitIsRefused_2986 commits T1's delete after
// T2's snapshot and before T2's delete: T2 still sees the relationship, and its
// delete must be refused rather than reported as committed.
func TestRelDelete_DeleteAfterPeerCommitIsRefused_2986(t *testing.T) {
	for _, walBacked := range []bool{false, true} {
		for _, tg := range relDelPeerTargets {
			t.Run(fmt.Sprintf("wal=%v/%s", walBacked, tg.name), func(t *testing.T) {
				ctx := context.Background()
				eng := lostRmEngine(t, walBacked, relDelPeerSetup)
				t1, t2 := beginTwo(t, ctx, eng)
				// Pin T2's snapshot before T1 commits.
				if err := execInTx(t2, `MATCH (n:N) RETURN count(n) AS c`); err != nil {
					t.Fatalf("T2 read: %v", err)
				}
				if err := execInTx(t1, relDelPeerQuery(tg.id)); err != nil {
					t.Fatalf("T1 delete: %v", err)
				}
				if err := t1.Commit(); err != nil {
					t.Fatalf("T1 commit: %v", err)
				}
				stmtErr := execInTx(t2, relDelPeerQuery(tg.id))
				var commitErr error
				if stmtErr == nil {
					commitErr = t2.Commit()
				}
				relDelPeerRefused(t, stmtErr, commitErr)
				if got := lostRmScalar(t, eng, relDelPeerCount(tg.id)); got != "0" {
					t.Fatalf("relationship id %d count = %s, want 0", tg.id, got)
				}
			})
		}
	}
}

// TestRelDelete_SelfRedeleteIsAccepted_2986 is the positive control: deleting a
// relationship this transaction already deleted is a no-op, never a conflict.
func TestRelDelete_SelfRedeleteIsAccepted_2986(t *testing.T) {
	for _, walBacked := range []bool{false, true} {
		t.Run(fmt.Sprintf("wal=%v", walBacked), func(t *testing.T) {
			eng := lostRmEngine(t, walBacked, relDelPeerSetup)
			tx, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			for _, q := range []string{
				// An undirected match binds the relationship on two rows.
				`MATCH (:N {name: 'a'})-[r:R {id: 1}]-() DELETE r`,
				// One statement deletes the same bound value twice.
				`MATCH (:N {name: 'a'})-[r:R {id: 2}]->() DELETE r DELETE r`,
				`MATCH (:N {name: 'a'})-[r:R {id: 3}]-() DELETE r`,
			} {
				if err := execInTx(tx, q); err != nil {
					_ = tx.Rollback()
					t.Fatalf("%q: %v", q, err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			if got := lostRmScalar(t, eng, `MATCH ()-[r:R]->() RETURN count(r) AS v`); got != "0" {
				t.Fatalf("relationship count = %s, want 0", got)
			}
		})
	}
}
