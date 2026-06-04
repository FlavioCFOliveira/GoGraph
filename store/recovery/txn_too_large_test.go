package recovery

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// v3OpAddNodeFrame builds the raw WAL payload of a v3 OpAddNode frame for
// key, carrying the transaction sequence seq. The layout mirrors
// txn.encodeOpTypedV3 for an OpAddNode op:
//
//	uint8  OpRecordV3
//	uint8  OpAddNode
//	uint64 seq (little-endian)
//	codec  src   (the node key)
//	codec  zero  (the unused dst slot)
//	uint16 labelLen = 0
//
// It is the single building block for a marker-less run: appending many of
// these to a WAL WITHOUT a trailing OpCommit marker is exactly the
// pathological input the recovery cap must reject.
func v3OpAddNodeFrame(t *testing.T, codec txn.Codec[string], seq uint64, key string) []byte {
	t.Helper()
	buf := []byte{txn.OpRecordV3, byte(txn.OpAddNode)}
	buf = binary.LittleEndian.AppendUint64(buf, seq)
	var err error
	if buf, err = codec.Encode(buf, key); err != nil {
		t.Fatalf("codec.Encode(src=%q): %v", key, err)
	}
	if buf, err = codec.Encode(buf, ""); err != nil { // zero dst slot
		t.Fatalf("codec.Encode(zero dst): %v", err)
	}
	buf = binary.LittleEndian.AppendUint16(buf, 0) // labelLen = 0
	return buf
}

// writeMarkerlessV3Run writes k valid-CRC v3 OpAddNode frames to a fresh WAL
// under dir, all carrying the same transaction sequence and NO OpCommit
// marker — a legitimately huge transaction's prefix, or a crafted/corrupt
// tail. Every frame is individually well-formed (valid magic, version, CRC),
// so the WAL reader yields all k frames; only the recovery op cap stops the
// buffer from growing to k.
func writeMarkerlessV3Run(t *testing.T, dir string, k int) {
	t.Helper()
	codec := txn.NewStringCodec()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	for i := 0; i < k; i++ {
		if err := w.Append(v3OpAddNodeFrame(t, codec, 1, "n"+itoa(i))); err != nil {
			t.Fatalf("Append(frame %d): %v", i, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// openCappedTxnOps opens dir through recovery.Open with the canonical
// string+int64 codecs and an explicit (test-lowered) per-transaction op cap.
func openCappedTxnOps(t *testing.T, dir string, maxTxnOps int) (Result[string, int64], error) {
	t.Helper()
	return Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
		MaxTxnOps:   maxTxnOps,
	})
}

// TestRecovery_MarkerlessV3Run_RejectedByOpCap is the headline regression for
// task #1296: a WAL that is a long marker-less run of valid-CRC v3 frames must
// be rejected with the typed [ErrTransactionTooLarge] rather than letting
// recovery buffer all K ops before discarding them at EOF.
//
// The assertion is via the cap/error, NOT via runtime.MemStats (the project's
// flaky-MemStats guidance): with K well above a small test cap, recovery must
// stop the instant the buffer would exceed the cap. Pre-fix recovery had no
// cap — it appended all K ops into `pending`, then discarded them at the
// marker-less EOF and returned nil (allocating proportionally to K). Post-fix
// Open returns ErrTransactionTooLarge, Result.IsClean() is false, and the
// graph holds none of the run's nodes.
func TestRecovery_MarkerlessV3Run_RejectedByOpCap(t *testing.T) {
	t.Parallel()
	const (
		opCap = 64
		K     = 4096 // K >> opCap: pre-fix this would buffer all 4096 ops
	)
	dir := t.TempDir()
	writeMarkerlessV3Run(t, dir, K)

	res, err := openCappedTxnOps(t, dir, opCap)
	if err == nil {
		t.Fatal("Open returned nil for a marker-less v3 run; an over-cap transaction must be surfaced")
	}
	if !errors.Is(err, ErrTransactionTooLarge) {
		t.Fatalf("Open error = %v, want errors.Is(err, ErrTransactionTooLarge)", err)
	}
	if res.IsClean() {
		t.Fatal("Result.IsClean() = true for an over-cap transaction, want false")
	}
	if !errors.Is(res.TailErr, ErrTransactionTooLarge) {
		t.Fatalf("Result.TailErr = %v, want errors.Is(TailErr, ErrTransactionTooLarge)", res.TailErr)
	}
	if res.Graph == nil {
		t.Fatal("Result.Graph must be non-nil even on rejection (diagnostics)")
	}
	// None of the marker-less run's ops were applied (no OpCommit was ever
	// read; the run was rejected before any apply).
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (over-cap transaction applies nothing)", res.WALOps)
	}
	if _, ok := res.Graph.AdjList().Mapper().Lookup("n0"); ok {
		t.Error("node n0 from the rejected run must not be present in the graph")
	}
}

// TestRecovery_TxnOpCap_BoundaryExactAndOverByOne pins the AC boundary: a
// transaction of EXACTLY the cap succeeds, and cap+1 fails. Both are built as
// a complete, committed v3 transaction (cap / cap+1 op frames followed by one
// OpCommit marker) through the real producer, then replayed with a recovery
// cap set to exactly `cap`.
func TestRecovery_TxnOpCap_BoundaryExactAndOverByOne(t *testing.T) {
	t.Parallel()
	const opCap = 32

	t.Run("exactly_cap_succeeds", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCommittedTxnOfSize(t, dir, opCap)

		res, err := openCappedTxnOps(t, dir, opCap)
		if err != nil {
			t.Fatalf("Open(exactly cap): %v, want nil", err)
		}
		if !res.IsClean() {
			t.Fatalf("Result.IsClean() = false at exactly the cap, want true (TailErr=%v)", res.TailErr)
		}
		if res.WALOps != opCap {
			t.Fatalf("WALOps = %d, want %d (a cap-sized transaction applies fully)", res.WALOps, opCap)
		}
		// Every node of the committed transaction is present.
		for i := 0; i < opCap; i++ {
			if _, ok := res.Graph.AdjList().Mapper().Lookup("n" + itoa(i)); !ok {
				t.Errorf("node n%d missing after a cap-sized recovery", i)
			}
		}
	})

	t.Run("cap_plus_one_fails", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeCommittedTxnOfSize(t, dir, opCap+1)

		res, err := openCappedTxnOps(t, dir, opCap)
		if err == nil {
			t.Fatal("Open(cap+1) returned nil, want ErrTransactionTooLarge")
		}
		if !errors.Is(err, ErrTransactionTooLarge) {
			t.Fatalf("Open(cap+1) error = %v, want errors.Is(err, ErrTransactionTooLarge)", err)
		}
		if res.IsClean() {
			t.Fatal("Result.IsClean() = true at cap+1, want false")
		}
		if res.WALOps != 0 {
			t.Fatalf("WALOps = %d at cap+1, want 0 (the over-cap transaction applies nothing)", res.WALOps)
		}
	})
}

// TestRecovery_TxnOpCap_DefaultUnreachedByOrdinaryTxn is a positive control:
// a small ordinary transaction recovered with the DEFAULT cap (MaxTxnOps left
// at its zero value, selecting txn.DefaultMaxTxnOps) replays cleanly — the
// default never trips a legitimate transaction.
func TestRecovery_TxnOpCap_DefaultUnreachedByOrdinaryTxn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeCommittedTxnOfSize(t, dir, 50)

	res, err := openCappedTxnOps(t, dir, 0) // 0 → txn.DefaultMaxTxnOps
	if err != nil {
		t.Fatalf("Open with default cap: %v, want nil", err)
	}
	if !res.IsClean() {
		t.Fatalf("Result.IsClean() = false under the default cap, want true (TailErr=%v)", res.TailErr)
	}
	if res.WALOps != 50 {
		t.Fatalf("WALOps = %d, want 50", res.WALOps)
	}
}

// writeCommittedTxnOfSize commits ONE transaction of exactly n AddNode ops
// ("n0".."n<n-1>") through the real typed store under dir, with the producer
// cap disabled so the producer never rejects the transaction we are building
// for the recovery-side boundary test. The result is n v3 op frames followed
// by one OpCommit marker — a complete, durable, atomically-committed
// transaction.
func writeCommittedTxnOfSize(t *testing.T, dir string, n int) {
	t.Helper()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptionsCapped[string, int64](g, w, txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}, txn.MaxTxnOpsUnlimited)
	tx := s.Begin()
	for i := 0; i < n; i++ {
		if err := tx.AddNode("n" + itoa(i)); err != nil {
			t.Fatalf("AddNode(n%d): %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal.Close: %v", err)
	}
}
