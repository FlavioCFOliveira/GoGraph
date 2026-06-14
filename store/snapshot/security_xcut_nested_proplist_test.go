package snapshot

// Cross-cutting security regression for SECURITY-GAP #1488 (SEC-2026-06-14b).
//
// Finding: the snapshot property-value decoder had NO recursion-depth bound on
// nested PropList values. The ENCODER explicitly rejects nested lists
// (encodeListPropertyValue: "snapshot: nested PropList not supported"), so a
// legitimately written snapshot never contains a list nested even one level
// deep. The DECODER did not enforce that invariant: decodeListPropertyValue
// read each element's kind tag from the (untrusted) wire and, when the tag was
// PropList (7), recursed via decodePropertyValue -> decodeListPropertyValue
// with no depth counter. A crafted properties.bin / edgehandles.bin could
// declare a list whose single element is itself a list, repeated to a depth
// bounded only by len(raw)/listElemMinBytes (5), driving unbounded Go-stack
// growth toward the 1 GiB goroutine-stack ceiling where the runtime aborts the
// process (CWE-674 uncontrolled recursion -> DoS). There is no recover() on the
// library path, so the fatal crashes the embedding application.
//
// THE FIX: decodeListPropertyValue now enforces the encoder's invariant — a
// PropList element kind is rejected with ErrPropertiesCorrupted before any
// recursion. Because both loader paths reach the recursive core through
// decodePropertyValue -> decodeListPropertyValue (properties.bin at APPLY time
// via ApplyPropertiesToGraph; edgehandles.bin at PARSE time via
// readEdgeHandleProp -> decodePropertyValue), the single guard closes both. A
// top-level PropList property remains valid; only NESTING is rejected.

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// secXcutNestedListBytes returns the PropList wire payload for a list nested
// `depth` levels deep. Each level is exactly listElemMinBytes(=5) header bytes
// plus the inner payload:
//
//	uint32 count=1 | uint8 kind=PropList | uint32 elemLen | <inner payload>
//
// The innermost level is an empty list (count=0, 4 bytes). depth 0 is a single
// empty list (no nesting); depth >= 1 is, by construction, hostile because the
// encoder forbids any nesting.
func secXcutNestedListBytes(depth int) []byte {
	var payload []byte
	payload = binary.LittleEndian.AppendUint32(payload, 0) // innermost empty list (zero count)
	for i := 0; i < depth; i++ {
		var lvl []byte
		lvl = binary.LittleEndian.AppendUint32(lvl, 1)                    // single-element count
		lvl = append(lvl, byte(lpg.PropList))                             // element kind = PropList
		lvl = binary.LittleEndian.AppendUint32(lvl, uint32(len(payload))) // elemLen
		lvl = append(lvl, payload...)
		payload = lvl
	}
	return payload
}

// TestSec_Snapshot_NestedPropListRejected is the strict regression gate for
// #1488: the decoder must reject a PropList that contains a nested PropList
// element — unconditionally, with ErrPropertiesCorrupted — rather than recurse
// without a depth bound. A legitimately encoded snapshot never nests lists (the
// encoder forbids it), so any positive nesting depth is hostile by construction.
// The hostile depth here is small enough to decode in-process without risking
// the stack ceiling; the point is the FIRST nested element is rejected.
func TestSec_Snapshot_NestedPropListRejected(t *testing.T) {
	t.Parallel()
	const hostileDepth = 4096
	raw := secXcutNestedListBytes(hostileDepth)

	if _, err := decodeListPropertyValue(raw); err == nil {
		t.Fatalf("decodeListPropertyValue accepted a PropList nested %d levels deep; "+
			"the no-nesting guard is missing or ineffective (CWE-674, #1488)", hostileDepth)
	} else if !errors.Is(err, ErrPropertiesCorrupted) {
		t.Fatalf("decodeListPropertyValue err = %v; want wrapped ErrPropertiesCorrupted", err)
	}
}

// TestSec_Snapshot_NestedPropListRejectedAtEveryDepth pins the secure contract
// across the boundary: depth 0 is a single empty list with NO nested element
// and must decode (a top-level list is legitimate); depth >= 1 contains at least
// one nested PropList element and must be rejected. This guards both directions:
// the guard must not over-reach (depth 0 still loads) and must not under-reach
// (the very first nested element trips it).
func TestSec_Snapshot_NestedPropListRejectedAtEveryDepth(t *testing.T) {
	t.Parallel()
	// depth 0: a valid, non-nested empty list — must decode.
	if v, err := decodeListPropertyValue(secXcutNestedListBytes(0)); err != nil {
		t.Fatalf("depth=0 (non-nested empty list) rejected: %v; the guard over-reaches", err)
	} else if v.Kind() != lpg.PropList {
		t.Fatalf("depth=0: expected PropList, got kind %d", v.Kind())
	}
	// depth >= 1: every payload carries at least one nested PropList element and
	// must be rejected at the first nested element.
	for _, depth := range []int{1, 2, 64, 1024} {
		raw := secXcutNestedListBytes(depth)
		if _, err := decodeListPropertyValue(raw); err == nil {
			t.Fatalf("depth=%d: nested PropList accepted; want rejection (#1488)", depth)
		} else if !errors.Is(err, ErrPropertiesCorrupted) {
			t.Fatalf("depth=%d: err = %v; want wrapped ErrPropertiesCorrupted", depth, err)
		}
	}
}
