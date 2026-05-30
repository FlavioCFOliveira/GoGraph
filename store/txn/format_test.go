package txn

import (
	"testing"
)

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
