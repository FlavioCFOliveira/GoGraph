//go:build !race

package proto

// wantMsgAllocs is the number of heap objects ReadMessage must allocate per
// single-chunk inbound message in a NORMAL (uninstrumented) build: exactly one,
// the reassembled message buffer the caller owns.
//
// The `make([]byte, chunkLen)` in the reassembly loop contributes nothing here.
// cmd/compile/internal/walk.isAppendOfMake recognises `append(s, make([]T,
// n)...)` and walk.extendSlice rewrites it to growslice + memclr, so the
// temporary is never materialised. See the race-build counterpart for what
// changes under instrumentation.
const wantMsgAllocs = 1
