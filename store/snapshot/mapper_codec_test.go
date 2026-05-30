package snapshot

import (
	"bytes"
	"testing"

	"gograph/graph"
	"gograph/store/txn"
)

// TestWriteMapper_StringByteIdenticalToV1 is the AC#3 byte-identity
// guard: WriteMapper for a string-keyed mapper must produce exactly the
// same bytes (and CRC) as the frozen v1 WriteMapperString writer,
// regardless of which string codec is supplied. A drift here would
// break the cross-process byte-equality guarantee and silently
// invalidate every string snapshot cache produced before the codec
// generalisation.
func TestWriteMapper_StringByteIdenticalToV1(t *testing.T) {
	t.Parallel()
	m := graph.NewMapper[string]()
	for _, k := range []string{"alice", "bob", "carol", "", "dave", "eve"} {
		m.Intern(k)
	}

	var v1 bytes.Buffer
	v1Size, v1CRC, err := WriteMapperString(&v1, m)
	if err != nil {
		t.Fatalf("WriteMapperString: %v", err)
	}

	var codecBuf bytes.Buffer
	cSize, cCRC, err := WriteMapper[string](&codecBuf, m, txn.NewStringCodec())
	if err != nil {
		t.Fatalf("WriteMapper[string]: %v", err)
	}

	if cSize != v1Size {
		t.Errorf("size: WriteMapper=%d WriteMapperString=%d", cSize, v1Size)
	}
	if cCRC != v1CRC {
		t.Errorf("crc: WriteMapper=%d WriteMapperString=%d", cCRC, v1CRC)
	}
	if !bytes.Equal(codecBuf.Bytes(), v1.Bytes()) {
		t.Fatalf("string mapper bytes drifted: WriteMapper produced %d bytes, WriteMapperString %d bytes",
			codecBuf.Len(), v1.Len())
	}

	// The version byte on disk must still be 1 for the string layout.
	ver, _ := decodeMapperHeaderVersion(t, codecBuf.Bytes())
	if ver != mapperFormatVersionString {
		t.Errorf("string mapper version = %d, want %d (v1)", ver, mapperFormatVersionString)
	}
}

// TestWriteMapper_CodecVersionIsTwo confirms a non-string mapper is
// stamped with the codec format version (2), distinguishing it on disk
// from the string (v1) layout so the loader picks the right reader.
func TestWriteMapper_CodecVersionIsTwo(t *testing.T) {
	t.Parallel()
	m := graph.NewMapper[int64]()
	for _, k := range []int64{10, 20, 30} {
		m.Intern(k)
	}
	var buf bytes.Buffer
	if _, _, err := WriteMapper[int64](&buf, m, txn.NewInt64Codec()); err != nil {
		t.Fatalf("WriteMapper[int64]: %v", err)
	}
	ver, _ := decodeMapperHeaderVersion(t, buf.Bytes())
	if ver != mapperFormatVersionCodec {
		t.Errorf("int64 mapper version = %d, want %d (v2)", ver, mapperFormatVersionCodec)
	}
}

// mapperRoundTripCase pairs a Mapper builder with the codec that
// encodes its keys, so a single table can exercise every supported key
// type through the WriteMapper -> ReadMapperBytes -> decode path.
type mapperRoundTripCase struct {
	name string
	// run interns a deterministic key set for one key type, serialises
	// it via WriteMapper, reads it back, and asserts the decoded pairs
	// match the originals (see roundTripKeys).
	run func(t *testing.T)
}

// TestMapperCodec_RoundTrip is the AC#2 matrix: mapper.bin v2 must
// round-trip for string, int, int64, uint64, and [16]byte codecs. For
// each key type the test interns a deterministic key set, serialises it
// via WriteMapper, reads it back via ReadMapperBytes, decodes every raw
// record through the matching codec, and asserts the decoded
// (NodeID -> key) pairs equal the originals.
func TestMapperCodec_RoundTrip(t *testing.T) {
	t.Parallel()
	cases := []mapperRoundTripCase{
		{name: "string", run: func(t *testing.T) {
			roundTripKeys(t, txn.NewStringCodec(),
				[]string{"alpha", "beta", "gamma", "", "delta"})
		}},
		{name: "int", run: func(t *testing.T) {
			roundTripKeys(t, txn.NewIntCodec(),
				[]int{-3, -1, 0, 7, 42, 1 << 20})
		}},
		{name: "int64", run: func(t *testing.T) {
			roundTripKeys(t, txn.NewInt64Codec(),
				[]int64{-(1 << 40), -1, 0, 1, 1 << 50})
		}},
		{name: "uint64", run: func(t *testing.T) {
			roundTripKeys(t, txn.NewUint64Codec(),
				[]uint64{0, 1, 255, 1 << 32, ^uint64(0)})
		}},
		{name: "uuid16", run: func(t *testing.T) {
			roundTripKeys(t, txn.NewUUIDCodec(), [][16]byte{
				{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
					0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00},
				{}, // zero UUID
				{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
					0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

// roundTripKeys interns keys into a fresh Mapper[N], serialises via
// WriteMapper, then reads the payload back through the SAME reader the
// loader would pick for the on-disk version and asserts every decoded
// (NodeID, key) matches the original Intern result.
//
// String keys land in the v1 layout (Pairs, raw UTF-8 — byte-identical
// to the pre-codec writer), so they are read via ReadMapperString and
// compared as strings. Every other key type lands in the v2 layout
// (RawPairs, codec-encoded bytes) and is decoded through codec, exactly
// as ApplyMapperToGraphWithCodec does during recovery.
func roundTripKeys[N comparable](t *testing.T, codec txn.Codec[N], keys []N) {
	t.Helper()
	m := graph.NewMapper[N]()
	want := make(map[graph.NodeID]N, len(keys))
	for _, k := range keys {
		id := m.Intern(k)
		want[id] = k
	}

	var buf bytes.Buffer
	size, crc, err := WriteMapper[N](&buf, m, codec)
	if err != nil {
		t.Fatalf("WriteMapper: %v", err)
	}
	if int64(buf.Len()) != size {
		t.Fatalf("size = %d, buf.Len = %d", size, buf.Len())
	}
	if len(keys) > 0 && crc == 0 {
		t.Fatal("CRC must be non-zero on a non-empty mapper")
	}

	ver, magicOK := decodeMapperHeaderVersion(t, buf.Bytes())
	if !magicOK {
		t.Fatal("mapper payload has bad magic")
	}

	// String layout (v1): read via ReadMapperString, compare as strings.
	if ver == mapperFormatVersionString {
		rb, rerr := ReadMapperString(&buf)
		if rerr != nil {
			t.Fatalf("ReadMapperString: %v", rerr)
		}
		if len(rb.Pairs) != len(keys) {
			t.Fatalf("Pairs len = %d, want %d", len(rb.Pairs), len(keys))
		}
		for _, p := range rb.Pairs {
			wantKey, ok := want[p.ID]
			if !ok {
				t.Errorf("readback node %d not in original mapper", uint64(p.ID))
				continue
			}
			// N is string here; compare via any() to keep the helper generic.
			if any(p.Key) != any(wantKey) {
				t.Errorf("node %d: key %q, want %v", uint64(p.ID), p.Key, wantKey)
			}
		}
		return
	}

	// Codec layout (v2): read raw bytes and decode through codec.
	rb, rerr := ReadMapperBytes(&buf)
	if rerr != nil {
		t.Fatalf("ReadMapperBytes: %v", rerr)
	}
	if len(rb.RawPairs) != len(keys) {
		t.Fatalf("RawPairs len = %d, want %d", len(rb.RawPairs), len(keys))
	}
	seen := make(map[graph.NodeID]bool, len(keys))
	for _, rp := range rb.RawPairs {
		got, rest, derr := codec.Decode(rp.Key)
		if derr != nil {
			t.Fatalf("decode key for node %d: %v", uint64(rp.ID), derr)
		}
		if len(rest) != 0 {
			t.Errorf("node %d: %d trailing bytes after decode", uint64(rp.ID), len(rest))
		}
		wantKey, ok := want[rp.ID]
		if !ok {
			t.Errorf("readback node %d not in original mapper", uint64(rp.ID))
			continue
		}
		if got != wantKey {
			t.Errorf("node %d: decoded key %v, want %v", uint64(rp.ID), got, wantKey)
		}
		seen[rp.ID] = true
	}
	if len(seen) != len(want) {
		t.Errorf("round-trip covered %d ids, want %d", len(seen), len(want))
	}
}

// decodeMapperHeaderVersion reads the magic + version prefix from a
// mapper.bin byte slice for the version assertions above.
func decodeMapperHeaderVersion(t *testing.T, b []byte) (version uint16, magicOK bool) {
	t.Helper()
	if len(b) < 6 {
		t.Fatalf("mapper payload too short: %d bytes", len(b))
	}
	magic := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
	version = uint16(b[4]) | uint16(b[5])<<8
	return version, magic == mapperMagic
}
