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
			t.Errorf("list n=%d: charge %d < real Go allocation %d — decoded-memory budget UNDER-counts lists",
				n, charge, listActual)
		}
	}
}
