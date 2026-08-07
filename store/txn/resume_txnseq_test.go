package txn_test

// resume_txnseq_test.go — the transaction sequence across a close/reopen cycle
// (rmp #2302, audit finding E5, acceptance criterion 4).
//
// Design: docs/design-wal-transaction-contiguity.md.
//
// The sequence groups a transaction's frames so recovery can apply them
// atomically. It was decoded by recovery and never written back, so a store
// reopened on a non-empty WAL restarted at 0 and minted 1 again — putting two
// different transactions under one sequence number in ONE log. Recovery's
// TxnSeq-suffix filter tolerated it only because frame contiguity plus equality
// happened to disambiguate, which is an accident rather than a guarantee.
//
// Both arms are here: seeded from recovery (monotone) and unseeded (the sequence
// repeats). The second is what makes the first mean something.

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/store/recovery"
	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// commitNodes opens a store over dir's WAL, commits one transaction per key, and
// closes the WAL. resume seeds the sequence; 0 means "start from scratch", which
// is what every caller did before rmp #2302.
func commitNodes(t *testing.T, dir string, resume uint64, keys ...string) {
	t.Helper()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	st := txn.NewStoreWithOptions(g, w, txn.Options[string, int64]{
		Codec:        txn.NewStringCodec(),
		WeightCodec:  txn.NewInt64WeightCodec(),
		ResumeTxnSeq: resume,
	})
	for _, k := range keys {
		tx := st.Begin()
		if aerr := tx.AddNode(k); aerr != nil {
			t.Fatalf("AddNode(%q): %v", k, aerr)
		}
		if cerr := tx.Commit(); cerr != nil {
			t.Fatalf("Commit(%q): %v", k, cerr)
		}
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("wal Close: %v", cerr)
	}
}

// walSeqs reads dir's WAL and returns the TxnSeq of every v3 commit marker, in
// file order. The markers are the transaction boundaries, so this is the list of
// sequence numbers the log actually spent.
func walSeqs(t *testing.T, dir string) []uint64 {
	t.Helper()
	r, err := wal.OpenReader(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("reader Close: %v", cerr)
		}
	}()
	var seqs []uint64
	for f := range r.Frames() {
		p := f.Payload
		if len(p) < 10 || p[0] != txn.OpRecordV3 || p[1] != byte(txn.OpCommit) {
			continue
		}
		seqs = append(seqs, binary.LittleEndian.Uint64(p[2:10]))
	}
	return seqs
}

// recoveredMaxSeq replays dir and returns the highest sequence recovery saw.
func recoveredMaxSeq(t *testing.T, dir string) uint64 {
	t.Helper()
	res, err := recovery.Open[string, int64](dir, recovery.Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if res.TailErr != nil {
		t.Fatalf("recovery tail error: %v", res.TailErr)
	}
	return res.MaxTxnSeq
}

// TestResumeTxnSeq_IsMonotoneAcrossReopen is acceptance criterion 4.
//
// Two transactions, close, recover, seed, two more: every sequence in the log
// must be distinct and increasing. A repeat means one WAL holds two different
// transactions under one number, and recovery's atomicity filter can no longer
// tell which frames belong to which.
//
// It is ALSO the regression test for a deadlock this test found. Seeding the
// minting counter alone is not enough: waitApplyTurn parks until
// appliedSeq == seq-1, and the predecessor of a resumed store's first
// transaction was applied by the previous store instance, which no longer exists
// to advance anything. With only txnSeq seeded, this test HUNG — the first commit
// after the resume waited forever for a sequence nobody would ever complete. Both
// watermarks are seeded now; a fix that touches one and not the other brings the
// hang straight back.
func TestResumeTxnSeq_IsMonotoneAcrossReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	commitNodes(t, dir, 0, "a", "b")
	maxSeq := recoveredMaxSeq(t, dir)
	if maxSeq == 0 {
		t.Fatal("recovery reported MaxTxnSeq 0 after two committed transactions: the " +
			"sequence cannot be resumed from a value that was never observed")
	}
	commitNodes(t, dir, maxSeq, "c", "d")

	seqs := walSeqs(t, dir)
	t.Logf("commit-marker sequences across the reopen: %v (resumed from %d)", seqs, maxSeq)
	if len(seqs) != 4 {
		t.Fatalf("found %d commit markers, want 4: %v", len(seqs), seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("sequence went backwards or repeated at marker %d: %v — one WAL now "+
				"holds two transactions under one sequence number", i, seqs)
		}
	}
}

// TestResumeTxnSeq_UnseededRepeatsTheSequence is the negative arm: the defect,
// reproduced rather than argued.
//
// A store reopened WITHOUT the seed restarts at 0 and re-mints the sequences the
// log already spent. This is what every caller did before rmp #2302, and it is
// what makes ResumeTxnSeq load-bearing rather than decorative.
func TestResumeTxnSeq_UnseededRepeatsTheSequence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	commitNodes(t, dir, 0, "a", "b")
	commitNodes(t, dir, 0, "c", "d") // reopened with no seed

	seqs := walSeqs(t, dir)
	t.Logf("commit-marker sequences with no seed: %v", seqs)
	if len(seqs) != 4 {
		t.Fatalf("found %d commit markers, want 4: %v", len(seqs), seqs)
	}
	seen := make(map[uint64]int, len(seqs))
	for _, s := range seqs {
		seen[s]++
	}
	repeated := false
	for _, n := range seen {
		if n > 1 {
			repeated = true
		}
	}
	if !repeated {
		t.Fatal("an unseeded reopen produced no repeated sequence. Either the store now " +
			"resumes by default — in which case ResumeTxnSeq is redundant and " +
			"TestResumeTxnSeq_IsMonotoneAcrossReopen proves nothing — or this test no " +
			"longer reopens the store at all")
	}
	t.Logf("reproduced: %d distinct sequences across 4 transactions; the reopen re-minted "+
		"numbers the log had already spent", len(seen))
}

// TestResumeTxnSeq_ZeroStartsAtOne pins the fresh-store case, so the new field
// cannot change the behaviour of a store that has nothing to resume from.
func TestResumeTxnSeq_ZeroStartsAtOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	commitNodes(t, dir, 0, "a")
	if got := walSeqs(t, dir); len(got) != 1 || got[0] != 1 {
		t.Fatalf("a fresh store's first transaction got sequence %v, want [1]", got)
	}
}

// TestRecovery_MaxTxnSeqCountsAnIncompleteTail covers the case that makes the
// derivation correct rather than merely convenient: a sequence that was MINTED is
// spent, even if its transaction never committed.
//
// A crash between a transaction's data frames and its commit marker leaves those
// frames in the log carrying a sequence. Resuming from the highest COMMITTED
// sequence would re-mint that number, so the recovered log would hold an
// abandoned transaction and a live one under one sequence — and recovery's
// suffix filter distinguishes them by exactly that number.
func TestRecovery_MaxTxnSeqCountsAnIncompleteTail(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	commitNodes(t, dir, 0, "a") // sequence 1, committed

	// Append a data frame carrying sequence 2 with no marker: the durable state
	// after a crash mid-transaction.
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	codec := txn.NewStringCodec()
	buf := []byte{txn.OpRecordV3, byte(txn.OpAddNode)}
	buf = binary.LittleEndian.AppendUint64(buf, 2)
	if buf, err = codec.Encode(buf, "orphan"); err != nil {
		t.Fatalf("codec.Encode: %v", err)
	}
	if buf, err = codec.Encode(buf, ""); err != nil {
		t.Fatalf("codec.Encode(zero dst): %v", err)
	}
	buf = binary.LittleEndian.AppendUint16(buf, 0)
	if aerr := w.Append(buf); aerr != nil {
		t.Fatalf("Append: %v", aerr)
	}
	if serr := w.Sync(); serr != nil {
		t.Fatalf("Sync: %v", serr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}

	if got := recoveredMaxSeq(t, dir); got != 2 {
		t.Fatalf("MaxTxnSeq = %d, want 2: an uncommitted transaction still SPENT its "+
			"sequence, and re-minting it would put two transactions under one number", got)
	}
}
