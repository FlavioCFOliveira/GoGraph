package prometheus

// striped.go — the per-core accumulators that keep a HOT metric from
// serialising every core on one cache line (rmp #2698).
//
// # The defect this exists to remove
//
// A counter is one atomic.Uint64. Every emission is a read-modify-write on it,
// so every core that increments must own that cache line EXCLUSIVELY, in turn.
// The line, not the counter, becomes the bottleneck, and adding cores makes
// emission slower in absolute terms. Measured on this host (Apple M4, 10 cores,
// Go 1.27.1) through the registry's own hot path:
//
//	goroutines            1        8       64
//	ns/op             8.451    48.25    49.75      <- one shared atomic
//	scaling            1.00    0.175    0.170
//
// That is Compliance Mandate 3's "a hot path that serialises every caller" in
// its cache-coherence form, and the mandate calls it a defect rather than a
// missed optimisation.
//
// # What was measured, and what it refuted
//
// Two candidate causes were separated by a 2x2 experiment (lookup present or
// absent) x (counter shared or unshared). The RESULT REFUTED THE OBVIOUS
// SUSPECT: the sync.Map lookup SCALES — resolving the handle and discarding it
// ran 8.350ns at one goroutine and 1.474ns at eight, a 5.67x speed-up, because
// Go 1.24+ backs sync.Map with a lock-free HashTrieMap whose Load of a present
// key is a pure read. Removing the lookup entirely and incrementing a cached
// handle — the shape prometheus/client_golang uses — measured 1.904ns at one
// goroutine and 23.63ns at eight, a scaling of 0.081x: WORSE than the full
// path. Caching the handle does not fix this defect; only unsharing the line
// does. The same experiment showed a padded, unshared counter scaling 7.43x.
//
// # Choosing a shard, and why not the obvious ways
//
// Go exposes no stable per-CPU identity; docs/design-reader-indicator.md
// settled that for the reader indicator and graph/mvcc/horizon.go restates it.
// Three candidates were measured here:
//
//   - A ROTATING ATOMIC COUNTER, which graph/mvcc/horizon.go uses, is itself a
//     shared read-modify-write and would reintroduce the very contention being
//     removed. Rejected by construction, not measured.
//   - RANDOM selection per emission (math/rand/v2, whose generator is per-M and
//     lock-free) measured 4.638ns at one goroutine and 11.50ns at eight — a
//     scaling of 0.40x. It spreads the traffic but never CONVERGES: each core
//     keeps migrating between lines, so no line ever settles in one core's
//     cache. Random striping was refuted as a fix.
//   - A sync.Pool TOKEN, which graph/index/count/count.go uses for affinity,
//     works — 2.76x at eight — but the Get/Put round trip costs 7.55ns against
//     a whole uncontended operation budget of 1.9ns.
//
// What is left is the goroutine's own STACK ADDRESS. Every goroutine has its
// own stack, so the address of a local is a per-goroutine value that is stable
// between stack growths, which is exactly the convergence random selection
// lacks, and it costs 0.529ns — an address computation, a shift and a mask.
//
// The low bits are useless: they encode the frame's offset within the stack,
// identical for every goroutine running this function. The stack BASE is what
// differs, so the address is shifted past Go's 2 KiB minimum stack before
// masking. Which bit that is was measured, not assumed; see [minStackShift]. Multiplicative (Fibonacci) mixing was tried and MADE IT WORSE —
// 0.642ns +/-99% against 0.319ns +/-41% at eight goroutines — because Go
// allocates goroutine stacks in a regular pattern, so adjacent stacks map to
// adjacent shards, and hashing destroys that spread. The raw shift-and-mask
// won on both mean and variance.
//
// The choice is a HINT and never a correctness input. A goroutine whose stack
// moves simply starts using a different shard; nothing is lost, because every
// shard is summed at scrape time.
//
// # Why the shards are installed lazily
//
// Striping every metric from creation would cost 32 x 128 bytes per series.
// Against the module's real cardinality — 268 counter names and 222 histogram
// names emitted from production code — that is about 2 MB of mostly-idle
// padding, against roughly 24 KB today. Compliance Mandate 4 calls an avoidable
// allocation a defect, and for the overwhelming majority of series, which are
// emitted from cold paths and never contended, the whole array would be
// avoidable.
//
// So a series starts as the single atomic it is today and grows shards only
// when it is PROVEN contended. The proof is free and it is exact: the
// unpromoted path swaps the value in with CompareAndSwap instead of adding to
// it, and a failed swap means another goroutine wrote between the load and the
// swap. That is the contention itself, observed rather than predicted. The
// technique is Doug Lea's, from java.util.concurrent.atomic.LongAdder (JSR-166,
// jsr166/src/main/java/util/concurrent/atomic/Striped64.java), which promotes
// its per-thread cells on exactly this signal.
//
// A counter emitted a billion times from one goroutine therefore never
// allocates a shard, and a counter two goroutines fight over allocates one
// almost immediately.

import (
	"sync/atomic"
	"unsafe"
)

// cacheLine is the padding unit. 128 rather than 64 because Go's own runtime
// uses 128 for arm64 (internal/cpu.CacheLinePadSize, whose comment names Apple
// silicon as the reason), because macOS reports hw.cachelinesize = 128 on Apple
// silicon, and because 128 is a multiple of the 64-byte line x86 and Neoverse
// use, so one padded slot never straddles two lines there either.
//
// graph/index/count/count.go and graph/mvcc/horizon.go make the same choice for
// the same reason and record the literature's disagreement about it; this
// package follows them so the module has one answer, not three.
const cacheLine = 128

// shardCount is the number of per-core accumulators a promoted series holds.
//
// It is a power of two so the index is a mask rather than a division, and it is
// a COMPILE-TIME constant so the array index needs no bounds check.
//
// 32 was chosen by measurement, not by the core count. Because shard selection
// has no per-CPU identity to work from it is effectively a random assignment,
// so the shard count has to beat the birthday problem rather than merely match
// the cores: with 16 shards and 8 goroutines only about 11% of assignments are
// collision-free, and the measured spread showed it (+/-162%). Against 32, both
// 64 and 128 shards measured indistinguishable on this 10-core host at 8 and at
// 64 goroutines, with every spread overlapping:
//
//	shards        8 goroutines        64 goroutines
//	32          0.4184ns +/-105%     0.3566ns +/- 8%
//	64          0.5216ns +/- 43%     0.3117ns +/-22%
//	128         0.3205ns +/- 85%     0.2834ns +/-12%
//
// Mandate 4 breaks the tie: 32 is half the memory of 64 and a quarter of 128
// for no measured throughput, so 32 is what a promoted series costs.
//
// The constant is sized for hosts of this order. A host with substantially more
// cores should re-run BenchmarkIncCounterParallel before assuming it still
// holds; that benchmark is committed for exactly that purpose.
const shardCount = 32

// shardMask selects a shard from a stack address.
const shardMask = shardCount - 1

// minStackShift is how far the stack address is shifted before masking.
//
// Go's minimum goroutine stack is 2 KiB (runtime.stackMin, unchanged since Go
// 1.4), so consecutive goroutine stacks are at least 2^11 bytes apart and bit
// 11 is the LOWEST bit that distinguishes one stack from the next. Bits below
// it carry the frame's offset within the stack, which is identical for every
// goroutine calling this function.
//
// This constant was wrong once, and the way it was wrong is worth keeping. It
// was first set to 13, from a misremembered 8 KiB minimum stack. Nothing failed
// — every test passed and the exposition was correct — but the shards stopped
// doing their job: measured across 8 concurrent goroutines, only 2 of the 32
// shards were ever occupied, 4 goroutines to a shard, so the design still
// serialised four cores per line and merely did it on two lines instead of one.
// It showed up only as VARIANCE, a level-8 sweep bouncing between 1.5x and 3.9x
// depending on which stacks the run happened to draw.
//
// The distribution is therefore measured rather than reasoned about, by
// TestShardIndex_SpreadsAcrossShards. At shift 11, 8 goroutines occupy 8
// distinct shards and 64 occupy all 32; at shift 13 the same measurement gives
// 2 and 13.
const minStackShift = 11

// shardIndex returns the calling goroutine's preferred shard.
//
// The returned value is a HINT: it identifies a shard that this goroutine will
// keep choosing while its stack stays put, which is what gives the per-core
// affinity. Correctness never depends on it — every shard is summed on read, so
// a goroutine that migrates to another shard loses nothing.
func shardIndex() uint64 {
	// x is never read. Its ADDRESS is the value being sampled: it lies in the
	// calling goroutine's stack, so it distinguishes goroutines from one
	// another. The conversion is to uintptr for arithmetic only and the result
	// is never converted back to a pointer, so it is safe against a moving
	// stack: a stack copy changes which shard this goroutine prefers and
	// nothing else.
	var x byte
	return uint64(uintptr(unsafe.Pointer(&x))>>minStackShift) & shardMask //nolint:gosec // G103: address sampled as an integer hint; never dereferenced.
}

// counterShard is one core's share of a counter. It owns a whole cache line so
// two cores incrementing never touch the same one.
type counterShard struct {
	n atomic.Uint64
	_ [cacheLine - 8]byte
}

// counterShards is a promoted counter's per-core accumulator array.
type counterShards struct {
	slots [shardCount]counterShard
}

// histShard is one core's share of a histogram: the whole observation state,
// on its own cache line. The three fields are updated together by one
// observation, so co-locating them is what SHOULD happen; the defect they had
// was being shared between cores, not being adjacent.
//
// The payload is 10 buckets plus inf plus sumNs = 96 bytes, which fits inside
// one 128-byte line with 32 bytes to spare.
type histShard struct {
	buckets [len(latencyBuckets)]atomic.Uint64
	inf     atomic.Uint64
	sumNs   atomic.Int64
	_       [cacheLine - (len(latencyBuckets)+2)*8]byte
}

// histShards is a promoted histogram's per-core accumulator array.
type histShards struct {
	slots [shardCount]histShard
}

// probeMask samples the contention probe: a series tests for concurrent writers
// on one emission in probeMask+1, and does nothing but its plain atomic add on
// the rest.
//
// # Why sampled rather than every time
//
// The probe was first written to run on every emission, by replacing the add
// with a load-and-swap. It works, but it was MEASURED to cost +21.8% on the
// uncontended path — BenchmarkIncCounter 8.351ns -> 10.170ns (p=0.000, n=8,
// interleaved A/B) — and that cost falls on every one of the module's cold
// series to detect a condition they are never in. Sampling moves it off the
// common path: at one probe in 256 the amortised cost is under 0.01ns.
//
// # Why it still detects contention promptly
//
// The probe fails when another goroutine writes inside the ~2ns window between
// its load and its swap. Under the contention that matters, writers arrive on
// that counter roughly every 48ns per core (the measured contended cost), so
// each probe has a substantial chance of catching one and a handful of probes
// is enough. A series therefore promotes within a few thousand emissions of
// becoming contended, which against emission counts in the millions is a
// warm-up, not a cost.
//
// 256 is a power of two so the test is a mask on the value the add already
// returned, needing neither a division nor a second atomic.
const probeMask = 255
