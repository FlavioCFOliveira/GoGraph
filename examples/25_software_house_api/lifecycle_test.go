package main

import (
	"context"
	"testing"
)

// writeOne runs a write Cypher statement against ds, draining and closing
// the result.
func writeOne(t *testing.T, ds *dataStore, cypher string) {
	t.Helper()
	res, err := ds.engine.RunAny(context.Background(), cypher, nil)
	if err != nil {
		t.Fatalf("write %q: %v", cypher, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		_ = res.Close()
		t.Fatalf("write %q iterate: %v", cypher, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("write %q close: %v", cypher, err)
	}
}

// TestLifecycleSnapshotSurvives proves committed state survives a
// close/reopen when a snapshot is taken before closing.
func TestLifecycleSnapshotSurvives(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	ds1, err := openStore(ctx, dir)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	if _, err := seedFixture(ds1.txnStore); err != nil {
		t.Fatalf("seed: %v", err)
	}
	writeOne(t, ds1, "CREATE (d:Developer:People {key:'dev:zoe', name:'Zoe'})")
	if err := ds1.snapshotNow(); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := ds1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	ds2, err := openStore(ctx, dir)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	t.Cleanup(func() { _ = ds2.Close() })
	if !hasSeed(ds2.graph) {
		t.Error("seed did not survive snapshot reopen")
	}
	if n, _ := queryRows(t, ds2, "MATCH (n:Developer) RETURN n", nil); n != 7 {
		t.Errorf("Developer count after snapshot reopen = %d, want 7", n)
	}
}

// TestLifecycleWALReplaySurvives proves committed state survives a
// close/reopen with NO explicit snapshot: recovery replays the WAL on top
// of the initial empty snapshot.
func TestLifecycleWALReplaySurvives(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	ds1, err := openStore(ctx, dir)
	if err != nil {
		t.Fatalf("open #1: %v", err)
	}
	if _, err := seedFixture(ds1.txnStore); err != nil {
		t.Fatalf("seed: %v", err)
	}
	writeOne(t, ds1, "CREATE (d:Developer:People {key:'dev:zoe', name:'Zoe'})")
	// No snapshotNow: only the WAL carries the seed + the write.
	if err := ds1.Close(); err != nil {
		t.Fatalf("close #1: %v", err)
	}

	ds2, err := openStore(ctx, dir)
	if err != nil {
		t.Fatalf("open #2: %v", err)
	}
	t.Cleanup(func() { _ = ds2.Close() })
	if !hasSeed(ds2.graph) {
		t.Error("seed did not survive WAL-replay reopen")
	}
	if n, _ := queryRows(t, ds2, "MATCH (n:Developer) RETURN n", nil); n != 7 {
		t.Errorf("Developer count after WAL-replay reopen = %d, want 7", n)
	}
}
