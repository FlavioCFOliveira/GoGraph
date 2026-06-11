package cypher_test

// result_cap_write_atomicity_test.go — regression gate for task #1338: a write
// statement whose result drain trips a bounded-resource guard (MaxResultRows or
// MaxResultBytes) must be rolled back ATOMICALLY, never partially committed and
// made durable.
//
// Before the fix, Result.materialize set r.rowsErr and broke out of the drain
// loop, but commitUnderBarrier consulted only rs.Err() (the pipeline error) when
// choosing between commit and rollback. The rows pulled before the trip had
// already applied their CREATE mutations eagerly in memory, so the success path
// ran: CommitWALOnly fsynced the partial transaction and the undo log was
// dropped. The caller received ErrResultRowsExceeded for a half-applied,
// durably-committed statement — an Atomicity violation.
//
// These tests drive the PUBLIC engine on both wirings the AC names:
//   - store-less (in-memory only): after the cap trips, the live graph must
//     hold ZERO nodes (the eager mutations were undone inside the barrier);
//   - WAL-backed: additionally, a fresh recovery.Open of the WAL directory must
//     find ZERO nodes (nothing was fsynced for the rolled-back statement).
//
// Each test FAILS on the pre-fix code (nodes survive, and for the WAL wiring
// they recover from disk) and PASSES after commitUnderBarrier treats a non-nil
// rowsErr exactly like a drain error.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// liveNodeCount (tombstone-aware, exectx_test.go) is the "0 nodes exist"
// oracle: a rolled-back CREATE may leave its key tombstoned rather than absent.

// runCappedWrite executes a write statement expected to trip a result cap,
// asserts the sentinel surfaces via Result.Err(), and closes the result. It
// also asserts Next() serves zero rows: once a cap trips, the truncated rows
// must not be iterable.
func runCappedWrite(t *testing.T, eng *cypher.Engine, query string, wantErr error) {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("RunInTx: %v", err)
	}
	defer func() { _ = res.Close() }()
	rows := 0
	for res.Next() {
		rows++
	}
	if rows != 0 {
		t.Errorf("Next served %d rows after a cap trip, want 0", rows)
	}
	if got := res.Err(); !errors.Is(got, wantErr) {
		t.Fatalf("Result.Err() = %v, want %v", got, wantErr)
	}
}

// TestRunInTx_RowCapTrip_RollsBackAtomically_Storeless is the store-less half
// of the #1338 gate: a CREATE producing more RETURN rows than MaxResultRows
// must leave ZERO nodes in the live in-memory graph — the rows applied before
// the trip are undone inside the visibility barrier, not kept.
func TestRunInTx_RowCapTrip_RollsBackAtomically_Storeless(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultRows: 3})

	runCappedWrite(t, eng,
		"UNWIND range(1, 8) AS i CREATE (n:Item {idx: i}) RETURN n",
		cypher.ErrResultRowsExceeded)

	if n := liveNodeCount(g); n != 0 {
		t.Fatalf("live graph holds %d nodes after a cap-tripped write, want 0 (partial commit)", n)
	}
	// The rollback must also revert the side-effect bookkeeping (#1282): a
	// statement that kept nothing created nothing.
	if na, nr, ea, er := g.SideEffectCounters(); na != 0 || nr != 0 || ea != 0 || er != 0 {
		t.Errorf("side-effect counters = (na=%d nr=%d ea=%d er=%d), want all zero", na, nr, ea, er)
	}
}

// TestRunInTx_ByteCapTrip_RollsBackAtomically_Storeless mirrors the row-cap
// gate for the aggregate-byte budget (MaxResultBytes): both caps share the
// rowsErr signal, so both must drive the same atomic rollback.
func TestRunInTx_ByteCapTrip_RollsBackAtomically_Storeless(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{MaxResultBytes: 1})

	runCappedWrite(t, eng,
		"UNWIND range(1, 8) AS i CREATE (n:Item {name: 'padding-padding-padding'}) RETURN n",
		cypher.ErrResultBytesExceeded)

	if n := liveNodeCount(g); n != 0 {
		t.Fatalf("live graph holds %d nodes after a byte-cap-tripped write, want 0 (partial commit)", n)
	}
}

// TestRunInTx_RowCapTrip_RollsBackAtomically_WALBacked is the WAL-backed half
// of the #1338 gate. Beyond the live-graph assertion, it proves DURABILITY was
// never granted to the truncated statement: after closing the WAL writer, a
// fresh recovery.Open of the directory must reconstruct an EMPTY graph. On the
// pre-fix code CommitWALOnly fsynced the partial transaction, so recovery finds
// the nodes and this test fails.
func TestRunInTx_RowCapTrip_RollsBackAtomically_WALBacked(t *testing.T) {
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	store := txn.NewStoreWithOptions[string, float64](g, w, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithOptions(g, cypher.EngineOptions{Store: store, MaxResultRows: 3})

	runCappedWrite(t, eng,
		"UNWIND range(1, 8) AS i CREATE (n:Item {idx: i}) RETURN n",
		cypher.ErrResultRowsExceeded)

	// (1) Live in-memory graph: the eager mutations were rolled back under the
	// barrier, so nothing is visible.
	if n := liveNodeCount(g); n != 0 {
		t.Fatalf("live graph holds %d nodes after a cap-tripped write, want 0 (partial commit)", n)
	}

	// (2) Durability: nothing was fsynced for the rolled-back statement. Close
	// the writer (flushing any buffered frames a buggy commit would have
	// appended) and recover from disk.
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
	rec, err := recovery.Open[string, float64](dir, recOpts())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if n := liveNodeCount(rec.Graph); n != 0 {
		t.Fatalf("recovered graph holds %d nodes after a cap-tripped write, want 0 (partial write made durable)", n)
	}

	// (3) The single-writer mutex was released inside the barrier: a follow-up
	// write on the same engine must not deadlock. It fails (the WAL is closed)
	// but must return promptly rather than block on Begin.
	if err := runWrite(t, eng, `CREATE (:Item {idx: 99})`); err == nil {
		t.Error("expected the post-trip write to fail on the closed WAL, got nil")
	}
}
