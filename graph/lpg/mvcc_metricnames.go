package lpg

// mvcc_metricnames.go — the per-bucket series names, built once (rmp #2312).
//
// # Why this file exists, measured
//
// The MVCC series that carry a bucket in their NAME — conflicts by store, the
// chain-depth histogram, the per-store depth summaries — were first published by
// concatenating the prefix with the bucket's label at the call site. Every
// concatenation allocates a string, and [Graph.publishMVCCMetrics] runs on every
// vacuum pass, so the cost is paid for as long as the graph is churning.
//
// It measured 7 236 603 allocations — 21.8% of every allocation in the process —
// on BenchmarkLabelWrite/nodes=10000/deltas=false at 2 000 000 iterations, and it
// moved the benchmark's reported allocs/op from 4 to 5. That number is not a write
// cost: it is the VACUUM's, attributed to the benchmark because -benchmem counts the
// whole process. It is a regression all the same, and the module's rule is that an
// allocation regression is not merged without a documented justification. There is no
// justification available for allocating a constant.
//
// The names are fixed at build time, so they are built once here and indexed. The
// tables are package-level and immutable after init; they are never rebuilt and never
// mutated.
//
// The alternative — publishing ONE series with the bucket as a Prometheus LABEL —
// is what a label-aware backend would want, and the module's [metrics.Backend] does
// not have labels: it takes a name and a value. Adding them is a change to the whole
// observability surface and belongs to whoever needs it, not to this task.

import "github.com/FlavioCFOliveira/GoGraph/graph/mvcc"

// conflictStoreSeries[i] is the cumulative per-store conflict gauge for bucket i,
// and conflictStoreCounters[i] the counter incremented at the detection site.
var conflictStoreSeries, conflictStoreCounters = func() ([mvcc.ConflictStoreCount]string, [mvcc.ConflictStoreCount]string) {
	var gauges, counters [mvcc.ConflictStoreCount]string
	for i := 0; i < mvcc.ConflictStoreCount; i++ {
		gauges[i] = "lpg.mvcc.conflicts.total.store." + mvcc.ConflictStoreMetric(i)
		counters[i] = "lpg.mvcc.conflicts.store." + mvcc.ConflictStoreMetric(i)
	}
	return gauges, counters
}()

// chainDepthBucketSeries[i] is the gauge for chain-depth bucket i.
var chainDepthBucketSeries = func() [mvcc.DepthBuckets]string {
	var names [mvcc.DepthBuckets]string
	for i := 0; i < mvcc.DepthBuckets; i++ {
		names[i] = "lpg.mvcc.chain_depth.bucket." + mvcc.DepthBucketLabel(i)
	}
	return names
}()

// chainDepthStoreDeepest and chainDepthStoreChains are the per-store depth summaries,
// in [depthStore] order.
var chainDepthStoreDeepest, chainDepthStoreChains = func() ([depthStoreCount]string, [depthStoreCount]string) {
	var deepest, chains [depthStoreCount]string
	for i, name := range depthStoreNames {
		deepest[i] = "lpg.mvcc.chain_depth.deepest.store." + name
		chains[i] = "lpg.mvcc.chain_depth.chains.store." + name
	}
	return deepest, chains
}()
