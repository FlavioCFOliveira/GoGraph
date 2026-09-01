//go:build soak || nightly

package audit352_test

// sortprofile_soak_test.go — the EXACT allocation-attribution sweep for #2652.
//
// # Why this file is soak-layer (rmp #2652)
//
// Every test here drives the instrument in allocattrib_test.go at
// runtime.MemProfileRate = 1, which takes a stack walk on EVERY allocation, over
// graphs of up to 256 000 nodes, on BOTH arms of the seam. That is the whole
// point — it is what makes the frame assertions exact rather than sampled — and
// it is also why it does not belong in the short layer. Measured on the reference
// host (Apple M4, 10 cores, darwin/arm64, go1.27.0), without -race:
//
//	TestSortDecorationArmFrames      119.1 s   (17 cells x 2 arms)
//	TestSortShapeAllocProfile         77.8 s   (4 shapes x 2 arms)
//	TestSortDecorationArmSignatures   17.2 s   (18 cells x 2 arms)
//
// Left in the short layer they took bench/audit352 from 70 s to 565 s, past the
// 240 s hard ceiling that scripts/pkg_time_budget.sh fails `make ci` on — and
// the budget is enforced UNDER -race, which is stricter still. Gated here, the
// short layer measures 72.4 s. See docs/test-layers.md.
//
// What stays in the short layer is the instrument's own correctness:
// TestAllocAttributionAgreesWithMallocs, TestAllocInstrumentDoesNotEnterItsOwnWindow
// and TestAllocProfileVsMallocsByAllocationKind are cheap and are genuine
// correctness gates — a broken instrument must fail fast, whereas the sweep it
// drives is a measurement.
//
// Run it with:
//
//	go test -tags=soak ./bench/audit352/ -run 'TestSortDecoration|TestSortShapeAlloc' -v
//
// or by setting SOAK_FULL=1 for the helpers that read the layer at runtime.

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
)

// TestSortDecorationArmFrames is the structural arm assertion for EVERY cell of
// the A/B sweep, including the n=2 and n=8 convergence cells where no allocation
// volume can tell the arms apart.
//
// It is NOT t.Parallel: it flips a process-global control and a process-global
// profile rate.
func TestSortDecorationArmFrames(t *testing.T) {
	type cell struct {
		shape    string
		n        int
		query    string
		wantRows int
		legacyFr string
		decorFr  string
	}
	cells := make([]cell, 0, len(sortABSizes)+2*len(topABSizes))
	for _, n := range sortABSizes {
		want := 10
		if n < 10 {
			want = n
		}
		cells = append(cells, cell{"sort", n, sortShapeQuery, want, frameSortLegacy, frameSortDecorated})
	}
	for _, n := range topABSizes {
		cells = append(cells,
			cell{"top/limit=10", n, topShapeQuery(10), 10, frameTopLegacy, frameTopDecorated},
			cell{"top/limit=full", n, topShapeQuery(n), n, frameTopLegacy, frameTopDecorated},
		)
	}

	for _, c := range cells {
		name := fmt.Sprintf("%s/n=%d", c.shape, c.n)
		t.Run(name, func(t *testing.T) {
			eng := sortShapeEngine(t, c.n)
			// Warm the plan cache on both arms OUTSIDE the measured window, so
			// one-off compilation cannot appear as a path difference.
			for _, d := range []bool{true, false} {
				restore := sortseam.SetKeyDecorationDisabled(d)
				if got := drainCounting(t, eng, c.query); got != c.wantRows {
					t.Fatalf("warm-up shipped %d rows, want %d", got, c.wantRows)
				}
				restore()
			}

			rate := profileRateFor(c.n)
			measure := func(disabled bool) attribution {
				restore := sortseam.SetKeyDecorationDisabled(disabled)
				defer restore()
				return exerciseAttributed(t, rate, func() {
					if got := drainCounting(t, eng, c.query); got != c.wantRows {
						t.Fatalf("shipped %d rows, want %d", got, c.wantRows)
					}
				})
			}

			legacy := measure(true)
			decorated := measure(false)

			legObjLegacyFrame, _ := legacy.cum(c.legacyFr)
			decObjLegacyFrame, _ := decorated.cum(c.legacyFr)
			legObjDecorFrame, _ := legacy.cum(c.decorFr)
			decObjDecorFrame, _ := decorated.cum(c.decorFr)

			legacy.assertDescribesWindow(t, "legacy window")
			decorated.assertDescribesWindow(t, "decorated window")
			t.Logf("rate=1/%d  total alloc objects: legacy=%d decorated=%d (%.2fx)  "+
				"mallocs: legacy=%d decorated=%d (%.2fx)",
				rate, legacy.totalObjects, decorated.totalObjects,
				ratio(legacy.totalObjects, decorated.totalObjects),
				legacy.windowMallocs, decorated.windowMallocs,
				ratio(int64(legacy.windowMallocs), int64(decorated.windowMallocs)))
			t.Logf("  %-44s legacy=%d decorated=%d", shortFn(c.legacyFr)+" [cum objs]",
				legObjLegacyFrame, decObjLegacyFrame)
			t.Logf("  %-44s legacy=%d decorated=%d", shortFn(c.decorFr)+" [cum objs]",
				legObjDecorFrame, decObjDecorFrame)

			// ── The assertions ──────────────────────────────────────────────
			// 1. The legacy arm really took the legacy comparator.
			if legObjLegacyFrame == 0 {
				t.Errorf("legacy arm allocated NOTHING beneath %s: the seam did not select "+
					"the legacy path, so this cell's A/B is the decorated path against itself",
					shortFn(c.legacyFr))
			}
			// 2. The decorated arm did NOT take it. This is the assertion that
			//    makes a mislabelled arm impossible.
			if decObjLegacyFrame != 0 {
				t.Errorf("decorated arm allocated %d objects beneath %s: it is (at least "+
					"partly) running the LEGACY comparator", decObjLegacyFrame, shortFn(c.legacyFr))
			}
			// 3. The decorated arm really took the decorated path.
			if decObjDecorFrame == 0 {
				t.Errorf("decorated arm allocated nothing beneath %s: it did not take the "+
					"decorated path either, so neither arm is identified",
					shortFn(c.decorFr))
			}
			// 4. Both arms evaluated sort keys at all. Without this a shape whose
			//    ORDER BY key resolved by schema lookup (ColIdx, no evaluator)
			//    would pass everything above while exercising nothing.
			legKeyObj, _ := legacy.cum(frameSortKeyValue)
			decKeyObj, _ := decorated.cum(frameSortKeyValue)
			if legKeyObj == 0 || decKeyObj == 0 {
				t.Errorf("sortKeyValue allocated nothing (legacy=%d decorated=%d): the ORDER BY "+
					"key is not evaluator-backed, so this cell measures nothing",
					legKeyObj, decKeyObj)
			}
		})
	}
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

// ─────────────────────────────────────────────────────────────────────────────
// The re-taken allocation profile
// ─────────────────────────────────────────────────────────────────────────────

// profileShapes are the shapes whose allocation profile the audit's ranked queue
// depends on. All of them run over the SHARED 120 000-node fixture with ~960 000
// edges, which is the graph the audit's original profile was taken on, so the
// shares reported here and the shares reported there describe the same workload.
var profileShapes = []struct {
	name     string
	query    string
	wantRows int
}{
	// The audit's reproduction, verbatim: bench/audit352/rowcost_test.go
	// paginationShapes "skip0_limit10". SKIP 0 blocks Top fusion.
	{"sort_skip0_limit10", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0 LIMIT 10`, 10},
	// Top, small limit — the fused shape.
	{"top_limit10", `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary LIMIT 10`, 10},
	// Top, full-width limit — the half whose amplification (33.7x) exceeded
	// Sort's (19.0x).
	{"top_limit_full", fmt.Sprintf(`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary LIMIT %d`, nodeCount), nodeCount},
	// A sort-free control over the same scan, so the report can separate what
	// the SORT costs from what the SCAN + PROJECT pipeline costs. Whatever tops
	// this profile is what tops the sort shape's profile once the sort's own
	// share is gone.
	{"scan_project_only", `MATCH (p:Person) RETURN p.firstName`, nodeCount},
}

// TestSortShapeAllocProfile re-takes the allocation profile that #2652's
// acceptance criterion asks for, on BOTH arms, and prints the flat and cumulative
// attribution of every window.
//
// The window is bracketed and the rate is 1, so the shares are exact and contain
// nothing but the query: TestMain's fixture construction (120 000 nodes,
// ~960 000 edges) is outside the window and cannot be mistaken for a query cost,
// which is what a whole-process `go test -memprofile` would do on the fixed build.
//
// It is NOT t.Parallel: it flips a process-global control and profile rate.
func TestSortShapeAllocProfile(t *testing.T) {
	eng := cypher.NewEngine(benchGraph)

	// Rate 1: EXACT, unsampled shares. A coarser rate samples per byte and so
	// biases object counts towards large allocations, which would corrupt exactly
	// the alloc_objects ranking this test exists to produce.
	const rate = 1

	for _, s := range profileShapes {
		s := s
		t.Run(s.name, func(t *testing.T) {
			plan, err := eng.Explain(s.query, nil)
			if err != nil {
				t.Fatalf("Explain: %v", err)
			}
			t.Logf("PLAN:\n%s", plan)

			for _, arm := range []struct {
				name     string
				disabled bool
			}{{"decorated", false}, {"legacy", true}} {
				restore := sortseam.SetKeyDecorationDisabled(arm.disabled)
				// Warm outside the window.
				if got := drainCounting(t, eng, s.query); got != s.wantRows {
					t.Fatalf("warm-up shipped %d rows, want %d", got, s.wantRows)
				}
				at := exerciseAttributed(t, rate, func() {
					if got := drainCounting(t, eng, s.query); got != s.wantRows {
						t.Fatalf("shipped %d rows, want %d", got, s.wantRows)
					}
				})
				restore()
				at.assertDescribesWindow(t, s.name+"/"+arm.name)

				t.Logf("──── %s / arm=%s ── one query, %d rows ── total %d alloc objects, %d alloc bytes "+
					"(rate 1/%d, Mallocs delta %d)",
					s.name, arm.name, s.wantRows, at.totalObjects, at.totalBytes, rate, at.windowMallocs)
				t.Logf("  window oracle: %s", at.windowRatios())
				t.Logf("  FLAT by alloc_objects, allocator plumbing stripped (top 15):\n%s",
					topN(at.flatObjects, at.totalObjects, 15))
				t.Logf("  FLAT by alloc_objects, first NON-runtime frame (top 15):\n%s",
					topN(at.flatGoObjects, at.totalObjects, 15))
				t.Logf("  CUM  by alloc_objects (top 20):\n%s", topN(at.cumObjects, at.totalObjects, 20))
				t.Logf("  FLAT by alloc_bytes, plumbing stripped (top 12):\n%s",
					topN(at.flatBytes, at.totalBytes, 12))
				t.Logf("  LARGEST STACKS by alloc_objects (top 8, 10 frames each):\n%s",
					topSites(&at, 8, 10))

				csObj, csBytes := at.cum(frameCollectAndSort)
				t.Logf("  Sort.collectAndSort cumulative: %d objects (%.2f%%), %d bytes (%.2f%%)",
					csObj, pct(csObj, at.totalObjects), csBytes, pct(csBytes, at.totalBytes))
			}
		})
	}
}

func pct(v, total int64) float64 {
	if total == 0 {
		return 0
	}
	return 100 * float64(v) / float64(total)
}

// TestSortDecorationArmSignatures prints the mallocs-per-query signature of both
// arms at every A/B cell. The benchmark asserts these; this test is where the
// numbers behind the assertion are recorded, so a reader can see the margin
// rather than trusting a threshold.
func TestSortDecorationArmSignatures(t *testing.T) {
	for _, n := range sortABSizes {
		want := 10
		if n < 10 {
			want = n
		}
		eng := sortShapeEngine(t, n)
		// Sort retains every row in its comparator, so heapRows == n.
		l, d := assertArm(t, eng, sortShapeQuery, n, n, want)
		t.Logf("shape=sort   n=%-7d legacy=%-12d decorated=%-10d ratio=%.3f surplus=%-10d "+
			"(assert surplus >= %d)",
			n, l, d, ratio(int64(l), int64(d)), int64(l)-int64(d), armSignatureMargin(n, n))
	}
	for _, n := range topABSizes {
		eng := sortShapeEngine(t, n)
		for _, lim := range []struct {
			name  string
			limit int
		}{{"10", 10}, {"full", n}} {
			q := topShapeQuery(lim.limit)
			// Top's comparator retains only the LIMIT, which is what makes the
			// margin reachable here; see [armSignatureMargin].
			l, d := assertArm(t, eng, q, n, lim.limit, lim.limit)
			t.Logf("shape=top    n=%-7d limit=%-6s legacy=%-12d decorated=%-10d ratio=%.3f "+
				"surplus=%-10d (assert surplus >= %d)",
				n, lim.name, l, d, ratio(int64(l), int64(d)), int64(l)-int64(d),
				armSignatureMargin(n, lim.limit))
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Arm signature — the mallocs-per-query oracle
// ─────────────────────────────────────────────────────────────────────────────
//
// These live here, with their only caller, rather than beside the benchmarks:
// the benchmarks assert their arm STRUCTURALLY (assertArmFramesForCell), and a
// helper with no caller in the default build is a lint finding waiting to happen.

// minSignatureRows is the smallest input size at which the two arms provably do
// DIFFERENT amounts of key evaluation.
//
// At n=2 sort.SliceStable performs exactly one comparison, so the legacy arm
// evaluates the key twice and the decorated arm — one evaluation per row — also
// twice. The difference is provably ZERO, and the measured cell confirms it
// (legacy 106 mallocs, decorated 107). No margin is honest there, so the cell is
// asserted structurally by TestSortDecorationArmFrames instead.
const minSignatureRows = 3

// armSignatureMargin returns the minimum number of EXTRA mallocs-per-query the
// legacy arm must show over the decorated arm, for a cell of `rows` input rows
// whose comparator retains `heapRows` of them (rows for Sort, the LIMIT for Top).
// It returns 0 when no margin is derivable and the cell must be asserted
// structurally instead.
//
// # Why a margin and not a ratio (rmp #2652)
//
// This used to be a RATIO floor of 2.0, derived from the Sort shape alone: the
// legacy arm performs ~2*C key evaluations for C = n*log2(n) comparisons against
// the decorated arm's n, so the evaluation amplification grows like 2*log2(n).
// That derivation does not transfer to Top, and applying it there made the
// assertion unreachable by construction: Top's comparator holds only `heapRows`
// entries, so once its heap is full each further input row costs ONE admission
// comparison and only the few rows that are actually admitted sift. For a limit
// of 10 the amplification is therefore ~2x on the evaluations, and the
// WHOLE-QUERY malloc ratio it produces is ~1.15 — measured 1.20 at n=1000 and
// 1.13 at n=256 000, against a demanded 2.0. The cell could never pass.
//
// The ratio was the wrong quantity. It divides by the query's total allocation,
// which is dominated by the Theta(rows) scan/project/row-copy work that BOTH arms
// pay, so it depends on a constant that has nothing to do with the seam. The
// DIFFERENCE does not:
//
//	legacyMallocs - decoratedMallocs = (legacyEvals - decoratedEvals) * objectsPerEval
//
// Both factors are properties of the shape. The decorated arm evaluates each key
// exactly once per input row. The legacy arm evaluates BOTH operands of every
// comparison, and every shape performs at least `rows` comparisons — Sort's
// stable sort ~rows*log2(rows), Top one admission test per row once its heap is
// full — so the legacy arm performs AT LEAST `rows` more evaluations. Each
// evaluation of an evaluator-backed key builds a fresh expr.RowContext map and a
// fresh schema walk.
//
// objectsPerEval is MEASURED, not assumed, and it is exactly 2. On the Sort shape
// the in-package counter gives both evaluation counts, so the quotient can be
// formed directly:
//
//	n=1000   mallocs 63192-14837 = 48355   evals 25178-1000  =  24178   -> 2.0000
//	n=4000   mallocs 299087-59841 = 239246 evals 123626-4000 = 119626   -> 2.0000
//
// So the derived floor is 2*rows extra mallocs. This returns `rows`, i.e. HALF
// the derived value, so the assertion carries a 2x margin against the model
// rather than sitting on it. Every cell of the sweep clears it by at least 2x —
// the tightest are the Top limit=10 cells, whose measured surplus converges to
// exactly 2*rows from above (2.97x rows at n=1000, 2.03x at n=64 000, 2.007x at
// n=256 000), which is itself the model being confirmed.
func armSignatureMargin(rows, heapRows int) int64 {
	if rows < minSignatureRows || heapRows <= 0 {
		return 0 // provably no difference, or a limit that admits nothing
	}
	return int64(rows)
}

// ─────────────────────────────────────────────────────────────────────────────

// assertArm is the per-cell arm assertion. It probes BOTH arms and requires the
// legacy one to allocate at least armSignatureMargin(n, heapRows) MORE than the
// decorated one. A seam that stopped working makes the two probes equal and this
// fails.
//
// heapRows is the number of rows the comparator retains: n for Sort, the LIMIT
// for Top. It is what makes the assertion reachable on both shapes; see
// [armSignatureMargin].
//
// It returns the two probe values so a caller can report them.
func assertArm(tb testing.TB, eng *cypher.Engine, query string, n, heapRows, wantRows int) (legacy, decorated uint64) {
	tb.Helper()
	legacy = mallocsPerQuery(tb, eng, query, true, wantRows)
	decorated = mallocsPerQuery(tb, eng, query, false, wantRows)
	want := armSignatureMargin(n, heapRows)
	if want == 0 {
		return legacy, decorated
	}
	surplus := int64(legacy) - int64(decorated)
	if surplus < want {
		tb.Fatalf("ARM ASSERTION FAILED for %q at n=%d (heap %d): legacy probe allocated %d, "+
			"decorated probe %d, surplus %d, want >= %d extra mallocs. The seam did not "+
			"select a different execution, so this cell would be measuring the same path twice.",
			query, n, heapRows, legacy, decorated, surplus, want)
	}
	return legacy, decorated
}

// ─────────────────────────────────────────────────────────────────────────────
// Reporting and probe helpers used only by this sweep
// ─────────────────────────────────────────────────────────────────────────────
//
// They live behind the build tag with their only callers. Left in the default
// build they would have no caller at all and the `unused` linter would — quite
// correctly — flag them.

// topN renders the n largest entries of m as a table with percentage shares.
func topN(m map[string]int64, total int64, n int) string {
	type kv struct {
		k string
		v int64
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].v != all[j].v {
			return all[i].v > all[j].v
		}
		return all[i].k < all[j].k
	})
	if len(all) > n {
		all = all[:n]
	}
	var sb strings.Builder
	for _, e := range all {
		pct := 0.0
		if total > 0 {
			pct = 100 * float64(e.v) / float64(total)
		}
		fmt.Fprintf(&sb, "    %8.2f%%  %14d  %s\n", pct, e.v, shortFn(e.k))
	}
	return sb.String()
}

// windowRatios renders both oracle ratios, for the report.
func (at *attribution) windowRatios() string {
	return fmt.Sprintf("obj %d/%d=%.4f  bytes %d/%d=%.4f",
		at.totalObjects, at.windowMallocs, float64(at.totalObjects)/float64(at.windowMallocs),
		at.totalBytes, at.windowBytes, float64(at.totalBytes)/float64(at.windowBytes))
}

// mallocsPerQuery runs the query once on the given arm and returns the process
// malloc delta.
//
// Mallocs is PROCESS-GLOBAL. It is attributable here only because benchmarks run
// sequentially and this harness starts no goroutine of its own; the runtime's own
// background allocation is orders of magnitude below the signal (tens of
// thousands of allocations against millions). It is used ONLY as a signature, to
// tell one arm from the other, never as a reported measurement — the reported
// allocs/op come from the benchmark's own accounting.
func mallocsPerQuery(tb testing.TB, eng *cypher.Engine, query string, disabled bool, wantRows int) uint64 {
	tb.Helper()
	restore := sortseam.SetKeyDecorationDisabled(disabled)
	defer restore()

	// One untimed warm-up so plan caching and pool warm-up are not in the delta.
	if got := drainCounting(tb, eng, query); got != wantRows {
		tb.Fatalf("warm-up shipped %d rows, want %d", got, wantRows)
	}

	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	if got := drainCounting(tb, eng, query); got != wantRows {
		tb.Fatalf("probe shipped %d rows, want %d", got, wantRows)
	}
	runtime.ReadMemStats(&b)
	return b.Mallocs - a.Mallocs
}
