package recovery

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRecovery_OrphanedPendingOpsDiscardedOnTxnSeqMismatch is the gate
// test for the txnSeq-blind pending buffer: ops from a transaction whose
// OpCommit marker was never written (an aborted commit) must NOT be
// applied when a LATER transaction's OpCommit is read.
//
// Without the fix, the v3 replay loop flushes the whole pending buffer
// on ANY OpCommit without comparing TxnSeq, so the orphaned tx1 ops are
// resurrected fused into tx2 — an Atomicity violation (an aborted
// transaction becomes visible because an unrelated one committed).
//
// Sequence:
//
//  1. On ONE store (so txnSeq is monotonic: tx1 = seq 1, tx2 = seq 2),
//     commit tx1 (AddNode "a") then tx2 (AddNode "b") and close the WAL.
//  2. Splice tx1's OpCommit frame out of the WAL file, simulating a
//     commit that failed between its data frames and its marker while
//     the WAL kept growing. Frames are independently CRC'd, so the
//     remaining sequence is well-formed: AddNode("a") seq 1 (orphaned),
//     AddNode("b") seq 2, OpCommit seq 2.
//  3. recovery.Open: only tx2 may be applied. WALOps == 1, node "b"
//     present, node "a" ABSENT. Before the fix WALOps == 2 and "a" is
//     resurrected.
func TestRecovery_OrphanedPendingOpsDiscardedOnTxnSeqMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}

	// Step 1: two transactions on the same store, so they carry distinct
	// monotonic sequences (tx1 = 1, tx2 = 2).
	w, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := txn.NewStoreWithOptions[string, int64](g, w, opts)

	tx1 := s.Begin()
	if err := tx1.AddNode("a"); err != nil {
		t.Fatalf("tx1 AddNode(a): %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("tx1 Commit: %v", err)
	}
	tx2 := s.Begin()
	if err := tx2.AddNode("b"); err != nil {
		t.Fatalf("tx2 AddNode(b): %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("tx2 Commit: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("wal Close: %v", err)
	}

	// Step 2: locate tx1's OpCommit frame (Kind == OpCommit, TxnSeq == 1)
	// by walking the frames with the production decoder, then rewrite the
	// file without it.
	raw, err := os.ReadFile(walPath) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	r := bytes.NewReader(raw)
	total := len(raw)
	spliceStart, spliceEnd := -1, -1
	for {
		frameStart := total - r.Len()
		f, derr := wal.Decode(r)
		if derr != nil {
			break // clean EOF surfaces as ErrTornFrame; the walk is done
		}
		frameEnd := total - r.Len()
		op, oerr := Decode(f.Payload)
		if oerr != nil {
			t.Fatalf("Decode frame at offset %d: %v", frameStart, oerr)
		}
		if op.Kind == txn.OpCommit && op.TxnSeq == 1 {
			spliceStart, spliceEnd = frameStart, frameEnd
		}
	}
	if spliceStart < 0 {
		t.Fatal("tx1 OpCommit frame not found in WAL")
	}
	spliced := make([]byte, 0, len(raw)-(spliceEnd-spliceStart))
	spliced = append(spliced, raw[:spliceStart]...)
	spliced = append(spliced, raw[spliceEnd:]...)
	if err := os.WriteFile(walPath, spliced, 0o600); err != nil {
		t.Fatalf("WriteFile (spliced WAL): %v", err)
	}

	// Step 3: recovery must apply ONLY tx2. The orphaned tx1 ops precede
	// tx2's in the buffer and carry TxnSeq 1 != 2; the suffix filter must
	// discard them instead of fusing them into tx2's commit.
	res, err := Open[string, int64](dir, OptionsFromTxn(opts))
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.IsClean() {
		t.Fatalf("IsClean = false, TailErr = %v (spliced WAL ends at a clean frame boundary)", res.TailErr)
	}
	if res.WALOps != 1 {
		t.Fatalf("WALOps = %d, want 1 (orphaned tx1 op merged into tx2's commit)", res.WALOps)
	}
	if _, ok := res.Graph.AdjList().Mapper().Lookup("b"); !ok {
		t.Fatal("node 'b' (committed tx2) missing after recovery")
	}
	if _, ok := res.Graph.AdjList().Mapper().Lookup("a"); ok {
		t.Fatal("node 'a' present after recovery: aborted tx1 was resurrected by tx2's OpCommit (Atomicity violation)")
	}
	if res.WALTailOffset != int64(len(spliced)) {
		t.Fatalf("WALTailOffset = %d, want %d", res.WALTailOffset, int64(len(spliced)))
	}
}
