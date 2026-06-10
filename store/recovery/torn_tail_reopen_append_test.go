package recovery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestRecovery_TornTailTruncatedOnReopenForAppend is the gate test for
// the torn-tail reopen-for-append durability hole: a crash that leaves
// a CRC-valid prefix plus a torn trailing frame must not make
// transactions committed AFTER the reopen unreadable.
//
// Without the fix, [wal.Open] reopens the file with O_APPEND and never
// truncates, so the second transaction's frames land after the torn
// junk; every reader stops at the first torn frame, and a transaction
// whose Commit returned nil is permanently lost on the next recovery —
// a Durability violation.
//
// Sequence:
//
//  1. Commit tx1 (AddNode "a") and close the WAL.
//  2. Append a 10-byte partial frame header — a torn tail.
//  3. recovery.Open: must be clean (torn tails are benign), recover
//     exactly tx1, and report WALTailOffset at the pre-junk boundary.
//  4. wal.Open again (must discard the torn tail) and commit tx2
//     (AddNode "b"); Commit returns nil — the durability contract.
//  5. recovery.Open again: tx2 MUST be visible (WALOps == 2, node "b"
//     present) and the WAL must end at a clean frame boundary.
//
//nolint:gocyclo // test: two commit/recover cycles with per-step asserts
func TestRecovery_TornTailTruncatedOnReopenForAppend(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal")

	opts := txn.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	}

	// Step 1: commit tx1 and close the WAL cleanly.
	w1, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open (first): %v", err)
	}
	g1 := lpg.New[string, int64](adjlist.Config{Directed: true})
	s1 := txn.NewStoreWithOptions[string, int64](g1, w1, opts)
	tx1 := s1.Begin()
	if err := tx1.AddNode("a"); err != nil {
		t.Fatalf("tx1 AddNode(a): %v", err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatalf("tx1 Commit: %v", err)
	}
	if err := w1.Close(); err != nil {
		t.Fatalf("wal Close (first): %v", err)
	}
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("Stat after tx1: %v", err)
	}
	durableSize := info.Size()

	// Step 2: append a 10-byte partial frame header (HeaderSize is 14,
	// so 10 zero bytes are an unfinished header) to simulate a crash
	// mid-write after the last fsync.
	f, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // path under t.TempDir
	if err != nil {
		t.Fatalf("OpenFile for torn append: %v", err)
	}
	if _, err := f.Write(make([]byte, 10)); err != nil {
		t.Fatalf("write torn junk: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync torn junk: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close torn junk: %v", err)
	}

	// Step 3: first recovery — torn tail is benign, tx1 is recovered.
	res1, err := Open[string, int64](dir, OptionsFromTxn(opts))
	if err != nil {
		t.Fatalf("recovery.Open (first): %v", err)
	}
	if !res1.IsClean() {
		t.Fatalf("IsClean = false after benign torn tail, TailErr = %v", res1.TailErr)
	}
	if res1.WALOps != 1 {
		t.Fatalf("first recovery WALOps = %d, want 1", res1.WALOps)
	}
	if res1.WALTailOffset != durableSize {
		t.Fatalf("first recovery WALTailOffset = %d, want %d (pre-junk boundary)",
			res1.WALTailOffset, durableSize)
	}
	if _, ok := res1.Graph.AdjList().Mapper().Lookup("a"); !ok {
		t.Fatal("node 'a' missing after first recovery")
	}

	// Step 4: reopen the WAL for append. Open must truncate the torn
	// junk so the new frames start at the last durable boundary.
	w2, err := wal.Open(walPath)
	if err != nil {
		t.Fatalf("wal.Open (second): %v", err)
	}
	info, err = os.Stat(walPath)
	if err != nil {
		t.Fatalf("Stat after reopen: %v", err)
	}
	if info.Size() != durableSize {
		t.Errorf("wal.Open left file at %d bytes, want %d (torn tail not truncated)",
			info.Size(), durableSize)
	}
	s2 := txn.NewStoreWithOptions[string, int64](res1.Graph, w2, opts)
	tx2 := s2.Begin()
	if err := tx2.AddNode("b"); err != nil {
		t.Fatalf("tx2 AddNode(b): %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("tx2 Commit: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("wal Close (second): %v", err)
	}

	// Step 5: second recovery — tx2 MUST be readable. This is the gate:
	// before the fix the reader stops at the stale torn frame and never
	// reaches tx2, even though its Commit returned nil.
	res2, err := Open[string, int64](dir, OptionsFromTxn(opts))
	if err != nil {
		t.Fatalf("recovery.Open (second): %v", err)
	}
	if res2.TailErr != nil {
		t.Fatalf("second recovery TailErr = %v, want nil (clean boundary)", res2.TailErr)
	}
	if res2.WALOps != 2 {
		t.Fatalf("second recovery WALOps = %d, want 2 (tx2 lost after reopen-for-append)",
			res2.WALOps)
	}
	if _, ok := res2.Graph.AdjList().Mapper().Lookup("a"); !ok {
		t.Fatal("node 'a' missing after second recovery")
	}
	if _, ok := res2.Graph.AdjList().Mapper().Lookup("b"); !ok {
		t.Fatal("node 'b' (committed tx2) missing after second recovery: durability violation")
	}
	finalInfo, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("Stat (final): %v", err)
	}
	if res2.WALTailOffset != finalInfo.Size() {
		t.Fatalf("second recovery WALTailOffset = %d, want file size %d",
			res2.WALTailOffset, finalInfo.Size())
	}
}
