package txn

// Encoder-level companion to wal_length_prefix_bound_test.go (rmp #2742).
//
// The external test drives the public API and settles the question that
// matters — an over-long field must not produce an acknowledged commit. This
// file pins the guard at the level it actually lives: every op-body encoder
// that writes a length prefix must fail stop before writing it, for every op
// kind that routes through it, including the kinds no Tx method emits today.
//
// It also covers the uint32 bound, which cannot be reached end-to-end: a
// property value would have to be 4 GiB. The predicate is tested directly
// instead, and the report says plainly that the wiring of the uint32 guard into
// the encoders is established by construction and inspection, not by a
// round trip.

import (
	"errors"
	"math"
	"math/bits"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// encoderGuardCase is one op kind whose body encoder length-prefixes a string
// with a uint16.
type encoderGuardCase struct {
	name string
	// build returns the op carrying s in the field under test.
	build func(s string) Op[string, int64]
	// encoder names the function the op dispatches to, for the failure message.
	encoder string
}

func encoderGuardCases() []encoderGuardCase {
	return []encoderGuardCase{
		{"OpRemoveNodeLabel", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpRemoveNodeLabel, Src: "a", Label: s}
		}, "encodeOpNodeOnly"},
		{"OpSetNodeProperty", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpSetNodeProperty, Src: "a", Key: s, Value: lpg.StringValue("v")}
		}, "encodeOpNodeProperty"},
		{"OpDelNodeProperty", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpDelNodeProperty, Src: "a", Key: s}
		}, "encodeOpNodeProperty"},
		{"OpSetEdgeProperty", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpSetEdgeProperty, Src: "a", Dst: "b", Key: s, Value: lpg.StringValue("v")}
		}, "encodeOpEdgeProperty"},
		{"OpSetEdgePropertyByHandle", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpSetEdgePropertyByHandle, Src: "a", Dst: "b", Handle: 3, Key: s, Value: lpg.StringValue("v")}
		}, "encodeOpEdgeProperty"},
		{"OpDelEdgePropertyByHandle", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpDelEdgePropertyByHandle, Src: "a", Dst: "b", Handle: 3, Key: s}
		}, "encodeOpEdgeProperty"},
		// OpAddEdgeWeighted and OpAddEdgeH route through encodeOpEdgeWeighted.
		// No Tx method sets Label on either kind, so this is the one guard the
		// public-API battery cannot reach; it is covered here because the wire
		// kind is still decoded by recovery.
		{"OpAddEdgeWeighted", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpAddEdgeWeighted, Src: "a", Dst: "b", Weight: 1, Label: s}
		}, "encodeOpEdgeWeighted"},
		{"OpAddEdgeH", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpAddEdgeH, Src: "a", Dst: "b", Weight: 1, Handle: 9, Label: s}
		}, "encodeOpEdgeWeighted"},
		{"OpSetNodeLabel", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpSetNodeLabel, Src: "a", Label: s}
		}, "encodeOpEdgeWithLabel"},
		{"OpSetEdgeLabel", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpSetEdgeLabel, Src: "a", Dst: "b", Label: s}
		}, "encodeOpEdgeWithLabel"},
		{"OpAddEdge", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpAddEdge, Src: "a", Dst: "b", Label: s}
		}, "encodeOpEdgeWithLabel"},
		{"OpSetEdgeLabelByHandle", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpSetEdgeLabelByHandle, Src: "a", Dst: "b", Handle: 4, Label: s}
		}, "encodeOpEdgeWithLabel"},
		// The two pre-existing guards (#1903). They are here so a future edit
		// to the shared helper cannot regress them unnoticed.
		{"OpCreateConstraint", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpCreateConstraint, Label: s, Key: "p", ConstraintName: "c"}
		}, "appendOpConstraintBody"},
		{"OpCreateIndex", func(s string) Op[string, int64] {
			return Op[string, int64]{Kind: OpCreateIndex, Label: "L", Key: "p", ConstraintName: s}
		}, "appendOpIndexBody"},
	}
}

// TestEncodeOpRefusesOversizeUint16Field_2742 asserts every op-body encoder
// that writes a uint16 length prefix refuses a string one byte past its
// capacity, with a typed error, and accepts one exactly at capacity.
func TestEncodeOpRefusesOversizeUint16Field_2742(t *testing.T) {
	t.Parallel()

	codec := NewStringCodec()
	wcodec := NewInt64WeightCodec()

	refused, accepted := 0, 0
	for _, c := range encoderGuardCases() {
		t.Run(c.name, func(t *testing.T) {
			over := strings.Repeat("o", maxWALSchemaStringLen+1)
			buf, err := encodeOpTypedV3(c.build(over), 1, codec, wcodec)
			if err == nil {
				t.Fatalf("%s (%s): a %d-byte field encoded without error; "+
					"the uint16 prefix wrapped to %d and the frame is %d bytes",
					c.name, c.encoder, len(over), len(over)%(maxWALSchemaStringLen+1), len(buf))
			}
			if !errors.Is(err, ErrFieldTooLong) {
				t.Fatalf("%s: error %v does not wrap ErrFieldTooLong", c.name, err)
			}
			if buf != nil {
				t.Fatalf("%s: refused encode still returned %d bytes; the caller could append them", c.name, len(buf))
			}
			refused++

			atCap := strings.Repeat("c", maxWALSchemaStringLen)
			if _, err := encodeOpTypedV3(c.build(atCap), 1, codec, wcodec); err != nil {
				t.Fatalf("%s: over-restricted — a %d-byte field (the exact uint16 capacity) was refused: %v",
					c.name, maxWALSchemaStringLen, err)
			}
			accepted++
		})
	}

	if want := len(encoderGuardCases()); refused != want || accepted != want {
		t.Fatalf("vacuous run: %d refusals and %d acceptances observed, want %d of each", refused, accepted, want)
	}
}

// TestCheckWALValueLen_2742 pins the uint32 predicate that bounds a property
// value, a list element payload, and a list element count.
//
// It exercises the predicate rather than the encoder because reaching the
// encoder's uint32 prefix requires a 4 GiB value. What this test establishes is
// that the bound is at MaxUint32 and not one byte either side of it; that the
// encoders call it is established by construction — there are exactly four
// uint32 prefixes in the property encoders and each is preceded by its call.
func TestCheckWALValueLen_2742(t *testing.T) {
	t.Parallel()

	if err := checkWALValueLen("probe", 0); err != nil {
		t.Fatalf("checkWALValueLen(0) = %v, want nil", err)
	}
	if err := checkWALValueLen("probe", maxWALValueLenInt); err != nil {
		t.Fatalf("over-restricted: checkWALValueLen(%d) = %v, want nil", maxWALValueLenInt, err)
	}

	if maxWALValueLenInt == math.MaxInt {
		// 32-bit platform: the clamped bound IS the largest representable
		// length, so no slice can exceed it and the guard is unreachable. There
		// is nothing to observe, and nothing that could truncate.
		t.Logf("platform int is %d bits: no length can exceed the uint32 prefix, guard unreachable", bits.UintSize)
		return
	}
	err := checkWALValueLen("probe", maxWALValueLenInt+1)
	if err == nil {
		t.Fatalf("checkWALValueLen(%d) = nil, want a refusal", maxWALValueLenInt+1)
	}
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("checkWALValueLen(%d) error %v does not wrap ErrFieldTooLong", maxWALValueLenInt+1, err)
	}
}

// TestCheckWALSchemaString_2742 pins the uint16 predicate at its exact
// boundary, so a later edit cannot tighten or loosen it by one byte unnoticed.
func TestCheckWALSchemaString_2742(t *testing.T) {
	t.Parallel()

	if maxWALSchemaStringLen != 1<<16-1 {
		t.Fatalf("maxWALSchemaStringLen = %d, want 65535 (the uint16 prefix capacity)", maxWALSchemaStringLen)
	}
	if err := checkWALSchemaString("probe", strings.Repeat("x", maxWALSchemaStringLen)); err != nil {
		t.Fatalf("over-restricted: a %d-byte string was refused: %v", maxWALSchemaStringLen, err)
	}
	err := checkWALSchemaString("probe", strings.Repeat("x", maxWALSchemaStringLen+1))
	if err == nil {
		t.Fatal("a 65536-byte string was accepted; the uint16 prefix would wrap to 0")
	}
	if !errors.Is(err, ErrFieldTooLong) {
		t.Fatalf("error %v does not wrap ErrFieldTooLong", err)
	}
	if got, want := err.Error(), "probe is 65536 bytes, maximum 65535"; !strings.Contains(got, want) {
		t.Fatalf("refusal message %q does not name the field and both lengths (want it to contain %q)", got, want)
	}
}
