//go:build !race

package packstream_test

// charge_upperbound_alloc_test.go — security engagement 2026-07-02 R2 (#1849).
//
// Empirical proof that the decoded-memory charge is an UPPER bound on the real
// Go allocation, for both map[string]Value and []Value of boxed scalars, across
// representative sizes including the worst-case-after-a-bucket-doubling points
// (n=8/64/1000). This is the self-guarding half of the finding-A1 fix: if a
// future Go release or a cost-constant edit ever lets the charge dip below the
// measured PRODUCTION allocation, this test fails.
//
// Gated //go:build !race: the race detector disables the tiny allocator and
// adds shadow memory, inflating measured allocation (~32 B/elem for a boxed-int
// list vs ~24 B in production), which is instrumentation overhead the budget
// deliberately does not — and must not — account for. The budget bounds
// production memory; this test therefore measures production allocation. The
// non-alloc end-to-end rejection test (charge_upperbound_test.go) runs under
// -race.

import (
	"runtime"
	"strconv"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bolt/packstream"
)

// measureRetainedAlloc reports the TotalAlloc delta of building x, keeping the
// result alive across the second MemStats read so the container's allocation is
// counted. It forces a process-wide runtime.GC, so callers MUST run serially
// (no t.Parallel).
func measureRetainedAlloc(build func() any) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	x := build()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(x)
	return after.TotalAlloc - before.TotalAlloc
}

// TestDecoder_ChargeUpperBoundsGoAllocation proves charge >= real allocation for
// map[string]Value and []Value of boxed ints. Keys are pre-allocated outside the
// measured window so only the container's hmap/bucket (map) or backing+boxes
// (list) allocation is measured — key string data is charged separately against
// the wire byte budget in production.
func TestDecoder_ChargeUpperBoundsGoAllocation(t *testing.T) {
	// Coverage instrumentation, like the race detector this file already
	// excludes via //go:build !race, adds allocations the decoded-memory budget
	// deliberately does not — and must not — account for, inflating
	// measureRetainedAlloc's process-wide TotalAlloc delta well past the
	// production charge (observed on a linux/amd64 CI runner under -coverpkg: an
	// n=0 empty list "retaining" ~5 KiB, a physical impossibility for real
	// retained memory). The budget bounds PRODUCTION memory, so this test must
	// measure uninstrumented production allocation only.
	if testing.CoverMode() != "" {
		t.Skip("allocation measurement is unreliable under coverage instrumentation; see the //go:build !race rationale at the top of this file")
	}

	mapBase := int64(packstream.MapCollectionCostForTest())
	mapPer := int64(packstream.MapEntryCostForTest())
	listBase := int64(packstream.CollectionCostForTest())
	listPer := int64(packstream.ListElemCostForTest())

	for _, n := range []int{0, 1, 2, 8, 13, 14, 52, 53, 64, 1000, 100000} {
		keys := make([]string, n)
		for i := range keys {
			keys[i] = strconv.Itoa(1_000_000 + i) // distinct, > 255 so values box
		}

		mapActual := measureRetainedAlloc(func() any {
			m := make(map[string]packstream.Value, n)
			for i := 0; i < n; i++ {
				m[keys[i]] = int64(1_000_000 + i)
			}
			return m
		})
		if charge := mapBase + int64(n)*mapPer; charge < int64(mapActual) {
			t.Errorf("map n=%d: charge %d < real Go allocation %d — decoded-memory budget UNDER-counts maps",
				n, charge, mapActual)
		}

		listActual := measureRetainedAlloc(func() any {
			l := make([]packstream.Value, n)
			for i := 0; i < n; i++ {
				l[i] = int64(1_000_000 + i)
			}
			return l
		})
		if charge := listBase + int64(n)*listPer; charge < int64(listActual) {
			t.Errorf("int-list n=%d: charge %d < real Go allocation %d — decoded-memory budget UNDER-counts lists",
				n, charge, listActual)
		}

		// String-element list: boxing a string into a Value allocates a ~16-byte
		// string header. Keys are pre-allocated, so only the box is measured.
		strActual := measureRetainedAlloc(func() any {
			l := make([]packstream.Value, n)
			for i := 0; i < n; i++ {
				l[i] = keys[i]
			}
			return l
		})
		if charge := listBase + int64(n)*listPer; charge < int64(strActual) {
			t.Errorf("string-list n=%d: charge %d < real Go allocation %d — budget UNDER-counts string-element lists",
				n, charge, strActual)
		}

		// Bytes-element list: boxing a []byte allocates a 24-byte slice header —
		// the worst-case element box, and the shape that broke the pre-48
		// listElemCost. The underlying array is shared (pre-allocated), so only
		// the per-element slice-header box is measured, mirroring the decoder,
		// which charges Bytes payload bytes against the wire budget separately.
		sharedBytes := []byte("payload")
		bytesActual := measureRetainedAlloc(func() any {
			l := make([]packstream.Value, n)
			for i := 0; i < n; i++ {
				l[i] = sharedBytes
			}
			return l
		})
		if charge := listBase + int64(n)*listPer; charge < int64(bytesActual) {
			t.Errorf("bytes-list n=%d: charge %d < real Go allocation %d — budget UNDER-counts []byte-element lists",
				n, charge, bytesActual)
		}
	}
}
