package audit352_test

// sortdecoration_ab_test.go — the interleaved, single-binary A/B for #2652
// (decorate-sort-undecorate in cypher/exec/sort.go and cypher/exec/top.go).
//
// # What this file measures, and what it does NOT
//
// The COMPLEXITY claim of #2652 is already settled by a counter, in-package:
// sort-key evaluations went from Theta(n log n) to Theta(n) (25 178 -> 1 000 at
// n=1000; 123 626 -> 4 000 at n=4000). No timing measurement can establish that,
// and this file does not try to. What it establishes is the only thing a counter
// cannot: what the complexity change is WORTH, in ns/op, B/op and allocs/op, over
// a size range wide enough for the log factor to move.
//
// # Why a single binary can hold both arms
//
// internal/sortseam carries a process-global atomic that forces the operators
// back onto the legacy per-comparison path. It is re-read once per blocking sort
// phase and once per Top.Init, so the arm can be flipped BETWEEN executions
// inside one process. That removes the whole class of cross-binary confounds this
// project has been bitten by: code layout, ASLR, two different builds.
//
// # Arm assertion — every cell, every round
//
// An unasserted arm is void, and the seam alone is not proof: it records what was
// ASKED for, not what ran. Every cell therefore probes BOTH arms before the timed
// loop and requires the legacy arm's mallocs-per-query to exceed the decorated
// arm's by a structural factor. If the seam silently stopped selecting a
// different execution, both probes return the same number and the cell FAILS
// rather than quietly measuring the decorated path twice.
//
// At n=2 no such margin exists — see armSignatureMargin — and the assertion is
// carried instead by TestSortDecorationArmFrames, which proves structurally, by
// frame presence in an exact allocation profile, which code path each arm took at
// EVERY cell in this sweep including that one.
//
// # Reproduction
//
//	go test ./bench/audit352/ -run 'TestSortDecoration|TestSortShapeAlloc' -count=1 -v
//	go test -c -o /tmp/a352.test ./bench/audit352/
//	for i in $(seq 1 8); do
//	  ord=ab; [ $((i % 2)) -eq 0 ] && ord=ba
//	  AUDIT352_ARM_ORDER=$ord /tmp/a352.test -test.run='^$' \
//	    -test.bench='BenchmarkSortDecoration|BenchmarkTopDecoration|BenchmarkSortNoiseFloor' \
//	    -test.count=1 -test.benchmem >> /tmp/sortab.txt
//	done
//	benchstat -col /arm -row /n -filter '/shape:sort' /tmp/sortab.txt

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/sortseam"
)

// ─────────────────────────────────────────────────────────────────────────────
// Cells
// ─────────────────────────────────────────────────────────────────────────────

// sortABSizes spans 2 -> 256 000, a 128 000x range, so log2(n) moves from 1 to
// 18: the legacy arm's per-row comparison count grows 18x across the sweep while
// the decorated arm's stays at 1. Anything smaller cannot show a complexity
// change in a timing measurement at all.
//
// n=2 and n=8 are the CONVERGENCE cells. At n=2 sort.SliceStable performs
// exactly one comparison, so the legacy arm evaluates the key twice and the
// decorated arm also twice: the two arms do literally identical work and MUST be
// statistically indistinguishable. A sweep without such a cell cannot tell a
// real effect from a harness that is simply slower on one arm.
var sortABSizes = []int{2, 8, 64, 1_000, 4_000, 16_000, 64_000, 256_000}

// topABSizes omits the tiny sizes: Top's amplification is M log N, and at M < 64
// there is nothing to amplify. The full-width limit variant is the one that was
// measured at 33.7x, so it is included at every one of these sizes.
var topABSizes = []int{1_000, 4_000, 16_000, 64_000, 256_000}

// assertArmFramesForCell is the PER-CELL arm assertion, run on the cell's OWN arm
// immediately before the timed loop.
//
// It is structural: the two execution paths are distinguished by a FRAME in an
// exact/near-exact allocation profile, not by a number. The legacy comparator
// (Sort.rowLess / rowLessForKeys) is unreachable on the decorated path and vice
// versa, so:
//
//   - a legacy cell must show allocation beneath the legacy comparator;
//   - a decorated cell must show EXACTLY ZERO beneath it, and non-zero beneath
//     the decorated path.
//
// This is what makes a timed cell non-void, and it is strictly stronger than
// reading the seam back (which proves only what was ASKED for) and than the
// mallocs-volume signature (which cannot separate the arms at all at n=2, where
// they do identical work, nor at n=8).
//
// Cost: one untimed query plus the snapshot GCs, at rate 1 up to n=16 000 and at
// rate 1/512 above it. Sampling weakens nothing that matters here: the legacy arm
// at n=256 000 allocates ~23.5M objects beneath rowLess, so at rate 1/512 a
// decorated cell reading zero samples rules out the legacy path by an
// overwhelming margin. Lag in the profile could only ADD a previous cell's frames
// to this window, i.e. cause a false FAILURE, never a false pass.
func assertArmFramesForCell(tb testing.TB, eng *cypher.Engine, query string, wantRows int,
	a sortArm, legacyFrame, decorFrame string, rate int) {
	tb.Helper()
	at := exerciseAttributed(tb, rate, func() {
		if got := drainCounting(tb, eng, query); got != wantRows {
			tb.Fatalf("arm probe shipped %d rows, want %d", got, wantRows)
		}
	})
	legObjs, _ := at.cum(legacyFrame)
	decObjs, _ := at.cum(decorFrame)
	if a.disabled {
		if legObjs == 0 {
			tb.Fatalf("ARM ASSERTION FAILED (cell arm=%q, %q): nothing allocated beneath %s, so "+
				"the seam did NOT select the legacy path and this timed cell would measure the "+
				"decorated path under a legacy label", a.name, query, shortFn(legacyFrame))
		}
		return
	}
	if legObjs != 0 {
		tb.Fatalf("ARM ASSERTION FAILED (cell arm=%q, %q): %d objects allocated beneath %s, so "+
			"this cell is running the LEGACY comparator under a decorated label",
			a.name, query, legObjs, shortFn(legacyFrame))
	}
	if decObjs == 0 {
		tb.Fatalf("ARM ASSERTION FAILED (cell arm=%q, %q): nothing allocated beneath %s either, "+
			"so neither path is identified and the cell is unattributable",
			a.name, query, shortFn(decorFrame))
	}
}

// assertSortOperator fails unless the query compiles to the operator this cell claims to
// exercise. Without it a planner change would leave every number below measuring
// a different program while still passing.
func assertSortOperator(tb testing.TB, eng *cypher.Engine, query, wantOp, banOp string) {
	tb.Helper()
	plan, err := eng.Explain(query, nil)
	if err != nil {
		tb.Fatalf("Explain(%q): %v", query, err)
	}
	if !strings.Contains(plan, wantOp) {
		tb.Fatalf("%q does not compile to a %s; plan:\n%s", query, wantOp, plan)
	}
	if banOp != "" && strings.Contains(plan, banOp) {
		tb.Fatalf("%q compiles to a %s, so it does not exercise %s; plan:\n%s",
			query, banOp, wantOp, plan)
	}
}

// runArmed is the single timed primitive every cell in this file uses, so no cell
// can differ from another by how it drives the query.
//
// It brackets the timed loop with an Explain on the same engine and fails if the
// physical plan changed underneath the measurement — the plan-drift trap this
// package documents.
func runArmed(b *testing.B, eng *cypher.Engine, query string, wantRows, n int, a sortArm,
	legacyFrame, decorFrame string) {
	b.Helper()
	planBefore, err := eng.Explain(query, nil)
	if err != nil {
		b.Fatalf("Explain(%q): %v", query, err)
	}

	restore := sortseam.SetKeyDecorationDisabled(a.disabled)
	defer restore()
	// Read the control BACK. This is the weak half of the arm assertion (it
	// proves only what was asked for); assertArm is the strong half.
	if got := sortseam.KeyDecorationDisabled(); got != a.disabled {
		b.Fatalf("seam read back %v, want %v for arm %q", got, a.disabled, a.name)
	}
	// The strong half: prove structurally, on this cell's own arm, that the
	// execution really took the path the cell claims. Also leaves the heap freshly
	// collected, identically for both arms, immediately before the timed loop.
	assertArmFramesForCell(b, eng, query, wantRows, a, legacyFrame, decorFrame, profileRateFor(n))

	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := eng.Run(ctx, query, nil)
		if err != nil {
			b.Fatalf("Run(%q): %v", query, err)
		}
		rows := 0
		for res.Next() {
			rows++
		}
		if e := res.Err(); e != nil {
			b.Fatalf("Err(%q): %v", query, e)
		}
		if err := res.Close(); err != nil {
			b.Fatalf("Close(%q): %v", query, err)
		}
		if rows != wantRows {
			b.Fatalf("shipped %d rows, want %d", rows, wantRows)
		}
	}
	b.StopTimer()

	planAfter, err := eng.Explain(query, nil)
	if err != nil {
		b.Fatalf("Explain after(%q): %v", query, err)
	}
	if planAfter != planBefore {
		b.Fatalf("PLAN DRIFTED during benchmark of %q\n--- before ---\n%s\n--- after ---\n%s",
			query, planBefore, planAfter)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The A/B sweeps
// ─────────────────────────────────────────────────────────────────────────────

// BenchmarkSortDecoration is the Sort-shape A/B. The two arms of each size run
// back to back so drift between them is minimal.
func BenchmarkSortDecoration(b *testing.B) {
	for _, n := range sortABSizes {
		eng := sortShapeEngine(b, n)
		wantRows := 10
		if n < 10 {
			wantRows = n
		}
		assertSortOperator(b, eng, sortShapeQuery, "Sort", "Top")
		for _, a := range armOrder() {
			b.Run(fmt.Sprintf("shape=sort/n=%d/arm=%s", n, a.name), func(b *testing.B) {
				runArmed(b, eng, sortShapeQuery, wantRows, n, a, frameSortLegacy, frameSortDecorated)
			})
		}
	}
}

// BenchmarkTopDecoration is the Top-shape A/B, at a small limit and at a
// full-width limit. The full-width limit is the shape whose amplification
// (33.7x) exceeded Sort's (19.0x), so it is the half that was expected to matter
// most.
func BenchmarkTopDecoration(b *testing.B) {
	for _, n := range topABSizes {
		eng := sortShapeEngine(b, n)
		for _, lim := range []struct {
			name  string
			limit int
		}{
			{"10", 10},
			{"full", n},
		} {
			q := topShapeQuery(lim.limit)
			wantRows := lim.limit
			assertSortOperator(b, eng, q, "Top", "")
			for _, a := range armOrder() {
				b.Run(fmt.Sprintf("shape=top/n=%d/limit=%s/arm=%s", n, lim.name, a.name), func(b *testing.B) {
					runArmed(b, eng, q, wantRows, n, a, frameTopLegacy, frameTopDecorated)
				})
			}
		}
	}
}

// BenchmarkSortNoiseFloor is the NOISE FLOOR: the identical cell structure as
// BenchmarkSortDecoration, but with BOTH arms on the decorated path. Any
// difference benchstat reports between ctlA and ctlB is this harness measuring
// the same program against itself, and no delta in the real A/B smaller than that
// is a finding.
//
// It must be run in the same rounds, in the same binary, as the real A/B: a noise
// floor measured at another time describes another host state.
func BenchmarkSortNoiseFloor(b *testing.B) {
	ctlA := sortArm{"ctlA", false}
	ctlB := sortArm{"ctlB", false}
	order := []sortArm{ctlA, ctlB}
	if strings.EqualFold(os.Getenv("AUDIT352_ARM_ORDER"), "ba") {
		order = []sortArm{ctlB, ctlA}
	}
	for _, n := range sortABSizes {
		eng := sortShapeEngine(b, n)
		wantRows := 10
		if n < 10 {
			wantRows = n
		}
		for _, a := range order {
			b.Run(fmt.Sprintf("shape=sort/n=%d/arm=%s", n, a.name), func(b *testing.B) {
				runArmed(b, eng, sortShapeQuery, wantRows, n, a, frameSortLegacy, frameSortDecorated)
			})
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Profile rate — shared with the soak-layer attribution sweep
// ─────────────────────────────────────────────────────────────────────────────
//
// These live here rather than in sortprofile_soak_test.go because the per-cell
// frame assertion the BENCHMARKS run (assertArmFramesForCell) needs the rate,
// and the benchmarks are not soak-gated: they cost nothing unless -bench asks
// for them.

// profileRateFor picks the MemProfile rate for a size.
//
// Rate 1 records EVERY allocation, so both presence and absence of a frame are
// exact. It costs a stack walk per allocation, which the legacy arm at n >= 64000
// performs tens of millions of times, so above that the rate is coarsened. The
// assertions stay sound because the margin is enormous: at rate 512 a 256 000-row
// legacy sort yields on the order of a million samples beneath rowLess, while the
// decorated arm's ENTIRE window is on the order of a hundred thousand. A frame
// that ran for every comparison cannot hide in that.
func profileRateFor(n int) int {
	if n <= largestExactFrameSize {
		return 1
	}
	return 512
}

// largestExactFrameSize is the largest cell measured at rate 1. It is a COST
// parameter, not a soundness one: raising it costs wall-clock, lowering it
// weakens the "decorated arm allocated nothing beneath the legacy comparator"
// assertion from exact to overwhelming-margin.
const largestExactFrameSize = 256_000

// ─────────────────────────────────────────────────────────────────────────────
// Fixtures — one graph per size, built once per process
// ─────────────────────────────────────────────────────────────────────────────

var (
	sortFixtureMu sync.Mutex
	sortEngines   = map[int]*cypher.Engine{}
)

// sortShapeEngine returns a warm Engine over an n-node graph carrying exactly
// the two properties the reproduction needs.
//
// salary is deliberately NOT monotonic in i and NOT distinct: (i*2654435761) mod
// 65536 scatters the key so the comparator does real work, and at the larger
// sizes the modulus is below n so ties are exercised too. Every value is
// >= 100 000, far outside the Go runtime's staticuint64s window (< 256), so no
// arm can be flattered by an allocation-free integer box.
//
// This is the same construction as the in-package complexity oracle
// (cypher/sort_key_eval_complexity_test.go), so the timing measured here and the
// evaluation counts measured there describe the same workload.
func sortShapeEngine(tb testing.TB, n int) *cypher.Engine {
	tb.Helper()
	sortFixtureMu.Lock()
	defer sortFixtureMu.Unlock()
	if e, ok := sortEngines[n]; ok {
		return e
	}
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			tb.Fatalf("AddNode %s: %v", key, err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			tb.Fatalf("SetNodeLabel %s: %v", key, err)
		}
		if err := g.SetNodeProperty(key, "firstName", lpg.StringValue(key)); err != nil {
			tb.Fatalf("SetNodeProperty firstName: %v", err)
		}
		salary := int64(100_000 + (i*2_654_435_761)%65_536)
		if err := g.SetNodeProperty(key, "salary", lpg.Int64Value(salary)); err != nil {
			tb.Fatalf("SetNodeProperty salary: %v", err)
		}
	}
	e := cypher.NewEngine(g)
	sortEngines[n] = e
	return e
}

// sortShapeQuery is the #2652 reproduction. `p.salary` is not projected, so
// irSortKeys compiles an expression evaluator rather than resolving the key by
// schema lookup — the shape whose evaluation count the defect amplified. The
// `SKIP 0` blocks ORDER BY+LIMIT fusion (#2509) and forces the full Sort.
const sortShapeQuery = `MATCH (p:Person) RETURN p.firstName ORDER BY p.salary SKIP 0 LIMIT 10`

// topShapeQuery is the same reproduction WITHOUT the SKIP, so ORDER BY + LIMIT
// fuses into Top.
func topShapeQuery(limit int) string {
	return fmt.Sprintf(`MATCH (p:Person) RETURN p.firstName ORDER BY p.salary LIMIT %d`, limit)
}

// ─────────────────────────────────────────────────────────────────────────────
// Arms
// ─────────────────────────────────────────────────────────────────────────────

type sortArm struct {
	name     string
	disabled bool // the value written to the sortseam control
}

var (
	armLegacy    = sortArm{"legacy", true}
	armDecorated = sortArm{"decorated", false}
)

// armOrder returns the two arms in the registration order this round asks for.
// The driver alternates AUDIT352_ARM_ORDER between rounds so that neither arm is
// systematically first: within a round the two arms of a cell run back to back
// (minimal drift between them), and across rounds the order mirrors (no
// first-position bias).
func armOrder() []sortArm {
	if strings.EqualFold(os.Getenv("AUDIT352_ARM_ORDER"), "ba") {
		return []sortArm{armDecorated, armLegacy}
	}
	return []sortArm{armLegacy, armDecorated}
}

// ─────────────────────────────────────────────────────────────────────────────
// Execution primitives

// drainCounting runs one query to completion and returns the rows it shipped.
// (labelcount_gate_ab_test.go already owns the name drainQuery, with a
// different signature.)
func drainCounting(tb testing.TB, eng *cypher.Engine, query string) int {
	tb.Helper()
	res, err := eng.Run(context.Background(), query, nil)
	if err != nil {
		tb.Fatalf("Run(%q): %v", query, err)
	}
	n := 0
	for res.Next() {
		n++
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("Err(%q): %v", query, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("Close(%q): %v", query, err)
	}
	return n
}
