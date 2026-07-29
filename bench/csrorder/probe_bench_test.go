package csrorder

import (
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// probe_bench_test.go — the linear-vs-binary neighbour probe, per degree.
//
// This makes the #2139 calibration (docs/design-degree-adaptive-adjacency.md
// §2.2) PERMANENT. That calibration was measured by a harness deliberately kept
// outside the working tree (§12), which means the sprint's own load-bearing
// measurement could not be re-run — and §2.4 of the audit is refuted precisely
// because ITS harness was also unavailable. A measurement that cannot be
// reproduced is not evidence, so the calibration is reproduced here.
//
// # Why a synthetic arena rather than a real fixture's CSR
//
// Arena size is not a detail, it is the dominant variable. §2.2 measured binary
// search at 7-12 ns per LEVEL in a cold arena against 1-4 ns/level when the
// array is L1-resident, because every level is a dependent load whose address
// depends on the previous comparison, so the levels serialise and cannot
// prefetch. A cache-resident arena therefore FLATTERS binary search and would
// overstate the win — the exact error class the #2139 harness had to correct for
// when 64 rotating keys flattered binary by 12x.
//
// The real fixtures in this package are ~4 MiB of edges, which is L2-resident on
// a modern machine. So the primitive sweep uses a declared [arenaBytes] arena,
// sized to exceed L2 on the machines this project is measured on, and the query
// benchmarks measure the real engine on the real fixtures. Building a flat array
// is far cheaper than building a graph, so the honest regime costs almost
// nothing here.

// arenaBytes is the size of the synthetic edges array the primitive probe walks.
// 64 MiB is roughly 4x the largest L2 on the machines this project is benchmarked
// on, which is the sizing #2139 settled on after finding a smaller arena
// unrepresentative.
const arenaBytes = 64 << 20

// arenaSlots is arenaBytes expressed in graph.NodeID slots (8 bytes each).
const arenaSlots = arenaBytes / 8

// probeArena is one synthetic CSR-shaped edges array plus the metadata needed to
// generate hit and miss keys for it.
type probeArena struct {
	// edges is the flat neighbour array, laid out as runs of `degree` slots.
	edges []graph.NodeID
	// degree is the length of every run.
	degree uint64
	// runs is len(edges)/degree.
	runs uint64
}

// arenaCache memoises built arenas. Each is 64 MiB, and the sub-benchmark loops
// below run once per -count iteration, so without memoisation a -count=10 run
// would build 360 arenas and churn over 20 GiB of allocation — dwarfing the
// probes being measured and injecting GC pauses into every sample.
var (
	arenaMu    sync.Mutex
	arenaCache = map[arenaKey]*probeArena{}
)

// arenaKey identifies a memoised arena by the two parameters that define it.
type arenaKey struct {
	degree  uint64
	ordered bool
}

// probeArenaFor returns the memoised arena for (degree, ordered).
func probeArenaFor(degree uint64, ordered bool) *probeArena {
	arenaMu.Lock()
	defer arenaMu.Unlock()
	k := arenaKey{degree: degree, ordered: ordered}
	if a, ok := arenaCache[k]; ok {
		return a
	}
	a := buildProbeArena(degree, ordered)
	arenaCache[k] = a
	return a
}

// buildProbeArena lays out runs of `degree` destinations across an
// [arenaBytes] array.
//
// Destinations within a run are EVEN and strictly ascending, so an odd key is
// guaranteed absent while still lying inside the run's [min, max] range. That is
// what makes the miss case an in-range miss — a miss key outside the range would
// let the binary search terminate after one comparison and the linear scan bail
// early, measuring neither algorithm honestly.
//
// When ordered is false the same multiset is written in a scattered order by
// walking the run with an odd stride, which is a full-cycle permutation of the
// run's positions. The two arms therefore hold IDENTICAL content and differ only
// in within-run order, which is the single variable under test.
func buildProbeArena(degree uint64, ordered bool) *probeArena {
	runs := uint64(arenaSlots) / degree
	if runs == 0 {
		runs = 1
	}
	total := runs * degree
	edges := make([]graph.NodeID, total)
	a := &probeArena{edges: edges, degree: degree, runs: runs}
	for r := uint64(0); r < runs; r++ {
		base := a.base(r)
		run := edges[r*degree : (r+1)*degree]
		if ordered {
			for k := uint64(0); k < degree; k++ {
				run[k] = graph.NodeID(base + 2*k)
			}
			continue
		}
		// Scatter: slot k receives the (k*stride mod degree)-th destination of the
		// run. Every swept degree is a power of two and the stride is odd, so the
		// two are coprime and the walk is a full-cycle permutation — it visits
		// every index exactly once, which is what keeps the multiset identical to
		// the ordered layout.
		const stride = 40_507 // odd
		for k := uint64(0); k < degree; k++ {
			run[k] = graph.NodeID(base + 2*((k*stride)%degree))
		}
	}
	return a
}

// base is the first destination of run r.
//
// Runs are spaced by 2*degree, which is exactly the span a run occupies given
// its destinations are the `degree` even numbers from base. Spacing by a fixed
// constant instead would make high-degree runs OVERLAP their neighbours' key
// ranges; that would not corrupt a probe (each probe is bounded to one run's
// slice, and an odd key is absent from every run) but it would invalidate the
// simple statement that a run owns its key range, which the arena's tests assert
// against.
func (a *probeArena) base(r uint64) uint64 { return r * 2 * a.degree }

// lcg is the multiplicative step of a 64-bit linear congruential generator, used
// to make the probe's key stream unpredictable.
//
// This is the other trap #2139 recorded: a short rotating key set is perfectly
// branch-predicted and cache-resident, and measuring with 64 rotating keys
// flattered binary search by 12x. The tell was that a MISS came out cheaper than
// a HIT, which is impossible for a scan that must run to the end of the run on a
// miss. An LCG-driven stream removes it.
const (
	lcgMul = 6364136223846793005
	lcgInc = 1442695040888963407
)

// benchmarkProbe drives one arm of the probe sweep.
//
// probe is the function under test, hit selects whether the generated key is
// present in the run, and the arena fixes the degree. Nothing is logged: a
// b.Log inside a benchmark interleaves with the result lines and has previously
// made benchstat see a fraction of the benchmarks in a run, so per-run facts are
// reported through b.ReportMetric only.
func benchmarkProbe(
	b *testing.B,
	arena *probeArena,
	hit bool,
	probe func(edges []graph.NodeID, start, end, dst uint64) (uint64, bool),
) {
	b.ReportAllocs()
	// The degree is already in the sub-benchmark name, so only the arena size is
	// reported here — it is the variable that decides whether the measurement is
	// in the memory-bound regime §2.2 calibrated, and it is not otherwise visible.
	b.ReportMetric(float64(len(arena.edges)*8)/(1<<20), "arenaMiB")

	var state uint64 = 0x2145_2141_2142_2143
	var found int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		state = state*lcgMul + lcgInc
		r := (state >> 33) % arena.runs
		k := (state >> 11) % arena.degree
		start := r * arena.degree
		key := arena.base(r) + 2*k
		if !hit {
			key++ // odd: in range, guaranteed absent
		}
		if _, ok := probe(arena.edges, start, start+arena.degree, key); ok {
			found++
		}
	}
	b.StopTimer()

	// Assert the arm actually did the work the name claims, so a fixture or key
	// generation bug shows up as a failure instead of an impossibly fast number.
	// This is the guard that would have caught #2139's flattered harness.
	switch {
	case hit && found != b.N:
		b.Fatalf("hit arm: found %d of %d probes; key generation is wrong", found, b.N)
	case !hit && found != 0:
		b.Fatalf("miss arm: found %d probes that must all miss", found)
	}
}

// BenchmarkProbe_Linear_Hit is the PRE-#2142 scan, hitting. Reported per degree.
func BenchmarkProbe_Linear_Hit(b *testing.B) {
	for _, d := range SweptDegrees {
		arena := probeArenaFor(uint64(d), true)
		b.Run(degreeName(d), func(b *testing.B) { benchmarkProbe(b, arena, true, ScanFirstDst) })
	}
}

// BenchmarkProbe_Linear_Miss is the PRE-#2142 scan, missing in range. The miss
// case is where the scan is worst: it must run to the end of the run.
func BenchmarkProbe_Linear_Miss(b *testing.B) {
	for _, d := range SweptDegrees {
		arena := probeArenaFor(uint64(d), true)
		b.Run(degreeName(d), func(b *testing.B) { benchmarkProbe(b, arena, false, ScanFirstDst) })
	}
}

// BenchmarkProbe_Binary_Hit is the shipped O(log d) probe, hitting.
func BenchmarkProbe_Binary_Hit(b *testing.B) {
	for _, d := range SweptDegrees {
		arena := probeArenaFor(uint64(d), true)
		b.Run(degreeName(d), func(b *testing.B) { benchmarkProbe(b, arena, true, SearchFirstDst) })
	}
}

// BenchmarkProbe_Binary_Miss is the shipped O(log d) probe, missing in range.
func BenchmarkProbe_Binary_Miss(b *testing.B) {
	for _, d := range SweptDegrees {
		arena := probeArenaFor(uint64(d), true)
		b.Run(degreeName(d), func(b *testing.B) { benchmarkProbe(b, arena, false, SearchFirstDst) })
	}
}

// BenchmarkProbe_Linear_Hit_Unordered scans a SCATTERED run holding the identical
// destination multiset.
//
// It exists to make the sweep's central comparison defensible by measurement
// rather than by argument. The other arms all run on the ORDERED array, which
// invites the objection that linear-on-ordered is not the pre-change cost. This
// benchmark answers it: a scan's cost is order-independent (on a miss it runs the
// whole run either way; on a hit the expected stopping position is degree/2 for a
// uniform key stream in both layouts), so comparing the two algorithms on one
// array isolates the algorithm instead of confounding it with the layout. If this
// ever diverges from BenchmarkProbe_Linear_Hit, the sweep's premise is wrong and
// the comparison must be restructured.
func BenchmarkProbe_Linear_Hit_Unordered(b *testing.B) {
	for _, d := range SweptDegrees {
		arena := probeArenaFor(uint64(d), false)
		b.Run(degreeName(d), func(b *testing.B) { benchmarkProbe(b, arena, true, ScanFirstDst) })
	}
}

// probeSink absorbs the control loop's result so the compiler cannot eliminate
// it. A package-level variable is the reliable form: a local one is provably dead
// after the loop and may be optimised away, which would report a floor of zero.
var probeSink uint64

// BenchmarkProbe_Control measures the key generation alone, with no probe. It is
// the floor to subtract before comparing against §2.2's net figures, which are
// control-subtracted. Without it the low-degree rows are dominated by harness
// cost and the ratio between the two arms is understated — §2.2's own d=16 parity
// of 1.01x only appears after subtracting a 7.1 ns floor.
func BenchmarkProbe_Control(b *testing.B) {
	for _, d := range SweptDegrees {
		arena := probeArenaFor(uint64(d), true)
		b.Run(degreeName(d), func(b *testing.B) {
			b.ReportAllocs()
			var state uint64 = 0x2145_2141_2142_2143
			var sink uint64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				state = state*lcgMul + lcgInc
				r := (state >> 33) % arena.runs
				k := (state >> 11) % arena.degree
				sink += arena.base(r) + 2*k
			}
			probeSink += sink
		})
	}
}

// degreeName renders a sub-benchmark name that sorts and greps cleanly, and that
// benchstat renders as a distinct row per degree — the per-degree reporting rmp
// #2145 requires, so a regression at degree 8 cannot be averaged away by a win
// at degree 4096.
func degreeName(d int) string {
	return "degree=" + itoa(d)
}
