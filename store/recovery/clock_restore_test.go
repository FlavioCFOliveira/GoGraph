package recovery_test

// clock_restore_test.go — rmp #2309 (MVCC C3b), acceptance criterion 1: after a
// close and reopen, the next commit timestamp exceeds every previously published
// one.
//
// Layer: short.
//
// # The defect this closes
//
// mvcc.Clock is process-local and constructed at ZERO on every open. Before #2309
// nothing recorded or restored a commit timestamp, so a reopened graph re-minted
// instants a previous process had already published AND made durable. The next
// commit landed at 1 — at or below timestamps already in the file — and a reader
// could reach a version that is simultaneously in its past and its future.
//
// The fix does not persist a counter. It DERIVES the floor from the largest commit
// timestamp the WAL carries and RATCHETS the clock to it, which is what InnoDB and
// Memgraph both settled on after removing theirs, and what PostgreSQL does per
// record during replay even though it also persists nextXid.

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// writeAndClose commits n nodes through a WAL-backed engine and closes it,
// returning the WAL path and the last instant the clock published.
func writeAndClose(t *testing.T, dir string, n, keyBase int) (walPath string, lastTS uint64) {
	t.Helper()
	walPath = filepath.Join(dir, "wal")
	wr, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, err := eng.RunInTx(ctx, "CREATE (x:Acct {id: $id})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(keyBase + i))}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	lastTS = g.MVCCStats().Now
	if err := wr.Close(); err != nil {
		t.Fatalf("close wal: %v", err)
	}
	return walPath, lastTS
}

// appendCommitMarker appends a bare OpCommit frame carrying commitTS to the WAL at
// path, as a transaction whose op frames are not present.
//
// # Why the test needs this rather than more ordinary commits
//
// The obvious test — commit, reopen, check the clock — passes WITHOUT the fix, and
// that was established by injecting the defect rather than assumed. Recovery mints
// an instant per replayed OP (18 for six three-op CREATE statements), so the
// replayed clock overshoots the durable maximum on its own and the restore is a
// no-op. The measurement is in docs/design-mvcc-clock-recovery.md.
//
// The restore only bites when the durable maximum EXCEEDS what replay reaches,
// which is the production case a snapshot creates: the snapshot folds the WAL
// prefix, so the file's timestamps are high while the ops left to replay are few.
// Writing a marker with a large timestamp reproduces that relationship directly and
// without a snapshot's machinery — which is C3d's to add.
//
// A marker whose ops are absent applies nothing, which is exactly what recovery
// does with an orphaned marker anyway, so it perturbs nothing but the maximum.
func appendCommitMarker(t *testing.T, path string, txnSeq, commitTS uint64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("open wal for append: %v", err)
	}
	defer func() { _ = f.Close() }()

	payload := []byte{txn.OpRecordV3, byte(txn.OpCommit)}
	payload = binary.LittleEndian.AppendUint64(payload, txnSeq)
	payload = binary.LittleEndian.AppendUint64(payload, commitTS)
	if _, err := wal.Encode(f, wal.Frame{Payload: payload}); err != nil {
		t.Fatalf("append commit marker: %v", err)
	}
}

// TestClockRestore_NextCommitExceedsEveryPreviouslyPublishedInstant is AC 1.
func TestClockRestore_NextCommitExceedsEveryPreviouslyPublishedInstant(t *testing.T) {
	dir := t.TempDir()
	const firstRun = 6
	walPath, lastBefore := writeAndClose(t, dir, firstRun, 0)
	if lastBefore == 0 {
		t.Fatal("the first process published no commit instant, so this test has no " +
			"prior clock to compare against")
	}

	// A durable instant far above anything replay will reach on its own. See
	// appendCommitMarker for why the ordinary case cannot discriminate.
	const durableMax = uint64(1_000_000)
	appendCommitMarker(t, walPath, 1<<40, durableMax)

	// Reopen through the real recovery path.
	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if res.MaxCommitTS != durableMax {
		t.Fatalf("recovery derived a maximum of %d, want %d: the instant is not being "+
			"read off the durable record, so there is nothing to derive the clock from",
			res.MaxCommitTS, durableMax)
	}

	// The recovered graph's clock must already sit above everything durable.
	if now := res.Graph.MVCCStats().Now; now <= durableMax {
		t.Fatalf("the recovered clock reads %d, at or below the %d already durable "+
			"before the crash: the next commit would re-mint an instant that is already "+
			"in the file, and a reader could reach a version that is at once in its "+
			"past and its future", now, durableMax)
	}

	// And a real commit on the recovered graph must exceed it too.
	wr, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("reopen wal: %v", err)
	}
	defer func() { _ = wr.Close() }()
	st := txn.NewStoreWithOptions[string, float64](res.Graph, wr, txn.Options[string, float64]{
		Codec:        txn.NewStringCodec(),
		WeightCodec:  txn.NewFloat64WeightCodec(),
		ResumeTxnSeq: res.MaxTxnSeq,
	})
	eng := cypher.NewEngineWithStore(st)
	if _, err := eng.RunInTx(context.Background(), "CREATE (x:Acct {id: 999})", nil); err != nil {
		t.Fatalf("commit after recovery: %v", err)
	}
	if now := res.Graph.MVCCStats().Now; now <= durableMax {
		t.Fatalf("the first commit after recovery published %d, at or below the %d "+
			"already durable before the crash", now, durableMax)
	}
}

// TestClockRestore_IsARatchetNotAnAssignment pins the direction. A floor BELOW the
// clock must not pull it back — PostgreSQL ratchets per record during replay for
// exactly this reason, rather than assigning.
func TestClockRestore_IsARatchetNotAnAssignment(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	// MVCC is armed by lpg.New and cannot be disarmed (rmp #2311); nothing to do here.
	if err := g.ApplyVersioned(func(tx lpg.WriteTx) error {
		return g.Writer(tx).AddNode("a")
	}); err != nil {
		t.Fatalf("ApplyVersioned: %v", err)
	}
	high := g.MVCCStats().Now

	g.RestoreMVCCClock(1) // a floor far below where the clock already is
	if got := g.MVCCStats().Now; got != high {
		t.Fatalf("the clock moved to %d after a floor of 1 was applied over %d: "+
			"RestoreMVCCClock must RAISE and never lower, or recovery could rewind a "+
			"clock past instants that are already visible", got, high)
	}

	g.RestoreMVCCClock(high + 100)
	if got := g.MVCCStats().Now; got != high+100 {
		t.Fatalf("the clock is %d after a floor of %d, want %d", got, high+100, high+100)
	}
}

// TestClockRestore_AWALWithNoTimestampsLeavesTheClockAlone is the older-file case:
// recovery of a WAL written before the timestamp existed must replay normally and
// simply not raise the clock.
func TestClockRestore_AWALWithNoTimestampsLeavesTheClockAlone(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")
	wr, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	// The store's OWN commit path writes commitTS 0 — "no MVCC timestamp" — which
	// is the same thing recovery sees in a pre-#2309 file.
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	st := txn.NewStoreWithOptions[string, float64](g, wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	tx := st.Begin()
	if err := tx.AddNode("a"); err != nil {
		t.Fatalf("AddNode: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := wr.Close(); err != nil {
		t.Fatalf("close wal: %v", err)
	}

	res, err := recovery.Open[string, float64](dir, recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open on a timestamp-less WAL failed: %v — an older file must "+
			"still replay, or an upgrade loses every durable transaction", err)
	}
	if res.MaxCommitTS != 0 {
		t.Fatalf("MaxCommitTS = %d for a WAL whose commits carry no instant, want 0: an "+
			"absent timestamp must contribute nothing to the derived floor", res.MaxCommitTS)
	}
	if res.WALOps == 0 {
		t.Fatal("the timestamp-less WAL replayed no ops, so this case did not actually " +
			"exercise recovery")
	}
}
