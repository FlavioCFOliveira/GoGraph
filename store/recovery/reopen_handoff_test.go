package recovery_test

// reopen_handoff_test.go — rmp #2522: the recovery-to-store handoff.
//
// Layer: short.
//
// # The defect these close
//
// Recovery derived [recovery.Result.MaxTxnSeq] on every open and every embedder
// threw it away. The only non-test assignment of [txn.Options.ResumeTxnSeq] in
// the module was the simulation harness, so a store reopened on a non-empty WAL
// restarted its sequence at 0 and re-minted 1, 2, 3 while transactions carrying
// those exact numbers were STILL PRESENT in the WAL being appended to. The
// sequence is what recovery's TxnSeq-suffix atomicity filter tells transactions
// apart by; two transactions under one number survived only because frame
// contiguity plus equality happened to disambiguate, and stops surviving the
// moment a reopen follows a torn tail.
//
// The existing coverage (store/txn/resume_txnseq_test.go) proved the FIELD
// works when hand-seeded. Nothing proved anybody seeded it. That is the gap
// these tests close: they exercise the reopen exactly as an embedder is told to
// write it, and adjudicate the outcome against the DURABLE record rather than
// an in-memory counter.

import (
	"context"
	"encoding/binary"
	"fmt"
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

// handoffOptions is the codec pair every helper here recovers and writes with.
func handoffOptions() recovery.Options[string, float64] {
	return recovery.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	}
}

// commitEpoch reopens dir THROUGH THE LIBRARY HANDOFF and commits n
// transactions, then closes the WAL.
//
// The body is deliberately the exact sequence an embedder is told to write —
// recover, refuse to append onto corruption, open the WAL, build the store off
// the Result — because that path, not the ResumeTxnSeq field, is what was
// broken. Replacing res.NewStore here with txn.NewStoreWithOptions reproduces
// the defect, which is what makes these assertions load-bearing.
func commitEpoch(t *testing.T, dir string, n, keyBase int) {
	t.Helper()
	res, err := recovery.Open[string, float64](dir, handoffOptions())
	if err != nil {
		t.Fatalf("recovery.Open: %v", err)
	}
	if !res.IsClean() {
		t.Fatalf("recovery reported corruption, refusing to append: %v", res.TailErr)
	}
	wr, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	st := res.NewStore(wr, txn.Options[string, float64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewFloat64WeightCodec(),
	})
	eng := cypher.NewEngineWithStore(st)
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if _, rerr := eng.RunInTx(ctx, "CREATE (x:Acct {id: $id})",
			map[string]expr.Value{"id": expr.IntegerValue(int64(keyBase + i))}); rerr != nil {
			t.Fatalf("commit %d: %v", i, rerr)
		}
	}
	if cerr := wr.Close(); cerr != nil {
		t.Fatalf("close wal: %v", cerr)
	}
}

// walCommitSeqs reads dir's WAL off disk and returns the TxnSeq of every v3
// commit marker, in file order. The markers are the transaction boundaries, so
// this is the list of sequence numbers the log actually spent — read from the
// DURABLE record, which is the only source that can adjudicate a claim about
// what a reopen minted.
func walCommitSeqs(t *testing.T, dir string) []uint64 {
	t.Helper()
	r, err := wal.OpenReader(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
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

// maxSeq returns the largest sequence in seqs, or 0 for an empty slice.
func maxSeq(seqs []uint64) uint64 {
	var m uint64
	for _, s := range seqs {
		if s > m {
			m = s
		}
	}
	return m
}

// TestReopen_MintsPastEverySequenceTheWALHolds is the regression for rmp #2522.
//
// It is a PROPERTY, not a fixed expectation: for any number of reopens and any
// number of transactions per epoch, every sequence a reopened store mints must
// strictly exceed every sequence already present in the WAL it is appending to.
// No epoch asserts a particular number, so the test cannot be satisfied by a
// counter that happens to line up.
//
// It reads the WAL back from disk between epochs. Asking the live store what
// sequence it is on would prove nothing: the in-memory counter is exactly the
// thing under suspicion, and the defect was that it disagreed with the file.
func TestReopen_MintsPastEverySequenceTheWALHolds(t *testing.T) {
	t.Parallel()
	shapes := [][]int{
		{1, 1},     // the minimum that can reopen at all
		{2, 3},     // the original reproduction: 1,2 then 1,2,3 again
		{5, 1, 7},  // three epochs — the floor must hold across REPEATED reopens
		{17, 4, 4}, // enough per epoch that an off-by-one floor still collides
	}
	for _, shape := range shapes {
		t.Run(fmt.Sprint(shape), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var prior []uint64
			keyBase := 0
			for epoch, n := range shape {
				commitEpoch(t, dir, n, keyBase)
				keyBase += n

				got := walCommitSeqs(t, dir)
				// Assert the epoch was actually observed: an oracle that cannot
				// fail proves nothing, and a silently-empty WAL would satisfy
				// every ordering claim below vacuously.
				if len(got) <= len(prior) {
					t.Fatalf("epoch %d committed %d transactions but the WAL gained no "+
						"commit markers (%d before, %d after): the durable record is not "+
						"being read, or the commits never reached the log",
						epoch, n, len(prior), len(got))
				}
				// The already-durable prefix must be untouched by the reopen.
				for i, want := range prior {
					if got[i] != want {
						t.Fatalf("epoch %d rewrote an already-durable sequence at marker %d: "+
							"%d became %d", epoch, i, want, got[i])
					}
				}
				// THE PROPERTY: everything this epoch minted sits strictly above
				// everything the log already held.
				floor := maxSeq(prior)
				for i, s := range got[len(prior):] {
					if epoch > 0 && s <= floor {
						t.Fatalf("epoch %d minted sequence %d at new marker %d, but the WAL "+
							"already held sequences up to %d (full log: %v). One WAL now holds "+
							"two different transactions under one sequence number, which is "+
							"exactly what recovery's TxnSeq-suffix atomicity filter "+
							"distinguishes them by", epoch, s, i, floor, got)
					}
				}
				prior = got
			}
			// Global invariant: across every epoch the log is strictly increasing,
			// so no sequence is spent twice anywhere in the file.
			for i := 1; i < len(prior); i++ {
				if prior[i] <= prior[i-1] {
					t.Fatalf("the durable sequence is not strictly increasing at marker %d: %v",
						i, prior)
				}
			}
			t.Logf("shape %v -> durable sequences %v", shape, prior)
		})
	}
}

// TestReplayWAL_RestoresTheMVCCClockFloor closes the second half of rmp #2522:
// the WAL-ONLY recovery core left the MVCC clock floor to whatever replay
// happened to mint, while the snapshot+WAL core restored it (rmp #2309). The
// one caller that got it right carried a hand-copied duplicate of the restore
// in its own harness, which is the same mis-placement as the sequence handoff.
//
// The durable maximum must EXCEED what replay reaches on its own or the test
// cannot discriminate — recovery mints an instant per replayed op, so an
// ordinary commit run overshoots the file's maximum and the restore is a no-op.
// See appendCommitMarker in clock_restore_test.go for the full reasoning; this
// reproduces the same relationship a snapshot creates in production.
func TestReplayWAL_RestoresTheMVCCClockFloor(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	walPath, published := writeAndClose(t, dir, 6, 0)
	if published == 0 {
		t.Fatal("the writer published no commit instant, so there is no clock to restore")
	}
	const durableMax = uint64(1_000_000)
	appendCommitMarker(t, walPath, 1<<40, durableMax)

	r, err := wal.OpenReader(walPath)
	if err != nil {
		t.Fatalf("wal.OpenReader: %v", err)
	}
	defer func() {
		if cerr := r.Close(); cerr != nil {
			t.Errorf("reader Close: %v", cerr)
		}
	}()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	res, err := recovery.ReplayWAL[string, float64](
		context.Background(), r, g,
		txn.NewStringCodec(), txn.NewFloat64WeightCodec(), txn.DefaultMaxTxnOps)
	if err != nil {
		t.Fatalf("recovery.ReplayWAL: %v", err)
	}
	if res.MaxCommitTS != durableMax {
		t.Fatalf("ReplayWAL derived a maximum instant of %d, want %d: the floor is not "+
			"being read off the durable record", res.MaxCommitTS, durableMax)
	}
	if now := g.MVCCStats().Now; now <= durableMax {
		t.Fatalf("after a WAL-only recovery the clock reads %d, at or below the %d already "+
			"durable: the next commit would re-mint an instant that is already in the file, "+
			"and a reader could reach a version that is at once in its past and its future",
			now, durableMax)
	}
}
