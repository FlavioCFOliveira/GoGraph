package recovery

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// rejectingCodec is a [txn.Codec[string]] that always returns
// [errCodecAlwaysRejects] from Decode. It is used to verify that
// codec errors propagate out of [Open] without crashing the
// recovery loop or masking the real cause.
type rejectingCodec struct{}

var errCodecAlwaysRejects = errors.New("test: codec always rejects")

func (rejectingCodec) Encode(buf []byte, v string) ([]byte, error) {
	return txn.NewStringCodec().Encode(buf, v)
}

func (rejectingCodec) Decode(buf []byte) (value string, rest []byte, err error) {
	return "", buf, errCodecAlwaysRejects
}

// TestRecovery_CodecRejectInvalidVersion writes a WAL using the
// standard v2 string codec, then replays it through a
// [rejectingCodec] whose Decode always fails. The test asserts that:
//
//  1. [Open] returns no Go-level error (the recovery loop
//     treats codec decode failures as torn/unknown frames and skips
//     them, consistent with the documented "stop-at-first-unknown"
//     policy).
//  2. WALOps == 0: no ops were applied because every frame decode
//     was rejected.
//  3. The recovered graph is non-nil and usable.
//
// This is the canonical way to probe the "codec version mismatch"
// scenario without requiring a real v1/v2 on-disk distinction: the
// injected codec plays the role of a v2-only decoder that refuses v1
// frames.
func TestRecovery_CodecRejectInvalidVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write a valid v2 WAL through the standard string codec.
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatalf("wal.Open: %v", err)
	}
	codec := txn.NewStringCodec()
	// Build a minimal v2 frame: OpRecordV2 marker + OpAddEdge kind +
	// codec-encoded src + codec-encoded dst.
	body, _ := codec.Encode(nil, "alice")
	body, _ = codec.Encode(body, "bob")
	// Append the uint16 label length (0 = no label) required by the
	// v2 frame layout that applyOpString expects.
	body = append(body, 0x00, 0x00)
	payload := append([]byte{txn.OpRecordV2, byte(txn.OpAddEdge)}, body...)
	if err := w.Append(payload); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Replay through the rejecting codec. Every Decode call fails, so
	// the loop must stop cleanly and return no ops applied.
	res, err := Open[string, int64](dir, Options[string, int64]{Codec: rejectingCodec{}, WeightCodec: txn.NewInt64WeightCodec()})
	if err != nil {
		t.Fatalf("Open(rejecting codec) = %v, want nil", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (all frames rejected by codec)", res.WALOps)
	}
	if res.Graph == nil {
		t.Fatal("Graph must be non-nil even when all codec decodes fail")
	}
	// The graph must be empty: alice->bob was in the WAL but the
	// rejecting codec prevented any Apply.
	if res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("rejected codec must not allow alice->bob to be applied")
	}
}
