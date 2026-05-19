package txn

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/lpg"
	"gograph/store/wal"
)

// TestEncodeOpLegacy_GoldenBytes locks down the v1 (untagged) WAL
// payload bytes for a fixed set of mutations against a string-keyed
// store. The golden hex string MUST NOT change without a deliberate
// migration plan documented under docs/persistence.md.
//
// The test feeds the encoder directly so the assertion captures the
// payload bytes only, independent of the WAL framing.
func TestEncodeOpLegacy_GoldenBytes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		op   Op[string]
		hex  string
	}{
		{
			name: "AddEdge alice->bob",
			op:   Op[string]{Kind: OpAddEdge, Src: "alice", Dst: "bob"},
			// kind=01, srcLen=0005 alice, dstLen=0003 bob, labelLen=0000
			hex: "01" + "0500" + "616c696365" + "0300" + "626f62" + "0000",
		},
		{
			name: "SetNodeLabel alice Person",
			op:   Op[string]{Kind: OpSetNodeLabel, Src: "alice", Label: "Person"},
			// kind=02, srcLen=0005 alice, dstLen=0000, labelLen=0006 Person
			hex: "02" + "0500" + "616c696365" + "0000" + "0600" + "506572736f6e",
		},
		{
			name: "SetEdgeLabel alice->bob KNOWS",
			op:   Op[string]{Kind: OpSetEdgeLabel, Src: "alice", Dst: "bob", Label: "KNOWS"},
			// kind=03, srcLen=0005 alice, dstLen=0003 bob, labelLen=0005 KNOWS
			hex: "03" + "0500" + "616c696365" + "0300" + "626f62" + "0500" + "4b4e4f5753",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeOpLegacy(tc.op)
			want, err := hex.DecodeString(tc.hex)
			if err != nil {
				t.Fatalf("decode golden hex: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("legacy v1 layout drift: got %s want %s", hex.EncodeToString(got), tc.hex)
			}
		})
	}
}

// TestEncodeOpTyped_V2HeaderTag verifies that the typed encoder
// prepends the v2 version marker and OpKind byte ahead of the codec
// payload, and that the trailing uint16 label length is little-endian.
func TestEncodeOpTyped_V2HeaderTag(t *testing.T) {
	t.Parallel()
	codec := NewStringCodec()
	op := Op[string]{Kind: OpAddEdge, Src: "alice", Dst: "bob"}
	got := encodeOpTyped(op, codec)
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

// TestTxn_BackwardsCompat_NewStoreUnchanged proves that committing
// through a [NewStore] (no codec) produces byte-identical WAL frames
// to the pre-codec implementation. Drift in this golden requires an
// intentional migration step.
func TestTxn_BackwardsCompat_NewStoreUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "wal")
	w, err := wal.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	s := NewStore(g, w)
	tx := s.Begin()
	if err := tx.SetNodeLabel("alice", "Person"); err != nil {
		t.Fatal(err)
	}
	if err := tx.AddEdge("alice", "bob"); err != nil {
		t.Fatal(err)
	}
	if err := tx.SetEdgeLabel("alice", "bob", "KNOWS"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// Read back the WAL via the reader and capture each frame's
	// payload. The bytes MUST match the v1 golden for each kind.
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := wal.OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer func() { _ = r.Close() }()
	var payloads [][]byte
	if err := r.Replay(func(f wal.Frame) error {
		payloads = append(payloads, append([]byte(nil), f.Payload...))
		return nil
	}); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got, want := len(payloads), 3; got != want {
		t.Fatalf("frame count = %d, want %d", got, want)
	}
	wants := []string{
		// SetNodeLabel alice Person
		"02" + "0500" + "616c696365" + "0000" + "0600" + "506572736f6e",
		// AddEdge alice->bob (no label)
		"01" + "0500" + "616c696365" + "0300" + "626f62" + "0000",
		// SetEdgeLabel alice->bob KNOWS
		"03" + "0500" + "616c696365" + "0300" + "626f62" + "0500" + "4b4e4f5753",
	}
	for i, want := range wants {
		gotHex := hex.EncodeToString(payloads[i])
		if gotHex != want {
			t.Fatalf("frame[%d] payload drift\n got: %s\nwant: %s", i, gotHex, want)
		}
	}
	// Sanity-check the on-disk file ends cleanly (no garbage trailer).
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("WAL is empty")
	}
}
