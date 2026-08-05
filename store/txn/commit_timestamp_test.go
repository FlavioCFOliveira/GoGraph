package txn

// commit_timestamp_test.go — rmp #2309 (MVCC C3a): the OpCommit marker carries the
// MVCC commit timestamp, and the format stays readable in both directions.
//
// Layer: short.
//
// The clock is restored at recovery by DERIVING it from the WAL rather than by
// trusting a persisted counter, which is what InnoDB and Memgraph both settled on
// after removing theirs. That only works if the instant is actually IN the durable
// record, and if a record written without one still replays.

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestEncodeCommitV3_CarriesTheTimestamp pins the wire layout: version, kind, the
// transaction sequence, then the commit timestamp.
func TestEncodeCommitV3_CarriesTheTimestamp(t *testing.T) {
	t.Parallel()
	const (
		seq      = uint64(0x1122334455667788)
		commitTS = uint64(0x99AABBCCDDEEFF00)
	)
	got := encodeCommitV3(seq, commitTS)

	want := []byte{OpRecordV3, byte(OpCommit)}
	want = binary.LittleEndian.AppendUint64(want, seq)
	want = binary.LittleEndian.AppendUint64(want, commitTS)
	if !bytes.Equal(got, want) {
		t.Fatalf("OpCommit payload = %#v, want %#v", got, want)
	}
	if len(got) != 18 {
		t.Fatalf("OpCommit payload is %d bytes, want 18 (2 header + 8 seq + 8 commitTS): "+
			"recovery reads the timestamp at a fixed offset", len(got))
	}
}

// TestEncodeCommitV3_PooledAndAllocatingAgree keeps the two encoders byte-identical.
// They diverged once before (rmp #1509 added the pooled form), and a divergence here
// would mean the hot path and the reference encoder write different files.
func TestEncodeCommitV3_PooledAndAllocatingAgree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ seq, ts uint64 }{
		{1, 1},
		{0, 0},
		{42, 0},
		{0, 42},
		{^uint64(0), ^uint64(0)},
	} {
		ref := encodeCommitV3(tc.seq, tc.ts)
		scratch := getEncodeScratch()
		pooled := append([]byte(nil), encodeCommitV3Into((*scratch)[:0], tc.seq, tc.ts)...)
		putEncodeScratch(scratch)
		if !bytes.Equal(ref, pooled) {
			t.Fatalf("seq=%d ts=%d: pooled %#v != allocating %#v", tc.seq, tc.ts, pooled, ref)
		}
	}
}

// TestEncodeCommitV3_ZeroTimestampIsStillWritten pins that a store-only writer — one
// with no MVCC clock, which passes zero — produces a well-formed marker rather than a
// short one.
//
// Zero means "no timestamp" to the reader, exactly as an ABSENT body does, so the two
// pre-#2309 and post-#2309 shapes agree on what a store-only commit contributes to the
// derived clock floor: nothing.
func TestEncodeCommitV3_ZeroTimestampIsStillWritten(t *testing.T) {
	t.Parallel()
	got := encodeCommitV3(7, 0)
	if len(got) != 18 {
		t.Fatalf("a zero timestamp produced a %d-byte marker, want 18: the field is "+
			"fixed-width, and a short marker would make the layout depend on the value",
			len(got))
	}
	if ts := binary.LittleEndian.Uint64(got[10:18]); ts != 0 {
		t.Fatalf("timestamp field = %d, want 0", ts)
	}
}
