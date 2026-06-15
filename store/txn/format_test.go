package txn

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// representativeOps returns one op of each structurally-distinct WAL frame
// shape, used to prove the pooled encoders (#1509) produce bytes identical to
// the allocating reference encoders and that buffer reuse across a sequence of
// ops never contaminates a frame.
func representativeOps() []Op[string, int64] {
	return []Op[string, int64]{
		{Kind: OpAddNode, Src: "alice"},
		{Kind: OpSetNodeLabel, Src: "alice", Label: "Person"},
		{Kind: OpAddEdgeWeighted, Src: "alice", Dst: "bob", Weight: 7},
		{Kind: OpSetNodeProperty, Src: "alice", Key: "age", Value: lpg.Int64Value(30)},
		{Kind: OpAddEdgeH, Src: "alice", Dst: "carol", Weight: 3, Handle: 42},
		{Kind: OpRemoveEdge, Src: "alice", Dst: "bob"},
	}
}

// TestEncodeOpTypedV3Into_ByteIdentical proves the pool-aware encoder
// (encodeOpTypedV3Into) produces byte-for-byte the same payload as the
// allocating reference encoder (encodeOpTypedV3) for every representative op
// shape, including when the destination buffer arrives non-empty (a grown,
// recycled pool buffer reset to length 0 but with leftover capacity). This is
// the txn-layer half of the #1509 byte-identity certification; the wal-layer
// half is TestEncode_GoldenBytes.
func TestEncodeOpTypedV3Into_ByteIdentical(t *testing.T) {
	t.Parallel()
	codec := NewStringCodec()
	wcodec := NewInt64WeightCodec()
	const seq = 12345
	for _, op := range representativeOps() {
		op := op
		t.Run(fmt.Sprintf("kind=%d", op.Kind), func(t *testing.T) {
			t.Parallel()
			want, err := encodeOpTypedV3(op, seq, codec, wcodec)
			if err != nil {
				t.Fatalf("reference encode: %v", err)
			}
			// Fresh empty buffer.
			gotFresh, err := encodeOpTypedV3Into(make([]byte, 0, 256), op, seq, codec, wcodec)
			if err != nil {
				t.Fatalf("Into (fresh): %v", err)
			}
			if !bytes.Equal(gotFresh, want) {
				t.Fatalf("fresh-buffer bytes differ:\n got %#v\nwant %#v", gotFresh, want)
			}
			// Recycled buffer: pre-dirty a larger backing array, reset to len 0,
			// and encode into it — the leftover capacity/contents must not leak
			// into the frame.
			dirty := bytes.Repeat([]byte{0xff}, 512)
			gotReused, err := encodeOpTypedV3Into(dirty[:0], op, seq, codec, wcodec)
			if err != nil {
				t.Fatalf("Into (reused): %v", err)
			}
			if !bytes.Equal(gotReused, want) {
				t.Fatalf("reused-buffer bytes differ:\n got %#v\nwant %#v", gotReused, want)
			}
		})
	}
}

// TestEncodeScratchPool_ReuseNoContamination drives the exact commit-loop
// pattern — one pooled scratch buffer reused across a whole transaction's ops
// plus the trailing OpCommit marker — and asserts each produced frame equals an
// independently fresh-buffer encode of the same op. It guards the load-bearing
// #1509 invariant that resetting and reusing the scratch between ops cannot
// corrupt a frame already handed to (and synchronously copied by) the WAL.
func TestEncodeScratchPool_ReuseNoContamination(t *testing.T) {
	t.Parallel()
	codec := NewStringCodec()
	wcodec := NewInt64WeightCodec()
	const seq = 9001
	ops := representativeOps()

	// Reference: each frame encoded into its own fresh buffer.
	want := make([][]byte, 0, len(ops)+1)
	for _, op := range ops {
		b, err := encodeOpTypedV3(op, seq, codec, wcodec)
		if err != nil {
			t.Fatalf("reference encode: %v", err)
		}
		want = append(want, b)
	}
	want = append(want, encodeCommitV3(seq))

	// Subject: one pooled scratch reused across the sequence, copying each
	// frame out immediately (as wal.Append does) before reuse.
	scratch := getEncodeScratch()
	defer putEncodeScratch(scratch)
	got := make([][]byte, 0, len(ops)+1)
	for _, op := range ops {
		payload, err := encodeOpTypedV3Into((*scratch)[:0], op, seq, codec, wcodec)
		if err != nil {
			t.Fatalf("Into: %v", err)
		}
		*scratch = payload
		got = append(got, append([]byte(nil), payload...)) // copy, mirroring Append
	}
	marker := encodeCommitV3Into((*scratch)[:0], seq)
	*scratch = marker
	got = append(got, append([]byte(nil), marker...))

	if len(got) != len(want) {
		t.Fatalf("frame count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d differs:\n got %#v\nwant %#v", i, got[i], want[i])
		}
	}
}

// TestEncodeOpTyped_V2HeaderTag verifies that the typed encoder
// prepends the v2 version marker and OpKind byte ahead of the codec
// payload, and that the trailing uint16 label length is little-endian.
func TestEncodeOpTyped_V2HeaderTag(t *testing.T) {
	t.Parallel()
	codec := NewStringCodec()
	op := Op[string, int64]{Kind: OpAddEdge, Src: "alice", Dst: "bob"}
	got, _ := encodeOpTyped(op, codec, WeightCodec[int64](nil))
	if len(got) < 2 {
		t.Fatalf("typed payload too short: %d", len(got))
	}
	if got[0] != OpRecordV2 {
		t.Fatalf("first byte = 0x%02x, want OpRecordV2 = 0x%02x", got[0], OpRecordV2)
	}
	if got[1] != byte(OpAddEdge) {
		t.Fatalf("kind byte = 0x%02x, want 0x%02x", got[1], byte(OpAddEdge))
	}
	// Walk the body: codec(src), codec(dst), uint16 labelLen, label.
	body := got[2:]
	src, rest, err := codec.Decode(body)
	if err != nil {
		t.Fatalf("decode src: %v", err)
	}
	if src != "alice" {
		t.Fatalf("src = %q, want alice", src)
	}
	dst, rest, err := codec.Decode(rest)
	if err != nil {
		t.Fatalf("decode dst: %v", err)
	}
	if dst != "bob" {
		t.Fatalf("dst = %q, want bob", dst)
	}
	if len(rest) != 2 {
		t.Fatalf("trailing region len = %d, want 2 (zero-length label prefix only)", len(rest))
	}
}
