package cypher

// commit_timestamp_wal_test.go — rmp #2309 (MVCC C3a), end to end: a durable commit
// puts its MVCC instant INTO the WAL record, and the contiguous frontier does not
// stall on the allocation that now happens before the fsync.
//
// Layer: short.
//
// # What C3a actually changed, and the risk it carries
//
// The commit timestamp used to be minted in lpg.Graph.endWrite, which runs from the
// caller's deferred teardown — strictly AFTER the WAL append and fsync. Nothing
// durable could carry it. The allocation therefore moved before the append:
//
//	AllocateCommitTS -> encode OpCommit -> fsync -> publish
//
// That is PostgreSQL's ordering, and it is a change to commit SEQUENCING rather than
// a refactor. It lengthens the window in which a timestamp is allocated but not
// visible from nanoseconds to a disk sync, and ONE unpublished timestamp holds the
// contiguous frontier back for EVERY reader. Worse, a timestamp that is allocated
// and then neither published nor abandoned stalls the frontier PERMANENTLY.
//
// So these tests assert both halves: the instant reaches the file, and every exit
// path discharges its allocation.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// walEngine builds a WAL-backed engine and returns it with the WAL path.
func walEngine(t *testing.T) (*Engine, *wal.Writer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	wr, err := wal.Open(path)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	return NewEngineWithStore(st), wr, path
}

// walCommitTimestamps replays the file and returns every commit timestamp it found,
// plus how many OpCommit markers carried none.
func walCommitTimestamps(t *testing.T, path string) (found []uint64, absent int) {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("open wal: %v", err)
	}
	defer func() { _ = f.Close() }()

	rd := wal.NewReader(f, nil)
	for fr := range rd.Frames() {
		op, derr := recovery.Decode(fr.Payload)
		if derr != nil || op.Kind != txn.OpCommit {
			continue
		}
		if ts, ok := op.CommitTS(); ok && ts != 0 {
			found = append(found, ts)
		} else {
			absent++
		}
	}
	return found, absent
}

// TestCommitTS_ReachesTheWAL is the C3a acceptance test: a WAL written by the new
// code decodes to the timestamps the clock actually published.
func TestCommitTS_ReachesTheWAL(t *testing.T) {
	eng, wr, path := walEngine(t)
	ctx := context.Background()
	const commits = 5
	for i := 0; i < commits; i++ {
		if _, err := eng.RunInTx(ctx, "CREATE (n:Acct {id: $id})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(i))}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	if err := wr.Close(); err != nil {
		t.Fatalf("close wal: %v", err)
	}

	found, absent := walCommitTimestamps(t, path)
	if len(found) < commits {
		t.Fatalf("the WAL carries %d commit timestamps for %d commits (%d markers had "+
			"none): the instant is not reaching the durable record, so recovery cannot "+
			"derive the clock from it and would restart at zero — re-minting instants "+
			"that were already made visible", len(found), commits, absent)
	}
	// Strictly increasing: the clock is monotonic and each commit takes a fresh
	// instant. A repeat would mean two transactions published at one instant.
	for i := 1; i < len(found); i++ {
		if found[i] <= found[i-1] {
			t.Fatalf("commit timestamps are not strictly increasing: %v — two "+
				"transactions sharing one instant makes the second's writes visible at "+
				"the first's, so a reader between them sees a state neither produced",
				found)
		}
	}
	// The largest durable timestamp must be one the clock actually reached, or the
	// derived floor would sit above or below what was really published.
	if last, now := found[len(found)-1], eng.g.MVCCStats().Now; last > now {
		t.Fatalf("the WAL records a commit at %d but the clock only reached %d: the "+
			"durable record claims an instant that never existed", last, now)
	}
}

// TestCommitTS_FrontierDoesNotStall is the regression for the risk C3a introduces.
//
// The allocation now happens before a disk sync, so a path that fails to publish OR
// abandon its timestamp leaves the contiguous frontier stuck forever: every later
// commit becomes invisible to new readers and the commit log grows without bound.
// The symptom is silent — writes keep succeeding — so nothing else would catch it.
func TestCommitTS_FrontierDoesNotStall(t *testing.T) {
	eng, wr, _ := walEngine(t)
	defer func() { _ = wr.Close() }()
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		if _, err := eng.RunInTx(ctx, "CREATE (n:Acct {id: $id})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(i))}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	if n := eng.g.MVCCStats().InFlightCommits; n != 0 {
		t.Fatalf("InFlightCommits = %d after every transaction finished, want 0: a "+
			"commit timestamp was allocated and then neither published nor abandoned, "+
			"which stalls the contiguous frontier PERMANENTLY — every later commit is "+
			"invisible to new readers and the commit log grows without bound", n)
	}

	// And the frontier is genuinely usable: a fresh read must see all 20.
	res, err := eng.Run(ctx, "MATCH (n:Acct) RETURN count(n) AS c", nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer func() { _ = res.Close() }()
	var got int64
	for res.Next() {
		if v, ok := res.Record()["c"].(expr.IntegerValue); ok {
			got = int64(v)
		}
	}
	if got != 20 {
		t.Fatalf("a reader at the frontier sees %d nodes, want 20: the frontier is "+
			"behind the commits, which is what an undischarged allocation looks like", got)
	}
}

// TestCommitTS_AStatementThatWritesNothingDoesNotStallTheFrontier covers the
// versioned-nothing exit, which allocates a timestamp on the durable path and then
// finds no commit record to publish it into.
func TestCommitTS_AStatementThatWritesNothingDoesNotStallTheFrontier(t *testing.T) {
	eng, wr, _ := walEngine(t)
	defer func() { _ = wr.Close() }()
	ctx := context.Background()

	// A write statement whose MATCH finds nothing: it runs the whole write bracket
	// and the whole durable commit path, and versions not one object.
	for i := 0; i < 10; i++ {
		if _, err := eng.RunInTx(ctx, "MATCH (n:Nothing) SET n.x = 1", nil); err != nil {
			t.Fatalf("empty write %d: %v", i, err)
		}
	}
	if n := eng.g.MVCCStats().InFlightCommits; n != 0 {
		t.Fatalf("InFlightCommits = %d after ten statements that versioned nothing, "+
			"want 0: the versioned-nothing exit allocated a timestamp it never "+
			"discharged, and the frontier is now stalled forever", n)
	}
}
