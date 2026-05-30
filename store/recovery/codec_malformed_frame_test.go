package recovery

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"gograph/store/txn"
	"gograph/store/wal"
)

// TestOpen_MalformedV2Frame_NotApplied is the table-driven guard battery
// for malformed v2 ([txn.OpRecordV2]) WAL frames replayed through the
// surviving string-recovery path: recovery.Open with the canonical
// string + int64 codecs, which routes every frame through openCodec ->
// applyOpCodec. It restores, in consolidated form, the malformed-frame
// robustness coverage that was lost when the OpenString-specific
// TestOpenString_TruncatedV2Body / V2MissingTrailingLabelLength /
// V2LabelOverflow tests were deleted alongside the v1 read wrappers, and
// it exercises the same three applyOpCodec guards directly through the
// public v2 API.
//
// Each case writes exactly one raw WAL frame, then asserts Open returns
// no Go-level error, applies zero ops, and leaves the alice -> bob edge
// absent — i.e. the malformed frame is detected and skipped, never
// half-applied. A clean well-formed frame is included as a positive
// control so the harness itself is proven to apply a valid frame.
func TestOpen_MalformedV2Frame_NotApplied(t *testing.T) {
	t.Parallel()

	codec := txn.NewStringCodec()
	// goodBody is a well-formed v2 OpAddEdge body: codec(src) + codec(dst)
	// + uint16 trailing label length (0). Reused as the base for the
	// malformed permutations below.
	goodBody := func() []byte {
		b, _ := codec.Encode(nil, "alice")
		b, _ = codec.Encode(b, "bob")
		return binary.LittleEndian.AppendUint16(b, 0)
	}

	cases := []struct {
		name string
		// body is everything after the {OpRecordV2, kind} header.
		body []byte
		// applied is the expected post-replay presence of alice -> bob.
		// Only the positive control sets this true.
		applied bool
	}{
		{
			// Positive control: a complete, well-formed frame applies.
			name:    "well_formed_applies",
			body:    goodBody(),
			applied: true,
		},
		{
			// Truncated body: the src string codec claims a 16-byte
			// length prefix with no bytes behind it, so codec.Decode of
			// src fails. Mirrors the deleted TestOpenString_TruncatedV2Body.
			name: "truncated_src_length_prefix",
			body: []byte{0x10, 0x00, 0x00, 0x00},
		},
		{
			// src decodes cleanly; dst claims 16 bytes with none behind
			// it, so codec.Decode of dst fails.
			name: "truncated_dst_body",
			body: func() []byte {
				b, _ := codec.Encode(nil, "alice")
				return append(b, 0x10, 0x00, 0x00, 0x00)
			}(),
		},
		{
			// src and dst decode cleanly but the mandatory trailing
			// uint16 label-length prefix is absent (rest < 2), so the
			// OpAddEdge arm of applyOpCodec rejects the frame. Mirrors the
			// deleted TestOpenString_V2MissingTrailingLabelLength.
			name: "missing_trailing_label_length",
			body: func() []byte {
				b, _ := codec.Encode(nil, "alice")
				b, _ = codec.Encode(b, "bob")
				return b // no uint16 labelLen trailer
			}(),
		},
		{
			// src and dst decode cleanly but the trailing label-length
			// prefix claims more bytes than the frame holds, so the
			// OpAddEdge arm rejects the frame. Mirrors the deleted
			// TestOpenString_V2LabelOverflow.
			name: "label_length_overflow",
			body: func() []byte {
				b, _ := codec.Encode(nil, "alice")
				b, _ = codec.Encode(b, "bob")
				b = binary.LittleEndian.AppendUint16(b, 100) // claim 100
				return append(b, 'L', 'a', 'b')              // only 3 follow
			}(),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			w, err := wal.Open(filepath.Join(dir, "wal"))
			if err != nil {
				t.Fatalf("wal.Open: %v", err)
			}
			payload := append([]byte{txn.OpRecordV2, byte(txn.OpAddEdge)}, tc.body...)
			if err := w.Append(payload); err != nil {
				t.Fatalf("Append: %v", err)
			}
			if err := w.Sync(); err != nil {
				t.Fatalf("Sync: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			res, err := Open[string, int64](dir, Options[string, int64]{
				Codec:       txn.NewStringCodec(),
				WeightCodec: txn.NewInt64WeightCodec(),
			})
			if err != nil {
				t.Fatalf("Open: %v, want nil (malformed frame must not surface a Go error)", err)
			}
			if res.Graph == nil {
				t.Fatal("Graph must be non-nil")
			}
			got := res.Graph.AdjList().HasEdge("alice", "bob")
			if got != tc.applied {
				t.Fatalf("HasEdge(alice,bob) = %v, want %v", got, tc.applied)
			}
			if tc.applied {
				if res.WALOps != 1 {
					t.Fatalf("WALOps = %d, want 1 (well-formed frame applied)", res.WALOps)
				}
				return
			}
			// Malformed frame: nothing applied, and the cut-off is
			// recorded via TailErr so the boundary is deterministic.
			if res.WALOps != 0 {
				t.Fatalf("WALOps = %d, want 0 (malformed frame must not apply)", res.WALOps)
			}
			if res.TailErr == nil {
				t.Fatal("TailErr = nil, want a non-nil cut-off error for the malformed frame")
			}
		})
	}
}

// TestOpen_WeightedV2Frame_TruncatedWeightRejected exercises the
// OpAddEdgeWeighted arm of applyOpCodec on the surviving v2 path: a
// weighted frame whose src and dst decode cleanly but whose trailing
// weight payload is absent. The (non-nil) int64 weight codec then fails
// to decode the missing weight varint, so the op is rejected and not
// applied. This complements the malformed-body battery above by covering
// the weight-decode-failure branch.
func TestOpen_WeightedV2Frame_TruncatedWeightRejected(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	codec := txn.NewStringCodec()
	// src + dst decode cleanly; the weight varint is absent, so the
	// int64 weight codec fails and the weighted op is rejected.
	body, _ := codec.Encode(nil, "alice")
	body, _ = codec.Encode(body, "bob")
	// No weight bytes, no trailing label length.
	payload := append([]byte{txn.OpRecordV2, byte(txn.OpAddEdgeWeighted)}, body...)
	if err := w.Append(payload); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v, want nil", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (weighted frame with truncated weight must not apply)", res.WALOps)
	}
	if res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("weighted frame with truncated weight must not apply alice->bob")
	}
}
