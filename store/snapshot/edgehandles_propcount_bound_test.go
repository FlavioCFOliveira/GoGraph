package snapshot

// Regression lock-in for the 2026-07-01 hostility+load audit finding F2
// (#1829): the per-record property map in readEdgeHandleRecord was allocated
// with make(map, propCount) using the raw on-disk propCount, validated only
// against the loose edgeHandlesMaxCount (1<<40) plausibility ceiling. Because
// a propCount of math.MaxUint32 is BELOW that ceiling, it passed the guard and
// reached the make, where Go eagerly pre-allocates hash buckets proportional
// to the hint — a tens-of-gigabyte allocation that OOM-crashes the process at
// recovery from a hostile/corrupt snapshot, BEFORE the truncated body is
// detected. Every sibling reader clamps the eager reservation with
// capHint(count, *CapHintMax); this per-record map was the sole outlier.
//
// The fix clamps the hint with capHint(uint64(propCount), edgeHandlesCapHintMax)
// so the allocation is bounded and a truncated body fails fast with a typed
// ErrEdgeHandlesCorrupted from readEdgeHandleProp. This test pins that: a
// one-record edgehandles.bin declaring propCount = math.MaxUint32 with NO
// property tuples following is rejected with ErrEdgeHandlesCorrupted rather
// than OOMing or panicking. Distinct from
// TestSec_Store_EdgeHandlesStringTableEagerCount, which exercises a count PAST
// the ceiling (rejected before the make); this one exercises a count UNDER the
// ceiling that reaches the make and relies on the capHint clamp.

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestSec_Store_EdgeHandlesPropCountMapBounded(t *testing.T) {
	var b []byte
	putU32 := func(v uint32) { b = binary.LittleEndian.AppendUint32(b, v) }
	putU64 := func(v uint64) { b = binary.LittleEndian.AppendUint64(b, v) }

	putU32(edgeHandlesMagic)
	putU32(edgeHandlesFormatVersion)
	putU64(0) // labels string table: empty
	putU64(0) // keys string table: empty
	putU64(1) // record count = 1
	// record 0
	putU64(0)              // Src
	putU64(0)              // Dst
	putU64(0)              // Handle
	putU32(0)              // labelCount = 0
	putU32(math.MaxUint32) // propCount = 0xFFFFFFFF (< edgeHandlesMaxCount, so it reaches the make)
	// truncated: no property tuples follow.

	var err error
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				t.Fatalf("ReadEdgeHandles panicked on a hostile propCount instead of returning a typed error: %v", rec)
			}
		}()
		_, err = ReadEdgeHandles(bytes.NewReader(b))
	}()

	if err == nil {
		t.Fatal("ReadEdgeHandles accepted a record declaring propCount=MaxUint32 with a truncated body; want ErrEdgeHandlesCorrupted")
	}
	if !errors.Is(err, ErrEdgeHandlesCorrupted) {
		t.Fatalf("ReadEdgeHandles error = %v; want errors.Is(ErrEdgeHandlesCorrupted)", err)
	}
}
