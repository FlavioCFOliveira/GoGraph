package audit352_test

// allocattrib_tinyprobe_test.go — why the memory profile's object count sits
// BELOW runtime.MemStats.Mallocs, by construction and by exactly how much.
//
// TestAllocAttributionAgreesWithMallocs proved the bracketed attribution is
// repeatable to 0.00%. It also exposed a DETERMINISTIC shortfall against
// Mallocs — 1 374 objects at n=1000, 179 933 in the 120 000-row sort window,
// 59 931 in the 120 000-row scan window, identical to the object across arms and
// across runs. A lag would not be deterministic, so the cause is structural, and
// a share taken from a profile whose total is unexplained is not evidence.
//
// This file isolates the cause with three windows of known composition.

import (
	"testing"
)

// TestAllocProfileVsMallocsByAllocationKind allocates a known number of objects
// of one known kind per window and reports how many the memory profile saw.
//
// runtime.MemStats.Mallocs is documented as the count of heap objects allocated;
// it is incremented for TINY allocations (objects below 16 bytes containing no
// pointers) individually, because the runtime tracks them in mcache.tinyAllocs,
// while the memory profile records the underlying 16-byte block. If that is the
// cause, the noscan-tiny window will show a large object shortfall and a small
// BYTE shortfall, and the other two windows will show neither.
func TestAllocProfileVsMallocsByAllocationKind(t *testing.T) {
	const iter = 200_000

	var sinkTiny []*[8]byte
	var sinkSmall [][]byte
	var sinkMap []map[string]int

	cases := []struct {
		name string
		fn   func()
	}{
		{"noscan_tiny_8B", func() {
			sinkTiny = make([]*[8]byte, 0, iter)
			for i := 0; i < iter; i++ {
				sinkTiny = append(sinkTiny, new([8]byte))
			}
		}},
		{"noscan_small_64B", func() {
			sinkSmall = make([][]byte, 0, iter)
			for i := 0; i < iter; i++ {
				sinkSmall = append(sinkSmall, make([]byte, 64))
			}
		}},
		{"map_small", func() {
			sinkMap = make([]map[string]int, 0, iter)
			for i := 0; i < iter; i++ {
				sinkMap = append(sinkMap, make(map[string]int, 4))
			}
		}},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			at := exerciseAttributed(t, 1, c.fn)
			objRatio := float64(at.totalObjects) / float64(at.windowMallocs)
			byteRatio := float64(at.totalBytes) / float64(at.windowBytes)
			t.Logf("%-18s profile=%d objs / %d B   mallocs=%d / TotalAlloc=%d B   "+
				"obj ratio=%.4f  byte ratio=%.4f  shortfall=%d objs",
				c.name, at.totalObjects, at.totalBytes, at.windowMallocs, at.windowBytes,
				objRatio, byteRatio, int64(at.windowMallocs)-at.totalObjects)
		})
	}
	// Keep the sinks reachable so nothing is optimised away.
	if len(sinkTiny)+len(sinkSmall)+len(sinkMap) == 0 {
		t.Fatal("sinks empty: the probe allocated nothing")
	}
}
