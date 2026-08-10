package exec

import "testing"

// The two benchmarks below pin the opposite ends of the rmp #2389 trade, and
// they exist as a PAIR because either one alone is satisfied by a wrong answer:
// reserving nothing wins the single-row shape and loses the full-batch one,
// reserving the whole capacity does the reverse. Both must stay cheap.
//
// Read them with -benchmem and watch B/op, not allocs/op. An allocation COUNT is
// blind to this regression: reserving 4096 int64 slots for one row and reserving
// 16 are both exactly one allocation, and the shipped fix for #2381 was measured
// against a count. The 4096x difference is only visible in bytes.

// BenchmarkDynamicChunkSingleRow is the OLTP shape — an indexed point lookup,
// which returns one row and exposes no RowCountHint, so the chunk falls back to
// DefaultChunkCapacity. This is examples/35_mvcc_mixed_workload's hot query,
// executed there ~3.2 million times per run. B/op must stay near the
// dynamicCommitFloor reservation (~128 B for an int64 column), not near the
// 32 KB the capacity would cost.
func BenchmarkDynamicChunkSingleRow(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := NewDynamicChunk(DefaultChunkCapacity, 1)
		c.PutInt64(0, int64(i))
		sinkChunk = c
	}
}

// BenchmarkDynamicChunkFullBatch is the scan/aggregation shape — a chunk that
// really does receive its whole capacity, as examples/23_bolt_server drives
// through EagerAggregation. Removing the reservation entirely moves this cost
// into append's doubling series (measured there: 636 MB of reservation became
// 2464 MB of growth copying), so B/op must stay near one capacity-sized backing.
func BenchmarkDynamicChunkFullBatch(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c := NewDynamicChunk(DefaultChunkCapacity, 1)
		for row := 0; row < DefaultChunkCapacity; row++ {
			c.PutInt64(0, int64(row))
		}
		sinkChunk = c
	}
}

// sinkChunk keeps the compiler from proving the chunks dead and eliding the
// allocations the benchmarks exist to measure.
var sinkChunk *Chunk
