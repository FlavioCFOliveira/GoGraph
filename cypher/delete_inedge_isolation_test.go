package cypher_test

// delete_inedge_isolation_test.go — rmp #2884.
//
// The DELETE guard ("does this node still have relationships") and DETACH
// DELETE's removal of incoming relationships read the adjacency's in-edge index
// (graph/adjlist/reverse.go), which records node ids only and is updated at
// write time, not at commit. Write transactions run concurrently under MVCC. The
// question is whether T1's decision about node d is taken from T1's snapshot
// plus T1's own writes, as snapshot isolation requires, or from another
// transaction's in-flight or later state.
//
// Every case is a fixed, deterministic interleaving of two explicit write
// transactions driven step by step on one goroutine:
//
//	T1: BEGIN · … · <DELETE | DETACH DELETE> d · COMMIT
//	T2: BEGIN · <add | remove> the in-edge x->d · <COMMIT | ROLLBACK>
//
// in three orders: T2 still open while T1 decides and T2 commits last
// ("uncommitted"), T2 committed after T1 began but before T1 decides
// ("committed-after-begin"), and T2 still open while T1 decides and T2 rolls back
// last ("aborted").
//
// The expected outcome is derived from snapshot isolation with first-committer-
// wins conflict detection (the ACID contract in CLAUDE.md; docs/isolation-design.md):
//
//   - integrity, always: no committed relationship has a deleted endpoint;
//   - T1 decides from its snapshot: T1's DELETE is refused with
//     ErrDeleteNodeHasRelationships only when the in-edge is in T1's snapshot,
//     and it is never allowed to delete d while the in-edge is in its snapshot;
//     a serialization conflict is always an admissible alternative outcome;
//   - an aborted T2 leaves no trace: T1 never observes T2's writes, and T1's
//     outcome equals T1 run alone — either directly, or on a retry once T2 has
//     rolled back when T1 lost a serialization conflict to T2 while T2 was
//     still open.
//
// Why a conflict is admissible even when T2 later aborts: GoGraph detects
// write-write conflicts first-updater-wins (graph/mvcc/conflict.go, after
// Memgraph's PrepareForWrite), so T1 is refused when it must overwrite what an
// in-flight T2 has claimed — T2 adding or removing x->d claims d, and T1's
// DETACH DELETE must also claim x. T1 cannot know that T2 will abort, and snapshot
// isolation admits aborting either transaction; what it forbids is T1 acting on
// T2's writes, which the retry check pins.
//
// Layer: short. No goroutines are spawned.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

type inEdgeCase struct {
	detach   bool   // T1 runs DETACH DELETE rather than DELETE
	t2Adds   bool   // T2 adds the in-edge (else removes the one the setup created)
	schedule string // "uncommitted" | "committed-after-begin" | "aborted"
}

func (c inEdgeCase) name() string {
	op := "DELETE"
	if c.detach {
		op = "DETACH_DELETE"
	}
	t2 := "remove"
	if c.t2Adds {
		t2 = "add"
	}
	return fmt.Sprintf("%s/T2-%s-in-edge/%s", op, t2, c.schedule)
}

type inEdgeOutcome struct {
	t1Stmt, t1Commit, t2Write, t2End error
	// retried reports that T1 lost a serialization conflict in the "aborted"
	// schedule and was run again, alone, after T2 rolled back; retryStmt and
	// retryCommit are that run's results.
	retried                bool
	retryStmt, retryCommit error
	dDeleted               bool // d is gone in the committed state
	edgeCommitted          bool // an x->d relationship slot is committed
	dangling               bool // a committed relationship has a deleted endpoint
}

func inEdgeGraph(t *testing.T, withEdge bool) (*cypher.Engine, *lpg.Graph[string, float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := cypher.NewEngine(g)
	q := "CREATE (:X {k: 'x'}), (:D {k: 'd'})"
	if withEdge {
		q = "CREATE (:X {k: 'x'})-[:R]->(:D {k: 'd'})"
	}
	res, err := eng.RunAny(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	_ = res.Close()
	return eng, g
}

// keyOf returns the graph key of the single node whose property k equals v.
func keyOf(t *testing.T, g *lpg.Graph[string, float64], v string) (string, graph.NodeID) {
	t.Helper()
	var keys []string
	var ids []graph.NodeID
	g.AdjList().Mapper().Walk(func(id graph.NodeID, key string) bool {
		keys = append(keys, key)
		ids = append(ids, id)
		return true
	})
	for i, key := range keys {
		if pv, ok := g.NodeProperties(key)["k"]; ok && pv == lpg.StringValue(v) {
			return key, ids[i]
		}
	}
	t.Fatalf("no node with k=%q", v)
	return "", 0
}

func execClose(tx *cypher.ExplicitTx, q string) error {
	res, err := tx.Exec(q, nil)
	if err != nil {
		return err
	}
	return res.Close()
}

func runInEdgeCase(t *testing.T, c inEdgeCase) inEdgeOutcome {
	t.Helper()
	eng, g := inEdgeGraph(t, !c.t2Adds)
	_, xID := keyOf(t, g, "x")
	_, dID := keyOf(t, g, "d")
	ctx := context.Background()

	t2Query := "MATCH (:X)-[r:R]->(:D) DELETE r"
	if c.t2Adds {
		t2Query = "MATCH (x:X), (d:D) CREATE (x)-[:R]->(d)"
	}
	t1Query := "MATCH (d:D) DELETE d"
	if c.detach {
		t1Query = "MATCH (d:D) DETACH DELETE d"
	}

	var o inEdgeOutcome
	runT1 := func() (stmtErr, commitErr error) {
		tx, err := eng.BeginTx(ctx)
		if err != nil {
			t.Fatalf("T1 retry BEGIN: %v", err)
		}
		stmtErr = execClose(tx, t1Query)
		if stmtErr != nil {
			_ = tx.Rollback()
			return stmtErr, nil
		}
		return nil, tx.Commit()
	}
	t1, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("T1 BEGIN: %v", err)
	}
	// T1 reads before T2 writes, so T1's snapshot is taken first whether the
	// engine pins it at BEGIN or at the first statement.
	if err := execClose(t1, "MATCH (n) RETURN count(n) AS c"); err != nil {
		t.Fatalf("T1 first read: %v", err)
	}
	t2, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatalf("T2 BEGIN: %v", err)
	}
	o.t2Write = execClose(t2, t2Query)
	if c.schedule == "committed-after-begin" {
		o.t2End = t2.Commit()
	}
	o.t1Stmt = execClose(t1, t1Query)
	if o.t1Stmt != nil {
		_ = t1.Rollback()
	} else {
		o.t1Commit = t1.Commit()
	}
	switch c.schedule {
	case "uncommitted":
		o.t2End = t2.Commit()
	case "aborted":
		o.t2End = t2.Rollback()
		if isConflict(o.t1Stmt) || isConflict(o.t1Commit) {
			o.retried = true
			o.retryStmt, o.retryCommit = runT1()
		}
	}

	o.dDeleted = g.IsTombstoned(dID)
	nbs, _ := g.AdjList().LoadEntry(xID)
	for _, n := range nbs {
		if n == dID {
			o.edgeCommitted = true
		}
		if g.IsTombstoned(n) {
			o.dangling = true
		}
	}
	if !g.IsTombstoned(xID) && o.dDeleted && o.edgeCommitted {
		o.dangling = true
	}
	return o
}

func isHasRels(err error) bool { return errors.Is(err, exec.ErrDeleteNodeHasRelationships) }
func isConflict(err error) bool {
	return errors.Is(err, cypher.ErrSerializationConflict)
}

func TestDeleteInEdgeIndex_DecidesFromTheTransactionSnapshot(t *testing.T) {
	cases := make([]inEdgeCase, 0, 12)
	for _, detach := range []bool{false, true} {
		for _, adds := range []bool{true, false} {
			for _, s := range []string{"uncommitted", "committed-after-begin", "aborted"} {
				cases = append(cases, inEdgeCase{detach: detach, t2Adds: adds, schedule: s})
			}
		}
	}
	for _, c := range cases {
		t.Run(c.name(), func(t *testing.T) {
			o := runInEdgeCase(t, c)
			t.Logf("T2 write=%v T2 end=%v | T1 stmt=%v T1 commit=%v | retried=%v retry stmt=%v retry commit=%v | "+
				"d deleted=%v x->d committed=%v dangling=%v",
				o.t2Write, o.t2End, o.t1Stmt, o.t1Commit, o.retried, o.retryStmt, o.retryCommit,
				o.dDeleted, o.edgeCommitted, o.dangling)
			if o.t2Write != nil && !isConflict(o.t2Write) {
				t.Fatalf("premise: T2's write failed: %v", o.t2Write)
			}

			// Integrity, always.
			if o.dangling {
				t.Errorf("integrity: a committed relationship x->d has a deleted endpoint")
			}

			// T1 decides from its snapshot. The in-edge is in T1's snapshot
			// exactly when the setup created it (T2 removes it) — T2 began
			// after T1's first read.
			inSnapshot := !c.t2Adds
			t1Committed := o.t1Stmt == nil && o.t1Commit == nil
			if !c.detach {
				if isHasRels(o.t1Stmt) && !inSnapshot {
					t.Errorf("isolation: DELETE refused for a relationship absent from T1's snapshot "+
						"(created by T2, which was %s)", c.schedule)
				}
				if inSnapshot && t1Committed && o.dDeleted {
					t.Errorf("isolation: DELETE removed d although T1's snapshot holds the in-edge x->d")
				}
			}
			if o.t1Stmt != nil && !isHasRels(o.t1Stmt) && !isConflict(o.t1Stmt) {
				t.Errorf("T1 statement failed with an unexpected error: %v", o.t1Stmt)
			}

			// An aborted T2 leaves no trace: T1's outcome equals T1 alone —
			// directly, or on the retry after a serialization conflict lost to
			// the still-open T2 (see the file comment).
			if c.schedule == "aborted" {
				stmt, commit := o.t1Stmt, o.t1Commit
				if o.retried {
					stmt, commit = o.retryStmt, o.retryCommit
				}
				committed := stmt == nil && commit == nil
				switch {
				case !c.detach && c.t2Adds: // no edge: DELETE succeeds
					if !committed || !o.dDeleted {
						t.Errorf("aborted T2: DELETE of an unconnected d must commit (stmt=%v commit=%v retried=%v)",
							stmt, commit, o.retried)
					}
				case !c.detach && !c.t2Adds: // edge: DELETE refused
					if !isHasRels(stmt) {
						t.Errorf("aborted T2: DELETE of a connected d must be refused, got stmt=%v commit=%v retried=%v",
							stmt, commit, o.retried)
					}
				default: // DETACH DELETE succeeds and removes d and its in-edge
					if !committed || !o.dDeleted || o.edgeCommitted {
						t.Errorf("aborted T2: DETACH DELETE must commit with d and x->d gone "+
							"(stmt=%v commit=%v retried=%v d deleted=%v edge=%v)",
							stmt, commit, o.retried, o.dDeleted, o.edgeCommitted)
					}
				}
			}
		})
	}
}
