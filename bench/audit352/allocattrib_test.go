package audit352_test

// allocattrib_test.go — a BRACKETED, EXACT allocation-attribution instrument for
// the sprint 352 audit, and the structural arm oracle for the #2652 A/B.
//
// # Why not `go test -memprofile`
//
// The audit's original allocation profile of the sort shape was taken with
// `-memprofile`, which writes ONE profile covering the whole process. In this
// package that profile also contains TestMain's fixture construction (120 000
// nodes and ~960 000 edges). On the DEFECTIVE build that did not matter: the
// legacy sort allocated tens of millions of objects per iteration and drowned
// the fixture out. On the FIXED build it matters enormously — the sort's share
// collapses, and the fixture would take the top of the profile and be reported
// as if it were a query cost.
//
// So the instrument here brackets the window instead: it snapshots
// runtime.MemProfile immediately before and immediately after the exercised
// window and reports the DIFFERENCE. Nothing outside the window can enter the
// attribution.
//
// # Why it is exact rather than sampled
//
// runtime.MemProfileRate is set to 1 for the window, so EVERY allocation is
// recorded and the shares are exact rather than 512 KB-sampled. That removes the
// sampling bias the original profile carried, at the cost of making the window
// slow — which is why this file measures attribution only and never time.
//
// # Why it is the arm oracle
//
// The legacy and decorated paths are distinguished by a FRAME, not by a number:
//
//	legacy   Sort: allocations beneath cypher/exec.(*Sort).rowLess
//	legacy   Top : allocations beneath cypher/exec.rowLessForKeys
//	decorated Sort: allocations beneath cypher/exec.(*Sort).sortDecorated,
//	                and ZERO beneath (*Sort).rowLess
//	decorated Top : allocations beneath cypher/exec.(*Top).consumeAndFinish,
//	                and ZERO beneath rowLessForKeys
//
// A frame cannot be mislabelled: absence of a frame is proof the code did not
// run, and at MemProfileRate=1 presence is not a sampling accident either. This
// is a strictly stronger arm assertion than reading the seam back, which only
// proves what was ASKED for.

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// MemProfile differencing
// ─────────────────────────────────────────────────────────────────────────────

// allocRec is one unique allocation stack's cumulative counters.
//
// stack ALIASES the reused record buffer rather than copying, so building a
// snapshot allocates nothing (see [readMemProfile]). It is therefore valid only
// for the MOST RECENT snapshot; [diffMemProfile] resolves frames from the
// `after` map only, and never reads `before`'s stacks, which is what makes the
// aliasing sound.
type allocRec struct {
	objects int64
	bytes   int64
	stack   []uintptr
}

// stackKey hashes a PC slice into a map key. PCs are stable for the process
// lifetime, so the same stack yields the same key across two snapshots.
//
// It returns a uint64 rather than a string ON PURPOSE: a string key has to be
// built, and building it allocates, and an allocation inside the snapshot path
// lands inside the very window the instrument is measuring (see
// [readMemProfile]). FNV-1a over the raw PCs is allocation-free and is what
// makes an empty window read exactly zero.
//
// A 64-bit hash can in principle collide, which would merge two stacks' counters
// into one siteDelta. That degrades attribution GRANULARITY only — the totals,
// and therefore every window oracle, are unaffected — and at the few thousand
// buckets this process holds the probability is around 2^-40.
func stackKey(pcs []uintptr) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for _, pc := range pcs {
		v := uint64(pc)
		for i := 0; i < 8; i++ {
			h ^= v & 0xff
			h *= prime64
			v >>= 8
		}
	}
	return h
}

// profRecs and profMaps are the snapshot machinery's REUSED storage.
//
// They are package-level, and that is the whole point: the instrument must
// allocate NOTHING between its opening and its closing snapshot, or its own
// storage lands inside the window it is measuring. See [readMemProfile] for the
// runtime mechanism that makes an allocation there impossible to hide, and
// TestAllocInstrumentDoesNotEnterItsOwnWindow for the assertion.
//
// profMaps has two slots because a bracket holds the opening snapshot alive
// while it takes the closing one; readMemProfile alternates between them and
// clears rather than reallocates, so the maps' buckets and the key strings they
// hold are reused from the second call onwards.
var (
	profRecs []runtime.MemProfileRecord
	profMaps [2]map[uint64]allocRec
	profSlot int
)

// readMemProfile returns every allocation stack the runtime currently holds,
// keyed by stack. The returned map is REUSED: it is valid only until the second
// following call, which is exactly the lifetime a bracket needs.
//
// inuseZero is true: the sort's allocations are all freed, so stacks whose live
// objects have dropped to zero MUST still be reported or the whole window would
// read as empty.
//
// Three GCs precede the read because runtime.MemProfile documents its result as
// possibly up to two collection cycles old and offers no explicit flush. The
// same-vs-same arm of TestAllocAttributionAgreesWithMallocs is what proves the
// number is sufficient rather than merely plausible.
//
// # Why this function must not allocate (rmp #2652)
//
// It used to allocate its record buffer and its result map on every call, and
// that made every bracketed window WRONG: the profile total disagreed with the
// TotalAlloc delta over the same window by 17x at n=2 and by 264x inside a full
// package run, so assertDescribesWindow failed at every cell of the A/B sweep.
//
// The cause is not the GC lag; it is that writing MemProfileRate = 0 does not
// silence the next allocation, it GUARANTEES it is recorded. Go 1.27's malloc
// fast paths gate the profiler as
//
//	c.nextSample -= int64(size)
//	if c.nextSample < 0 || MemProfileRate != c.memProfRate {
//		profilealloc(mp, x, size)
//	}
//
// (runtime/malloc.go, five call sites), and profilealloc RECORDS the sample and
// only then re-syncs c.memProfRate and sets c.nextSample = nextSample(), which
// returns MaxInt64 while the rate is 0. So each change of MemProfileRate forces
// exactly one profiled allocation per P before that P goes quiet. With a fresh
// buffer per call the victim was this function's own 112 KiB
// []runtime.MemProfileRecord, allocated after the opening snapshot had been read
// and flushed into the readable window by the closing snapshot's GCs — one
// phantom record of 114 688 bytes, inside a window whose real content was 9 176
// bytes.
//
// Reusing the storage removes the victim: the forced sample now lands on the
// first allocation AFTER the closing snapshot, which is outside the delta.
// Measured after the change: an empty window reads exactly 0 objects and 0
// bytes, and a window of 100 known 1 KiB allocations reads 102 432 profiled
// bytes against a 102 400-byte TotalAlloc delta (ratio 1.0003).
//
// Widening assertDescribesWindow's band would have been the wrong fix: the
// oracle was right and the instrument was wrong.
func readMemProfile() map[uint64]allocRec {
	runtime.GC()
	runtime.GC()
	runtime.GC()
	for {
		n, ok := runtime.MemProfile(profRecs, true)
		if ok {
			profRecs = profRecs[:n]
			break
		}
		// Grow with headroom so a bracket that adds a few buckets does not have
		// to grow again between its two snapshots.
		profRecs = make([]runtime.MemProfileRecord, n+512)
	}
	if profMaps[profSlot] == nil {
		profMaps[profSlot] = make(map[uint64]allocRec, 8192)
	}
	out := profMaps[profSlot]
	profSlot = (profSlot + 1) % len(profMaps)
	clear(out)
	for i := range profRecs {
		st := profRecs[i].Stack()
		key := stackKey(st)
		r := out[key]
		r.objects += profRecs[i].AllocObjects
		r.bytes += profRecs[i].AllocBytes
		if r.stack == nil {
			r.stack = st // alias, not copy: see allocRec
		}
		out[key] = r
	}
	profRecs = profRecs[:cap(profRecs)]
	return out
}

// warmProfileSnapshotStorage grows the reused snapshot storage and the maps'
// buckets so that no bracket has to grow them, and absorbs the ONE profiled
// allocation the runtime forces on each P after a MemProfileRate change (see
// [readMemProfile]).
//
// The caller must already have set MemProfileRate to 0 and must call this
// BEFORE the opening snapshot, so that both the growth and the forced sample
// land outside the window. It is cheap once the storage has reached its steady
// size: three profile reads with no allocation.
func warmProfileSnapshotStorage() {
	for i := 0; i < len(profMaps)+1; i++ {
		readMemProfile()
	}
}

// siteDelta is one allocation stack's contribution to the bracketed window.
type siteDelta struct {
	frames  []string // resolved, LEAF FIRST
	objects int64
	bytes   int64
}

// resolveFrames expands a PC slice into function names, leaf first, expanding
// inlined frames (so a function that was inlined into its caller still appears
// under its own name).
func resolveFrames(pcs []uintptr) []string {
	fr := runtime.CallersFrames(pcs)
	var out []string
	for {
		f, more := fr.Next()
		if f.Function != "" {
			out = append(out, f.Function)
		}
		if !more {
			break
		}
	}
	return out
}

// diffMemProfile returns the per-stack allocation deltas between two snapshots.
//
// It reads COUNTERS from both maps but STACKS only from after, which is what
// lets a snapshot alias the reused record buffer instead of copying (see
// [allocRec]). after must be the most recent snapshot taken.
func diffMemProfile(before, after map[uint64]allocRec) []siteDelta {
	var out []siteDelta
	for k, a := range after {
		b := before[k]
		dObj := a.objects - b.objects
		dB := a.bytes - b.bytes
		if dObj <= 0 && dB <= 0 {
			continue
		}
		out = append(out, siteDelta{frames: resolveFrames(a.stack), objects: dObj, bytes: dB})
	}
	return out
}

// attribution is the aggregated view of one bracketed window.
type attribution struct {
	totalObjects int64
	totalBytes   int64
	// windowMallocs is runtime.MemStats.Mallocs over the SAME window. It does
	// not lag, so it is the oracle that the profile total describes this window
	// and nothing else. See TestAllocAttributionAgreesWithMallocs.
	windowMallocs uint64
	// windowBytes is runtime.MemStats.TotalAlloc over the same window, the
	// byte-side counterpart of windowMallocs.
	windowBytes uint64
	// rate is the runtime.MemProfileRate the window ran at. 1 means the
	// attribution is EXACT; anything larger means the counts are Poisson-sampled
	// per byte and therefore biased towards large objects, so they may be used
	// for frame PRESENCE but not for shares.
	rate int
	// flatObjects/flatBytes attribute to the leaf frame after stripping the
	// runtime's allocator plumbing (see isAllocatorPlumbing), so the leaf is the
	// allocation KIND where the runtime performs one (runtime.convTstring for an
	// interface box, runtime.growslice for a re-grow) and the requesting function
	// otherwise.
	flatObjects map[string]int64
	flatBytes   map[string]int64
	// flatGoObjects/flatGoBytes attribute to the first NON-runtime frame: the
	// project or stdlib function that asked for the memory. Where the two flat
	// views disagree, the difference is exactly the runtime's own share.
	flatGoObjects map[string]int64
	flatGoBytes   map[string]int64
	// cum[fn] is what was allocated with fn ANYWHERE on the stack. A function
	// appearing twice on one stack (recursion) is counted once for that stack.
	cumObjects map[string]int64
	cumBytes   map[string]int64
	sites      []siteDelta
}

func attribute(sites []siteDelta) attribution {
	at := attribution{
		flatObjects: map[string]int64{}, flatBytes: map[string]int64{},
		flatGoObjects: map[string]int64{}, flatGoBytes: map[string]int64{},
		cumObjects: map[string]int64{}, cumBytes: map[string]int64{},
		sites: sites,
	}
	for _, s := range sites {
		at.totalObjects += s.objects
		at.totalBytes += s.bytes
		if leaf := leafAfterPlumbing(s.frames); leaf != "" {
			at.flatObjects[leaf] += s.objects
			at.flatBytes[leaf] += s.bytes
		}
		if leaf := leafAfterRuntime(s.frames); leaf != "" {
			at.flatGoObjects[leaf] += s.objects
			at.flatGoBytes[leaf] += s.bytes
		}
		seen := make(map[string]bool, len(s.frames))
		for _, f := range s.frames {
			if seen[f] {
				continue
			}
			seen[f] = true
			at.cumObjects[f] += s.objects
			at.cumBytes[f] += s.bytes
		}
	}
	return at
}

// isAllocatorPlumbing reports whether fn sits between the caller's allocation
// request and the point at which the profile record is taken.
//
// Go 1.27 split mallocgc into specialised fast paths
// (runtime.mallocgcSmallScanNoHeaderSC2 and friends). The runtime's memory
// profiler skips a FIXED number of frames, and with the extra fast-path frame in
// place that skip no longer lands on the caller: measured on the sort
// reproduction, 97.85% of alloc_objects came out flat under runtime.mallocgc,
// which makes an unfiltered flat attribution report the allocator instead of the
// program. Stripping these frames puts the flat leaf back where `go tool pprof
// -top` puts it.
//
// runtime.convT*, runtime.growslice, runtime.mapassign* and runtime.stringtoslice*
// are deliberately NOT plumbing: pprof reports them under their own names and they
// carry real information (an interface box, a slice re-grow).
func isAllocatorPlumbing(fn string) bool {
	if strings.HasPrefix(fn, "runtime.mallocgc") {
		return true
	}
	switch fn {
	case "runtime.profilealloc", "runtime.mProf_Malloc",
		"runtime.newobject", "runtime.makeslice", "runtime.makeslicecopy",
		"runtime.newarray", "runtime.makechan", "runtime.makemap",
		"runtime.makemap_small":
		return true
	}
	return false
}

// leafAfterPlumbing returns the first frame that is not allocator plumbing.
func leafAfterPlumbing(frames []string) string {
	for _, f := range frames {
		if !isAllocatorPlumbing(f) {
			return f
		}
	}
	return ""
}

// leafAfterRuntime returns the first frame outside package runtime.
func leafAfterRuntime(frames []string) string {
	for _, f := range frames {
		if !strings.HasPrefix(f, "runtime.") {
			return f
		}
	}
	return ""
}

// topSites renders the n largest individual allocation STACKS, leaf first, so a
// share can be traced to a call site rather than to a function name that occurs
// in several places. That ambiguity is real here: (*Project).Next occurs twice per
// query, once as the top-level operator and once inside every
// ParallelScanProject worker's fused sub-plan, which is why the cumulative shares
// of the two can sum above 100%.
func topSites(at *attribution, n, depth int) string {
	sites := append([]siteDelta(nil), at.sites...)
	sort.Slice(sites, func(i, j int) bool { return sites[i].objects > sites[j].objects })
	if len(sites) > n {
		sites = sites[:n]
	}
	var sb strings.Builder
	for i, s := range sites {
		fmt.Fprintf(&sb, "    [%d] %7.2f%%  %10d objs  %12d B\n", i,
			100*float64(s.objects)/float64(at.totalObjects), s.objects, s.bytes)
		shown := 0
		for _, f := range s.frames {
			if isAllocatorPlumbing(f) {
				continue
			}
			fmt.Fprintf(&sb, "            %s\n", shortFn(f))
			shown++
			if shown >= depth {
				break
			}
		}
	}
	return sb.String()
}

// shortFn drops the module path so tables stay readable.
func shortFn(fn string) string {
	return strings.TrimPrefix(fn, "github.com/FlavioCFOliveira/GoGraph/")
}

// exerciseAttributed runs fn inside a bracketed window at the given
// MemProfileRate and returns the allocation attribution of everything fn
// allocated.
//
// # Why the rate is switched around fn and not around the whole function
//
// The instrument's own snapshot machinery allocates: one []MemProfileRecord of
// ~300 bytes per bucket, one map entry per bucket, and a formatted key per
// bucket. At rate 1 those allocations are profiled too, they land AFTER the
// opening snapshot has been read, and they therefore appear inside the window.
// Measured at n=1000 they outnumbered the query's own allocations 15 to 1 and
// varied from call to call, which made two identical windows disagree by 37.8%.
//
// Profiling is therefore DISABLED (rate 0) while the snapshots are taken and
// enabled only around fn, so nothing but fn can enter the attribution. This is
// the fix that TestAllocAttributionAgreesWithMallocs verifies.
//
// # Disabling the rate is not sufficient on its own (rmp #2652)
//
// Setting MemProfileRate = 0 does not silence the next allocation, it forces the
// runtime to RECORD one — once per P — because the profiler's gate fires on
// `MemProfileRate != c.memProfRate` and it is profilealloc, which records, that
// re-syncs the two. [readMemProfile] carries the runtime citation and the
// measured consequence: a 114 688-byte phantom inside a 9 176-byte window.
//
// Two things follow, and both are load-bearing here:
//
//   - the snapshot path must allocate NOTHING, so that the forced sample has no
//     victim inside the bracket. That is why profRecs and profMaps are reused.
//   - warmProfileSnapshotStorage is called after the rate is dropped to 0 and
//     BEFORE the opening snapshot, so the forced sample for that transition is
//     absorbed outside the window. The rate -> 0 transition after fn is absorbed
//     the same way, by the snapshot machinery being allocation-free.
//
// TestAllocInstrumentDoesNotEnterItsOwnWindow asserts the result directly: an
// empty window must read exactly zero.
//
// It is NOT safe to call concurrently with anything else in the process:
// MemProfileRate and the memory profile are process-global.
func exerciseAttributed(tb testing.TB, rate int, fn func()) attribution {
	tb.Helper()
	prevRate := runtime.MemProfileRate

	runtime.MemProfileRate = 0
	warmProfileSnapshotStorage()
	before := readMemProfile()

	var msA, msB runtime.MemStats
	runtime.ReadMemStats(&msA)
	runtime.MemProfileRate = rate
	fn()
	runtime.MemProfileRate = 0
	runtime.ReadMemStats(&msB)

	after := readMemProfile()
	runtime.MemProfileRate = prevRate

	at := attribute(diffMemProfile(before, after))
	at.windowMallocs = msB.Mallocs - msA.Mallocs
	at.windowBytes = msB.TotalAlloc - msA.TotalAlloc
	at.rate = rate
	return at
}

// assertDescribesWindow fails unless the profile totals agree with the
// non-lagging MemStats deltas over the same window. It is the oracle that caught
// the instrument polluting its own window by 15x; every share quoted from an
// exact (rate 1) window must pass it.
//
// BYTES are the exact axis and carry the assertion. OBJECTS are not exact, and
// the band reflects a MEASURED property of the runtime rather than a guessed
// tolerance: TestAllocProfileVsMallocsByAllocationKind shows MemStats.Mallocs
// counting each NOSCAN TINY object (< 16 bytes, pointer-free) individually while
// the memory profile records only the shared 16-byte tiny block it was carved
// from. Measured object ratio: exactly 0.5000 for an all-8-byte workload, exactly
// 1.0000 for 64-byte and for map workloads, with the byte ratio at 1.0070..1.0358
// throughout. A real window therefore lands in [0.5, 1.0] according to how much
// of it is tiny, and only a value outside that band indicates a broken
// instrument.
func (at *attribution) assertDescribesWindow(tb testing.TB, what string) {
	tb.Helper()
	if at.rate != 1 {
		return // sampled window: the totals are not comparable by construction
	}
	byteRatio := float64(at.totalBytes) / float64(at.windowBytes)
	if byteRatio < 0.95 || byteRatio > 1.10 {
		tb.Errorf("%s: profile total %d alloc BYTES disagrees with the TotalAlloc delta %d over "+
			"the same window (ratio %.4f, want 0.95..1.10). Bytes are the exact axis, so the "+
			"attribution does not describe this window and no share taken from it is trustworthy.",
			what, at.totalBytes, at.windowBytes, byteRatio)
	}
	objRatio := float64(at.totalObjects) / float64(at.windowMallocs)
	if objRatio < 0.45 || objRatio > 1.10 {
		tb.Errorf("%s: profile total %d alloc OBJECTS is outside the measured tiny-allocation "+
			"band relative to the Mallocs delta %d (ratio %.4f, want 0.45..1.10)",
			what, at.totalObjects, at.windowMallocs, objRatio)
	}
}

// cum looks a frame up by suffix, so the short constants above match the
// fully-qualified names the runtime reports.
func (at *attribution) cum(suffix string) (objects, bytes int64) {
	for fn, v := range at.cumObjects {
		if hasFrameSuffix(fn, suffix) {
			objects += v
			bytes += at.cumBytes[fn]
		}
	}
	return objects, bytes
}

func hasFrameSuffix(fn, suffix string) bool {
	return len(fn) >= len(suffix) && fn[len(fn)-len(suffix):] == suffix
}

// The frames that distinguish the two execution paths. Neither is reachable on
// the other arm, so presence/absence is a proof, not a correlation.
const (
	frameSortLegacy     = "cypher/exec.(*Sort).rowLess"
	frameSortDecorated  = "cypher/exec.(*Sort).sortDecorated"
	frameTopLegacy      = "cypher/exec.rowLessForKeys"
	frameTopDecorated   = "cypher/exec.(*Top).consumeAndFinish"
	frameCollectAndSort = "cypher/exec.(*Sort).collectAndSort"
	frameSortKeyValue   = "cypher/exec.sortKeyValue"
)
