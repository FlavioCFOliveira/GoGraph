package hash

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
)

// TestIndexes_HashSerializeRoundtrip seeds an Index[string] with a
// known population, round-trips it through Serialize/Deserialize and
// asserts that every per-value cardinality, membership and the
// global DistinctValues count match the original.
func TestIndexes_HashSerializeRoundtrip(t *testing.T) {
	t.Parallel()
	src := New[string]()
	keys := []string{"alpha", "bravo", "charlie", "delta", "echo"}
	for ki, k := range keys {
		for n := uint64(0); n < 16; n++ {
			if (n+uint64(ki))%2 == 0 {
				src.Insert(k, graph.NodeID(n))
			}
		}
	}

	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}

	dst := New[string]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}

	if src.DistinctValues() != dst.DistinctValues() {
		t.Fatalf("DistinctValues src=%d dst=%d",
			src.DistinctValues(), dst.DistinctValues())
	}
	for _, k := range keys {
		if a, b := src.Cardinality(k), dst.Cardinality(k); a != b {
			t.Fatalf("Cardinality(%q) src=%d dst=%d", k, a, b)
		}
		for n := uint64(0); n < 16; n++ {
			if a, b := src.Contains(k, graph.NodeID(n)), dst.Contains(k, graph.NodeID(n)); a != b {
				t.Fatalf("Contains(%q,%d) src=%v dst=%v", k, n, a, b)
			}
		}
	}
}

// TestIndexes_HashSerializeEmpty asserts the empty index roundtrips
// cleanly.
func TestIndexes_HashSerializeEmpty(t *testing.T) {
	t.Parallel()
	src := New[string]()
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize empty: %v", err)
	}
	dst := New[string]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize empty: %v", err)
	}
	if dst.DistinctValues() != 0 {
		t.Fatalf("DistinctValues after empty roundtrip = %d, want 0",
			dst.DistinctValues())
	}
}

// TestIndexes_HashCorruptedCRC asserts CRC mismatch surfaces as
// [index.ErrIndexCorrupted].
func TestIndexes_HashCorruptedCRC(t *testing.T) {
	t.Parallel()
	src := New[string]()
	src.Insert("k", graph.NodeID(1))
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	corrupt := buf.Bytes()
	corrupt[len(corrupt)-1] ^= 0xFF
	dst := New[string]()
	err := dst.Deserialize(bytes.NewReader(corrupt))
	if !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("CRC tamper = %v, want ErrIndexCorrupted", err)
	}
}

// TestIndexes_HashKindAndApply locks the subscriber identifier and
// confirms Apply is a documented no-op for the generic index.
func TestIndexes_HashKindAndApply(t *testing.T) {
	t.Parallel()
	if got := New[string]().Kind(); got != "hash" {
		t.Fatalf("Kind = %q, want \"hash\"", got)
	}
	idx := New[string]()
	idx.Apply(index.Change{Op: index.OpSetNodeProperty, Node: 1, NewValue: "x"})
	if idx.DistinctValues() != 0 {
		t.Fatalf("Apply must be a no-op for the generic hash index; got %d distinct values",
			idx.DistinctValues())
	}
}

// TestIndexes_HashInt64Roundtrip exercises the encodeValue / decodeValue
// path for a non-string V.
func TestIndexes_HashInt64Roundtrip(t *testing.T) {
	t.Parallel()
	src := New[int64]()
	values := []int64{-7, 0, 1, 2, 9223372036854775807}
	for vi, v := range values {
		src.Insert(v, graph.NodeID(uint64(vi)))
	}
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize int64: %v", err)
	}
	dst := New[int64]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize int64: %v", err)
	}
	for vi, v := range values {
		if !dst.Contains(v, graph.NodeID(uint64(vi))) {
			t.Fatalf("Contains(%d, %d) = false after roundtrip", v, vi)
		}
	}
}

// TestIndexes_HashUnsupportedV asserts that serialising an
// instantiation with an unsupported V surfaces
// [index.ErrIndexValueTypeUnsupported].
func TestIndexes_HashUnsupportedV(t *testing.T) {
	t.Parallel()
	type custom struct {
		A, B int
	}
	src := New[custom]()
	src.Insert(custom{A: 1, B: 2}, graph.NodeID(1))
	var buf bytes.Buffer
	err := src.Serialize(&buf)
	if !errors.Is(err, index.ErrIndexValueTypeUnsupported) {
		t.Fatalf("Serialize custom struct = %v, want ErrIndexValueTypeUnsupported", err)
	}
}

// hashRoundtrip exercises a single (V, value) pair through the
// Serialize/Deserialize pair and asserts membership. Generic over V
// so each kind-specific subtest stays a one-liner.
func hashRoundtrip[V comparable](t *testing.T, v V) {
	t.Helper()
	src := New[V]()
	src.Insert(v, graph.NodeID(1))
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	dst := New[V]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if !dst.Contains(v, graph.NodeID(1)) {
		t.Fatalf("roundtrip lost entry for value %v", v)
	}
}

// TestIndexes_HashAllSupportedTypesRoundtrip exercises every
// supported V kind end-to-end so the type switch in encodeValue /
// decodeValue stays covered.
func TestIndexes_HashAllSupportedTypesRoundtrip(t *testing.T) {
	t.Parallel()
	t.Run("int32", func(t *testing.T) { hashRoundtrip[int32](t, -7) })
	t.Run("uint32", func(t *testing.T) { hashRoundtrip[uint32](t, 7) })
	t.Run("uint64", func(t *testing.T) { hashRoundtrip[uint64](t, 7) })
	t.Run("float64", func(t *testing.T) { hashRoundtrip[float64](t, 3.14) })
	t.Run("bool-true", func(t *testing.T) { hashRoundtrip[bool](t, true) })
	t.Run("bool-false", func(t *testing.T) { hashRoundtrip[bool](t, false) })
}

// TestIndexes_HashDeserializeShortPayload exercises the short-buffer
// branches: each kind decoder rejects a payload whose length does not
// match the expected fixed width.
func TestIndexes_HashDeserializeShortPayload(t *testing.T) {
	t.Parallel()
	// Build a known-good serialised int64 payload then strip the
	// trailing CRC and a key byte so the inner per-entry decode hits
	// the short-buffer branch.
	src := New[int64]()
	src.Insert(int64(1), graph.NodeID(1))
	var buf bytes.Buffer
	if err := src.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	// Truncate to half the body to ensure the inner loop reads short.
	truncated := buf.Bytes()[:len(buf.Bytes())/2]
	dst := New[int64]()
	if err := dst.Deserialize(bytes.NewReader(truncated)); !errors.Is(err, index.ErrIndexCorrupted) {
		t.Fatalf("truncated payload = %v, want ErrIndexCorrupted", err)
	}
}

// TestSerializeWireFormatIsPinned holds the on-disk format byte for byte across
// round three of rmp #2692.
//
// Round three split where a posting list's ids come FROM: an empty or singleton
// key is serialized out of [entry.meta] and everything wider out of a published
// [snapshot]. The format itself must not have moved an inch — a stored index is
// read back by [Index.Deserialize] from a previous build, and
// cypher/index_hydration.go hydrates engines from images written earlier — and a
// round-trip test cannot see a format change, because it writes and reads with
// the same code.
//
// So the expected digest is not this build's own output. It was measured on the
// ROUND-TWO build, whose serializer read every tier out of a snapshot, with
// byte-identical probe code, and pasted here. A build that hashes differently
// has changed the format, whatever its round trips say.
//
// The fixture deliberately includes keys that arrive at the singleton tier by
// DEMOTION as well as directly, because those are exactly the keys whose ids
// now come from a different place than they did.
func TestSerializeWireFormatIsPinned(t *testing.T) {
	t.Parallel()

	// Measured on the round-two build (rmp #2692), probe code identical to the
	// fixture below.
	const (
		wantLen    = 3292
		wantDigest = "f85eddc732eb9651ec61abeedf5d26af5ec335890d03aea590dbde2835d0adc3"
	)

	idx := New[int64]()
	// Every width from 1 to 13: the singleton, small and bitmap tiers.
	for v := int64(0); v < 40; v++ {
		width := int(v)%13 + 1
		for n := range width {
			idx.Insert(v, graph.NodeID(uint64(v)*100+uint64(n)+1))
		}
	}
	// Keys that reach the singleton tier by demotion rather than directly.
	for v := int64(100); v < 110; v++ {
		for n := range 20 {
			idx.Insert(v, graph.NodeID(uint64(v)*100+uint64(n)+1))
		}
		for n := 1; n < 20; n++ {
			idx.Delete(v, graph.NodeID(uint64(v)*100+uint64(n)+1))
		}
	}
	var buf bytes.Buffer
	if err := idx.Serialize(&buf); err != nil {
		t.Fatalf("Serialize: %v", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	got := hex.EncodeToString(sum[:])
	if buf.Len() != wantLen || got != wantDigest {
		t.Errorf("the serialized image is %d bytes, sha256 %s; want %d bytes, "+
			"sha256 %s: the on-disk format has changed, so an index written by an "+
			"earlier build is no longer the image this one writes",
			buf.Len(), got, wantLen, wantDigest)
	}
	// A digest is only worth pinning if it is the digest of something valid.
	dst := New[int64]()
	if err := dst.Deserialize(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if got, want := dst.DistinctValues(), idx.DistinctValues(); got != want {
		t.Fatalf("DistinctValues after the round trip = %d, want %d", got, want)
	}
}
