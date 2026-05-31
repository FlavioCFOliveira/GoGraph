package packstream_test

import (
	"bytes"
	"errors"
	"testing"

	"gograph/bolt/packstream"
)

// markerTinyList1 is the PackStream TinyList-of-one-element marker (0x9X with
// the low nibble being the element count). 0x91 opens a list that contains
// exactly one further value, so a run of these bytes encodes a value nested
// one level deeper per byte.
const markerTinyList1 = 0x91

// markerNull is the PackStream NULL marker; it terminates a nesting chain with
// a scalar leaf.
const markerNull = 0xC0

// nestedListChain builds a payload of `levels` TinyList-of-1 markers terminated
// by a single NULL leaf. Decoding it recurses exactly `levels` levels deep.
func nestedListChain(levels int) []byte {
	buf := bytes.Repeat([]byte{markerTinyList1}, levels)
	return append(buf, markerNull)
}

// TestReadValueNestingTooDeep is the regression test for security fix C1: a
// payload nested past maxValueDepth must return ErrNestingTooDeep rather than
// recurse without bound into a fatal, unrecoverable stack overflow. Before the
// fix, a payload of ~16M 0x91 bytes (allowed by the 16 MiB Bolt frame cap)
// crashed the whole process during the pre-authentication HELLO decode.
func TestReadValueNestingTooDeep(t *testing.T) {
	// Far past the bound, but tiny on the wire — proves a single short frame
	// suffices. The test completing at all (no fatal stack overflow) is itself
	// part of what is being asserted.
	payload := bytes.Repeat([]byte{markerTinyList1}, 16_000_000)

	dec := packstream.NewDecoder(bytes.NewReader(payload))
	v, err := dec.ReadValue()
	if err == nil {
		t.Fatalf("ReadValue accepted an over-deep payload, want error; got value %v", v)
	}
	if !errors.Is(err, packstream.ErrNestingTooDeep) {
		t.Fatalf("ReadValue error = %v, want ErrNestingTooDeep", err)
	}
}

// TestReadValueNestingAtBound asserts the boundary is inclusive on the safe
// side: a value nested exactly up to the maximum still decodes correctly.
func TestReadValueNestingAtBound(t *testing.T) {
	// A chain whose deepest leaf is read at depth == maxValueDepth (allowed).
	payload := nestedListChain(packstream.MaxValueDepthForTest())

	dec := packstream.NewDecoder(bytes.NewReader(payload))
	v, err := dec.ReadValue()
	if err != nil {
		t.Fatalf("ReadValue rejected a value at the depth bound: %v", err)
	}

	// Walk the decoded structure and confirm it is a chain of single-element
	// lists ending in a NULL leaf.
	depth := 0
	cur := v
	for {
		list, ok := cur.([]packstream.Value)
		if !ok {
			break
		}
		if len(list) != 1 {
			t.Fatalf("at level %d: list len = %d, want 1", depth, len(list))
		}
		cur = list[0]
		depth++
	}
	if cur != nil {
		t.Fatalf("leaf = %v (%T), want nil (NULL)", cur, cur)
	}
	if depth != packstream.MaxValueDepthForTest() {
		t.Fatalf("decoded nesting depth = %d, want %d", depth, packstream.MaxValueDepthForTest())
	}
}

// TestReadValueNestingJustPastBound checks the rejection edge: one level beyond
// the at-bound chain must be refused with ErrNestingTooDeep.
func TestReadValueNestingJustPastBound(t *testing.T) {
	payload := nestedListChain(packstream.MaxValueDepthForTest() + 1)

	dec := packstream.NewDecoder(bytes.NewReader(payload))
	if _, err := dec.ReadValue(); !errors.Is(err, packstream.ErrNestingTooDeep) {
		t.Fatalf("ReadValue error = %v, want ErrNestingTooDeep", err)
	}
}
