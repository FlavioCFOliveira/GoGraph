//go:build race

package proto

// wantMsgAllocs is the number of heap objects ReadMessage must allocate per
// single-chunk inbound message in a RACE-INSTRUMENTED build: two.
//
// The extra object is NOT a defect and NOT present in shipped binaries. It is
// the `make([]byte, chunkLen)` temporary in the reassembly loop, which a normal
// build elides: cmd/compile/internal/walk.isAppendOfMake begins
//
//	if base.Flag.N != 0 || base.Flag.Cfg.Instrumenting { return false }
//
// so under -race (Instrumenting) the `append(s, make([]T, n)...)` rewrite to
// growslice + memclr is DISABLED and the temporary slice is really allocated.
//
// Recorded here because it is a trap for allocation measurement in this
// package: any allocs/op figure gathered under -race overstates the shipped
// cost by one object per chunk. Measure allocations WITHOUT -race.
const wantMsgAllocs = 2
