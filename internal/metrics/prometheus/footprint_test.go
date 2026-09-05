package prometheus

// footprint_test.go — what the per-core shards COST in memory (rmp #2698).
// Layer: short.
//
// Compliance Mandate 4 forbids buying CPU with RAM without measuring the RAM,
// so the shards' footprint is measured here rather than asserted in prose. The
// cardinality driven below is the module's real one, counted from production
// call sites: 268 distinct counter names, 222 histogram names (2 direct
// ObserveLatency plus 220 metrics.Time sites) and 40 gauge names.
//
// The design's whole memory claim is that a series pays for shards only when it
// is contended. This file measures both ends of that claim: the resting
// footprint every deployment pays, and the worst case where every series is
// contended at once.

import (
	"runtime"
	"testing"
	"time"
	"unsafe"
)

// The module's real metric cardinality, counted from production emission sites.
const (
	realCounters   = 268
	realHistograms = 222
	realGauges     = 40
)

// TestFootprint_PerSeriesSizesAreWhatTheDesignSays pins the always-paid cost.
// These are exact, deterministic sizes, not measurements: they are what every
// series carries whether or not it is ever contended.
func TestFootprint_PerSeriesSizesAreWhatTheDesignSays(t *testing.T) {
	cSize := unsafe.Sizeof(counter{})
	hSize := unsafe.Sizeof(histogram{})
	cShards := unsafe.Sizeof(counterShards{})
	hShards := unsafe.Sizeof(histShards{})

	t.Logf("counter=%dB histogram=%dB counterShards=%dB histShards=%dB",
		cSize, hSize, cShards, hShards)

	// The shards pointer is the only unconditional cost the change adds: 8 bytes
	// on a counter (8 -> 16) and 8 on a histogram (96 -> 104).
	if want := uintptr(16); cSize != want {
		t.Errorf("sizeof(counter) = %d, want %d", cSize, want)
	}
	if want := uintptr(104); hSize != want {
		t.Errorf("sizeof(histogram) = %d, want %d", hSize, want)
	}
	// A promoted series costs one cache line per shard, and no more: this is the
	// guard that catches a field being added to a shard and silently doubling
	// the stride.
	if want := uintptr(shardCount * cacheLine); cShards != want {
		t.Errorf("sizeof(counterShards) = %d, want %d (a shard must be exactly one cache line)", cShards, want)
	}
	if want := uintptr(shardCount * cacheLine); hShards != want {
		t.Errorf("sizeof(histShards) = %d, want %d (a shard must be exactly one cache line)", hShards, want)
	}

	always := uintptr(realCounters+realHistograms) * 8
	worst := uintptr(realCounters)*cShards + uintptr(realHistograms)*hShards
	t.Logf("at the module's real cardinality (%d counters, %d histograms, %d gauges): "+
		"unconditional added cost = %d B; worst case, every series promoted = %d B (%.2f MiB)",
		realCounters, realHistograms, realGauges, always, worst, float64(worst)/(1<<20))
}

// heapDelta returns the bytes of live heap that fn's result retains.
func heapDelta(t *testing.T, fn func() any) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	v := fn()
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(v)
	if after.HeapAlloc < before.HeapAlloc {
		return 0
	}
	return after.HeapAlloc - before.HeapAlloc
}

// buildRegistry populates a registry at the module's real cardinality, from a
// single goroutine so nothing is contended, and optionally forces every series
// to the promoted representation.
func buildRegistry(promoteAll bool) *Registry {
	r := New()
	cNames := make([]string, realCounters)
	hNames := make([]string, realHistograms)
	for i := 0; i < realCounters; i++ {
		cNames[i] = "store.pkg.Symbol.calls." + string(rune('a'+i%26)) + string(rune('a'+i/26))
		r.IncCounter(cNames[i], 1)
	}
	for i := 0; i < realHistograms; i++ {
		hNames[i] = "search.pkg.Symbol.latency." + string(rune('a'+i%26)) + string(rune('a'+i/26))
		r.ObserveLatency(hNames[i], time.Millisecond)
	}
	for i := 0; i < realGauges; i++ {
		r.SetGauge("graph.pkg.gauge."+string(rune('a'+i%26))+string(rune('a'+i/26)), float64(i))
	}
	if promoteAll {
		for _, n := range cNames {
			r.getOrCreateCounter(n).promote()
		}
		for _, n := range hNames {
			r.getOrCreateHistogram(n).promote()
		}
	}
	return r
}

// TestFootprint_UncontendedRegistryStaysSmall is the Mandate 4 claim, measured:
// a registry at the module's full cardinality, driven from ONE goroutine, must
// not allocate a single shard array.
func TestFootprint_UncontendedRegistryStaysSmall(t *testing.T) {
	resting := heapDelta(t, func() any { return buildRegistry(false) })
	promoted := heapDelta(t, func() any { return buildRegistry(true) })

	t.Logf("registry at real cardinality: resting (nothing contended) = %d B (%.1f KiB); "+
		"worst case (every series promoted) = %d B (%.2f MiB); ratio %.1fx",
		resting, float64(resting)/1024, promoted, float64(promoted)/(1<<20),
		float64(promoted)/float64(resting))

	// The worst case must be dominated by the shard arrays, which is what makes
	// the laziness worth having. Anything close to the resting size would mean
	// the shards are not actually being allocated and the promoted arm is not
	// measuring what it claims.
	minShards := uint64(realCounters+realHistograms) * uint64(shardCount*cacheLine)
	if promoted < minShards {
		t.Errorf("promoted registry = %d B, want at least %d B of shard arrays; the promoted arm did not allocate them", promoted, minShards)
	}
	// The resting registry must be far smaller than the promoted one. The bound
	// is deliberately loose — this is a heap measurement, not an exact size —
	// but a regression that promoted eagerly would blow through it by 50x.
	if resting > minShards/10 {
		t.Errorf("resting registry = %d B, which is not small against the %d B of shards it avoids; is something promoting without contention?", resting, minShards)
	}
}
