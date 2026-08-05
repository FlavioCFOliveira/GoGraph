package recovery

// commit_timestamp_test.go — rmp #2309 (MVCC C3a): recovery reads the MVCC commit
// timestamp off the OpCommit marker, and tolerates a marker written before the field
// existed.
//
// Layer: short.
//
// # Why "absent" must be distinguishable from "zero"
//
// Recovery derives the clock's floor as one past the largest commit timestamp it
// sees. A marker with no timestamp must contribute NOTHING to that maximum; a marker
// whose timestamp genuinely is zero must contribute zero. A bare uint64 return would
// collapse the two and make an old file look like a file full of commits at instant
// zero — which is harmless today only because zero is also the floor, and would stop
// being harmless the moment the derivation changes.

import (
	"encoding/binary"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/txn"
)

// commitPayload builds an OpCommit v3 payload the way store/txn's encoder does.
func commitPayload(seq uint64, ts []byte) []byte {
	p := []byte{txn.OpRecordV3, byte(txn.OpCommit)}
	p = binary.LittleEndian.AppendUint64(p, seq)
	return append(p, ts...)
}

func TestOpCommitTS_DecodesTheTimestamp(t *testing.T) {
	t.Parallel()
	const (
		seq  = uint64(77)
		want = uint64(0x0123456789ABCDEF)
	)
	ts := binary.LittleEndian.AppendUint64(nil, want)

	op, err := Decode(commitPayload(seq, ts))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if op.Kind != txn.OpCommit {
		t.Fatalf("Kind = %v, want OpCommit", op.Kind)
	}
	if op.TxnSeq != seq {
		t.Fatalf("TxnSeq = %d, want %d", op.TxnSeq, seq)
	}
	got, ok := op.CommitTS()
	if !ok {
		t.Fatal("CommitTS reported ABSENT for a marker that carries one: recovery would " +
			"derive a clock floor below timestamps that are already durable, and could " +
			"re-mint an instant that was made visible before the crash")
	}
	if got != want {
		t.Fatalf("CommitTS = %d, want %d", got, want)
	}
}

// TestOpCommitTS_AbsentOnAPreviousFormatMarker is the backward-compatibility case, and
// it is the whole of the compatibility policy: no version negotiation, because the
// frame header carries no per-record shape and an older reader ignored this body.
func TestOpCommitTS_AbsentOnAPreviousFormatMarker(t *testing.T) {
	t.Parallel()
	op, err := Decode(commitPayload(5, nil)) // the pre-#2309 shape: empty body
	if err != nil {
		t.Fatalf("Decode of a pre-#2309 OpCommit failed: %v — an older WAL must still "+
			"replay, or an upgrade loses every durable transaction", err)
	}
	if op.Kind != txn.OpCommit || op.TxnSeq != 5 {
		t.Fatalf("marker decoded as kind=%v seq=%d, want OpCommit seq=5", op.Kind, op.TxnSeq)
	}
	if ts, ok := op.CommitTS(); ok {
		t.Fatalf("CommitTS reported PRESENT (%d) for a marker with an empty body: an "+
			"absent timestamp must contribute nothing to the derived clock floor, and "+
			"reporting one invents an instant the file never recorded", ts)
	}
}

// TestOpCommitTS_ZeroIsPresentAndNotAbsent is the discrimination the two cases above
// exist to make.
func TestOpCommitTS_ZeroIsPresentAndNotAbsent(t *testing.T) {
	t.Parallel()
	op, err := Decode(commitPayload(9, binary.LittleEndian.AppendUint64(nil, 0)))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	got, ok := op.CommitTS()
	if !ok || got != 0 {
		t.Fatalf("CommitTS = (%d, %v), want (0, true): a written zero is PRESENT, and "+
			"conflating it with an absent field is the distinction this method exists for",
			got, ok)
	}
}

// TestOpCommitTS_IsOnlyForTheCommitMarker keeps an ordinary op's body — which is
// codec-encoded endpoints, not a timestamp — from being read as one.
func TestOpCommitTS_IsOnlyForTheCommitMarker(t *testing.T) {
	t.Parallel()
	p := []byte{txn.OpRecordV3, byte(txn.OpAddNode)}
	p = binary.LittleEndian.AppendUint64(p, 3)
	p = append(p, "some codec-encoded node body"...)

	op, err := Decode(p)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ts, ok := op.CommitTS(); ok {
		t.Fatalf("CommitTS reported PRESENT (%d) for an OpAddNode frame: it would be "+
			"reading codec bytes as an instant, and the derived clock floor would jump "+
			"to whatever those bytes happen to spell", ts)
	}
}

// TestOpCommitTS_ShortBodyIsAbsentNotAnError pins the truncation case. A marker whose
// body is present but under 8 bytes cannot be produced by the encoder, so seeing one
// means the file is damaged — and the replay loop, not this accessor, is what decides
// what a damaged tail means.
func TestOpCommitTS_ShortBodyIsAbsentNotAnError(t *testing.T) {
	t.Parallel()
	op, err := Decode(commitPayload(1, []byte{1, 2, 3}))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if ts, ok := op.CommitTS(); ok {
		t.Fatalf("CommitTS reported PRESENT (%d) for a 3-byte body: a partial timestamp "+
			"is not a timestamp, and reading past it would be a bounds violation", ts)
	}
}
