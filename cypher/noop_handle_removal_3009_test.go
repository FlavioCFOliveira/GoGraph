package cypher_test

// noop_handle_removal_3009_test.go — regression gate for rmp #3009: a Cypher
// REMOVE of a property the relationship does not carry also removes it from the
// instance's by-handle bag, and that removal wrote a version when the bag carried
// other keys — so it refused a peer writing another key of the same relationship
// and the durable path logged an OpDelEdgePropertyByHandle frame for nothing.
// Helpers are shared with noop_conflict_stamp_3006_3008_test.go.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// TestRemoveAbsentRelProperty_LogsNoHandleRemoval_3009: on the persisted store a
// committed removal of an absent key appends no by-handle removal frame; the
// control removal of a present key appends exactly one.
func TestRemoveAbsentRelProperty_LogsNoHandleRemoval_3009(t *testing.T) {
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
			if got := countWALOps3006(t, filepath.Join(dir, "wal"), txn.OpDelEdgePropertyByHandle); got != c.want {
				t.Errorf("REMOVE r.%s logged %d OpDelEdgePropertyByHandle records, want %d", c.key, got, c.want)
			}
		})
	}
}

// TestRemoveAbsentRelProperty_NoFalseConflictOnInstance_3009: T1 removes a key r
// does not carry and stays open; a peer then sets another property of the same
// relationship and commits. The peer must succeed, and so must T1.
func TestRemoveAbsentRelProperty_NoFalseConflictOnInstance_3009(t *testing.T) {
	t.Parallel()
	for engName, open := range noopStampEngines() {
		t.Run(engName, func(t *testing.T) {
			t.Parallel()
			eng, _, _ := open(t)
			relRemovalSetup3006(t, eng)
			t1, err := eng.BeginTx(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			noopStampExec(t, t1, removeAbsent3006)
			if err := noopStampAuto(eng, "MATCH (:Item {id:0})-[r:R]->() SET r.p = 2"); err != nil {
				_ = t1.Rollback()
				t.Fatalf("peer write of the same relationship beside an open removal of an absent key: %v", err)
			}
			if err := t1.Commit(); err != nil {
				t.Fatalf("T1 commit: %v", err)
			}
		})
	}
}
