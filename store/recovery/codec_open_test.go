package recovery

import (
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/store/txn"
	"github.com/FlavioCFOliveira/GoGraph/store/wal"
)

// TestOpen_RejectsUnknownLeadingByte confirms that a CRC-valid frame whose
// transaction-record version byte is neither the v2 nor the v3 magic tag is
// rejected by the generic typed [Open] path. Such a byte denotes a legacy
// untagged record (or garbage); the fmt.Sprintf-derived endpoints have no
// inverse through a typed codec, so Decode rejects it with
// ErrUnsupportedRecordVersion. Per the fail-stop contract (task #1289) this
// is genuine corruption — not a torn tail — so Open surfaces it as a hard
// error and applies no ops. The frame is hand-built (a leading non-magic
// kind byte followed by a well-formed length-prefixed body) so the
// assertion is independent of any v1 encoder.
func TestOpen_RejectsUnknownLeadingByte(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	// Leading byte = OpSetNodeLabel (a v1 OpKind, not OpRecordV2/V3),
	// followed by srcLen|src, dstLen|dst, labelLen|label.
	frame := []byte{byte(txn.OpSetNodeLabel)}
	frame = binary.LittleEndian.AppendUint16(frame, uint16(len("alice")))
	frame = append(frame, "alice"...)
	frame = binary.LittleEndian.AppendUint16(frame, 0) // empty dst
	frame = binary.LittleEndian.AppendUint16(frame, uint16(len("Person")))
	frame = append(frame, "Person"...)
	if err := w.Append(frame); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err == nil {
		t.Fatal("Open returned nil for an unknown record version; genuine corruption must be surfaced")
	}
	if !errors.Is(err, ErrUnsupportedRecordVersion) {
		t.Fatalf("Open error = %v, want errors.Is(err, ErrUnsupportedRecordVersion)", err)
	}
	if res.IsClean() {
		t.Fatal("Result.IsClean() = true for an unknown record version, want false")
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (legacy-tagged frame must not apply through Open)", res.WALOps)
	}
}

// TestOpen_TruncatedV2Body produces a v2 frame with a truncated body
// and verifies the recovery loop stops cleanly through the typed
// [Open] path.
func TestOpen_TruncatedV2Body(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	// version + kind + truncated string codec length prefix
	payload := []byte{txn.OpRecordV2, byte(txn.OpAddEdge), 0x10, 0x00}
	if err := w.Append(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (truncated v2 body)", res.WALOps)
	}
	// TailErr is overwritten by the WAL reader's tail-error at loop
	// exit; we only assert no ops applied.
}

// TestOpen_TruncatedV2DstBody exercises the dst-decode-failure branch
// on applyOpCodec: codec(src) decodes cleanly but codec(dst) claims a
// length its body cannot satisfy, so codec.Decode returns an error and
// the op is rejected. This complements TestOpen_TruncatedV2Body, which
// fails on the src decode instead. The malformed-frame rejection lives
// on the surviving generic [Open] path (applyOpCodec); the deleted
// OpenString wrapper exercised the equivalent branch in applyOpString.
func TestOpen_TruncatedV2DstBody(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	// Build a partial v2 frame: codec(src) ok, codec(dst) short.
	codec := txn.NewStringCodec()
	body, _ := codec.Encode(nil, "alice")
	body = append(body, 0x10, 0x00, 0x00, 0x00) // dst length 16 but no body
	payload := append([]byte{txn.OpRecordV2, byte(txn.OpAddEdge)}, body...)
	if err := w.Append(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// applyOpCodec returns false on the dst decode failure, so the op is
	// not applied and the graph must not carry the alice node.
	if _, ok := res.Graph.AdjList().Mapper().Lookup("alice"); ok {
		t.Fatal("partial v2 frame should not apply")
	}
}

// TestOpen_V2MissingTrailingLabelLength forces the rest-len < 2 branch
// of applyOpCodec's OpAddEdge arm: src and dst decode cleanly but the
// mandatory trailing uint16 label-length prefix is absent, so the
// frame is treated as corrupt and not applied. This is the surviving
// generic [Open] path; the deleted OpenString wrapper exercised the
// matching branch in applyOpString.
func TestOpen_V2MissingTrailingLabelLength(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	codec := txn.NewStringCodec()
	body, _ := codec.Encode(nil, "alice")
	body, _ = codec.Encode(body, "bob")
	// No uint16 labelLen trailer; applyOpCodec must reject and bail.
	payload := append([]byte{txn.OpRecordV2, byte(txn.OpAddEdge)}, body...)
	if err := w.Append(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("payload missing labelLen must not apply")
	}
}

// TestOpen_V2LabelOverflow forces the labelLen-larger-than-rest branch
// of applyOpCodec's OpAddEdge arm: src and dst decode cleanly but the
// trailing label-length prefix claims more bytes than the frame holds,
// so the frame is rejected as corrupt. This is the surviving generic
// [Open] path; the deleted OpenString wrapper exercised the matching
// branch in applyOpString.
func TestOpen_V2LabelOverflow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	codec := txn.NewStringCodec()
	body, _ := codec.Encode(nil, "alice")
	body, _ = codec.Encode(body, "bob")
	body = binary.LittleEndian.AppendUint16(body, 100) // claim 100 bytes of label
	body = append(body, 'L', 'a', 'b')                 // but only 3 follow
	payload := append([]byte{txn.OpRecordV2, byte(txn.OpAddEdge)}, body...)
	if err := w.Append(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := Open[string, int64](dir, Options[string, int64]{
		Codec:       txn.NewStringCodec(),
		WeightCodec: txn.NewInt64WeightCodec(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("payload with overflowing labelLen must not apply")
	}
}

// TestDecode_ShortV2Payload ensures decodeV2 rejects the empty case
// — payload that is only a magic byte without a kind byte.
func TestDecode_ShortV2Payload(t *testing.T) {
	t.Parallel()
	if _, err := Decode([]byte{txn.OpRecordV2}); err == nil {
		t.Fatal("Decode([0xFE]) returned no error")
	}
}
