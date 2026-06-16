package sim

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
)

// runWrite executes a mutating Cypher statement through the engine's autocommit
// write path and drains the result, failing the test on error.
func runWrite(t *testing.T, s *SimStore, query string) {
	t.Helper()
	res, err := s.Engine().RunInTx(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunInTx(%q): %v", query, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result err for %q: %v", query, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close for %q: %v", query, err)
	}
}

// scalarInt runs a count query and returns its first integer column.
func scalarInt(t *testing.T, s *SimStore, query string) int64 {
	t.Helper()
	res, err := s.Engine().Run(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", query, err)
	}
	defer func() { _ = res.Close() }()
	var n int64
	if res.Next() {
		if v, ok := res.ValueAt(0).(expr.IntegerValue); ok {
			n = int64(v)
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("result err for %q: %v", query, err)
	}
	return n
}

// TestSimStore_CommitReopenRecovers is the #1540 acceptance test: open a DB on a
// SimDisk, commit nodes and an edge, close, then reopen via the REAL recovery
// path (ReplayWAL over the SimDisk WAL bytes) and confirm every committed write
// survives — all in-memory, no real filesystem.
func TestSimStore_CommitReopenRecovers(t *testing.T) {
	disk := NewSimDisk(NewSeed(1), 0) // no faults: a clean commit/reopen cycle.

	s, err := OpenSimStore(disk, defaultSimStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore (fresh): %v", err)
	}
	if got := s.WALOps(); got != 0 {
		t.Fatalf("fresh store WALOps = %d, want 0", got)
	}

	runWrite(t, s, "CREATE (:Person {name:'alice'})")
	runWrite(t, s, "CREATE (:Person {name:'bob'})")
	runWrite(t, s, "MATCH (a:Person {name:'alice'}), (b:Person {name:'bob'}) CREATE (a)-[:KNOWS]->(b)")

	if got := scalarInt(t, s, "MATCH (n) RETURN count(n)"); got != 2 {
		t.Fatalf("pre-crash node count = %d, want 2", got)
	}
	if got := scalarInt(t, s, "MATCH ()-[r]->() RETURN count(r)"); got != 1 {
		t.Fatalf("pre-crash edge count = %d, want 1", got)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the SAME SimDisk image through real recovery.
	s2, err := OpenSimStore(disk, defaultSimStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore (reopen): %v", err)
	}
	defer func() { _ = s2.Close() }()
	if !s2.Clean() {
		t.Fatalf("reopen not clean")
	}
	if got := scalarInt(t, s2, "MATCH (n) RETURN count(n)"); got != 2 {
		t.Fatalf("post-recovery node count = %d, want 2", got)
	}
	if got := scalarInt(t, s2, "MATCH ()-[r]->() RETURN count(r)"); got != 1 {
		t.Fatalf("post-recovery edge count = %d, want 1", got)
	}
	if got := scalarInt(t, s2, "MATCH (p:Person {name:'alice'}) RETURN count(p)"); got != 1 {
		t.Fatalf("post-recovery alice survives = %d, want 1", got)
	}
}

// TestSimStore_CrashKeepsDurableBytes proves a Crash (no graceful close) still
// recovers every op that was committed-and-synced before it, because the WAL
// bytes live in the SimDisk image, not in the dropped in-memory engine.
func TestSimStore_CrashKeepsDurableBytes(t *testing.T) {
	disk := NewSimDisk(NewSeed(2), 0)

	s, err := OpenSimStore(disk, defaultSimStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore: %v", err)
	}
	runWrite(t, s, "CREATE (:T {k:1})")
	runWrite(t, s, "CREATE (:T {k:2})")
	runWrite(t, s, "CREATE (:T {k:3})")

	// SIGKILL-equivalent: drop the engine, keep the SimDisk WAL image.
	s.Crash()

	s2, err := OpenSimStore(disk, defaultSimStoreConfig())
	if err != nil {
		t.Fatalf("OpenSimStore (post-crash): %v", err)
	}
	defer func() { _ = s2.Close() }()
	if !s2.Clean() {
		t.Fatalf("post-crash reopen not clean")
	}
	if got := scalarInt(t, s2, "MATCH (n:T) RETURN count(n)"); got != 3 {
		t.Fatalf("post-crash node count = %d, want 3 (committed+synced ops are durable)", got)
	}
	if s2.WALOps() == 0 {
		t.Fatalf("post-crash WALOps = 0, want >0 (recovery replayed the durable WAL)")
	}
}
