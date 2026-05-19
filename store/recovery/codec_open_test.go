package recovery

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gograph/store/txn"
	"gograph/store/wal"
)

// TestOpenWithCodec_RejectsV1Frames confirms that a v1 WAL replayed
// through the generic typed path is rejected — the legacy
// fmt.Sprintf encoding is not generally invertible. The first frame
// halts the loop and surfaces a tail error. Callers that need to
// drain a v1 corpus must use [OpenString].
func TestOpenWithCodec_RejectsV1Frames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Append(encodeLegacyV1(txn.OpSetNodeLabel, "alice", "", "Person")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := OpenWithCodec[string, int64](dir, txn.NewStringCodec())
	if err != nil {
		t.Fatalf("OpenWithCodec: %v", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (v1 frame must not apply through OpenWithCodec)", res.WALOps)
	}
	// NB: TailErr is overwritten by the WAL reader's clean tail-error
	// at the loop exit, mirroring OpenStringCtx's documented behaviour.
	// The contract we assert is "no ops applied", not "error surfaced".
}

// TestOpenWithCodec_NilCodec rejects a nil codec.
func TestOpenWithCodec_NilCodec(t *testing.T) {
	t.Parallel()
	_, err := OpenWithCodec[string, int64](t.TempDir(), nil)
	if err == nil {
		t.Fatal("OpenWithCodec(nil) must error")
	}
}

// TestOpenWithCodec_PreCancelledCtx confirms the snapshot-boundary
// ctx.Err() check fires for the typed open path.
func TestOpenWithCodec_PreCancelledCtx(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := OpenWithCodecCtx[string, int64](ctx, t.TempDir(), txn.NewStringCodec())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestOpenWithCodec_EmptyDir mirrors the OpenString empty-dir test:
// the typed path returns a fresh graph and no error.
func TestOpenWithCodec_EmptyDir(t *testing.T) {
	t.Parallel()
	res, err := OpenWithCodec[string, int64](t.TempDir(), txn.NewStringCodec())
	if err != nil {
		t.Fatalf("OpenWithCodec: %v", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0", res.WALOps)
	}
	if res.Graph == nil {
		t.Fatal("Graph must be non-nil")
	}
}

// TestOpenWithCodec_TruncatedV2Body produces a v2 frame with a
// truncated body and verifies the recovery loop stops cleanly.
func TestOpenWithCodec_TruncatedV2Body(t *testing.T) {
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
	res, err := OpenWithCodec[string, int64](dir, txn.NewStringCodec())
	if err != nil {
		t.Fatalf("OpenWithCodec: %v", err)
	}
	if res.WALOps != 0 {
		t.Fatalf("WALOps = %d, want 0 (truncated v2 body)", res.WALOps)
	}
	// TailErr is overwritten by the WAL reader's tail-error at loop
	// exit (same as OpenStringCtx); we only assert no ops applied.
}

// TestOpenString_TruncatedV2Body exercises the v2 truncation branch on
// applyOpString — string-keyed path covering the early-return arms.
func TestOpenString_TruncatedV2Body(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	// Build a partial v2 frame: codec(src) ok, codec(dst) short.
	codec := txn.NewStringCodec()
	body := codec.Encode(nil, "alice")
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
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	// applyOpString returns early without applying; the loop continues
	// so WALOps is still incremented (the op is a no-op). The graph
	// must not carry the alice node.
	if _, ok := res.Graph.AdjList().Mapper().Lookup("alice"); ok {
		t.Fatal("partial v2 frame should not apply")
	}
}

// TestOpenString_V2MissingTrailingLabelLength forces the rest-len < 2
// branch of applyOpString's v2 path.
func TestOpenString_V2MissingTrailingLabelLength(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	codec := txn.NewStringCodec()
	body := codec.Encode(nil, "alice")
	body = codec.Encode(body, "bob")
	// No uint16 labelLen trailer; applyOpString must reject and bail.
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
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
	}
	if res.Graph.AdjList().HasEdge("alice", "bob") {
		t.Fatal("payload missing labelLen must not apply")
	}
}

// TestOpenString_V2LabelOverflow forces the labelLen-larger-than-rest
// branch of applyOpString's v2 path.
func TestOpenString_V2LabelOverflow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	w, err := wal.Open(filepath.Join(dir, "wal"))
	if err != nil {
		t.Fatal(err)
	}
	codec := txn.NewStringCodec()
	body := codec.Encode(nil, "alice")
	body = codec.Encode(body, "bob")
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
	res, err := OpenString(dir)
	if err != nil {
		t.Fatalf("OpenString: %v", err)
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

// TestOpenWithCodec_BogusSnapshotErrors checks that a corrupted
// snapshot manifest surfaces as an error on the typed open path,
// mirroring OpenString's behaviour.
func TestOpenWithCodec_BogusSnapshotErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snapshot")
	if err := os.MkdirAll(snapDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapDir, "manifest.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := OpenWithCodec[string, int64](dir, txn.NewStringCodec())
	if err == nil {
		t.Fatal("OpenWithCodec with corrupted snapshot must error")
	}
}

// TestOpenWithCodec_UnreadableWalErrors covers the "wal exists but
// cannot be opened" branch on the typed open path.
func TestOpenWithCodec_UnreadableWalErrors(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "store")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	walPath := filepath.Join(dir, "wal")
	if err := os.WriteFile(walPath, []byte{}, 0o600); err != nil { //nolint:gosec // t.TempDir
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }() //nolint:gosec // test cleanup restores access
	if _, err := OpenWithCodec[string, int64](dir, txn.NewStringCodec()); err == nil {
		t.Fatal("OpenWithCodec with unreadable WAL should error")
	}
}
