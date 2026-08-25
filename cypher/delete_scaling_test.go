package cypher_test

// delete_scaling_test.go — rmp #2400 / #2418.
//
// The 2026-08-11 concurrency assessment measured GoGraph deleting nodes at
// 82 µs each against Neo4j's 2.2 µs and Memgraph's 0.2 µs, and — worse than the
// constant — growing without bound: five seed-and-wipe cycles of the SAME node
// count took 3.279 s, 6.312 s, 9.331 s, 12.366 s, 15.656 s against one live
// engine, at exactly one core of four.
//
// The assessment named the tombstone bitmap's copy-on-write clone as the root
// cause. IT WAS NOT. Reproducing the cycle in process and profiling it put
// 78.77% of the CPU in graph.Mapper.Walk, reached from the delete path's
// InNeighbours, against 0.99% in the bitmap clone (1.72% for the whole of
// removeNodeInfo). InNeighbours answered "what points at this
// node" by scanning EVERY interned node and every one of its adjacency slots,
// once per node deleted — so deleting k nodes from a graph of n cost O(k·n),
// and because a deleted node keeps its Mapper slot for ever, n counted every
// node the graph had EVER held rather than the ones still live. That is why the
// cost grew per cycle for identical work, why it was flat within a cycle, and
// why one core was busy: the walk is serial.
//
// The fix is the adjacency's live in-edge index (graph/adjlist/reverse.go).
// These tests are the gate that keeps it: they fail on the old behaviour and
// pass on the new.
//
// # Which layer measures what, and why the short layer measures CPU
//
// The gate is a last/first RATIO over six cycles. Until 2026-08-19 it took that
// ratio over WALL time in the short layer, on the stated reasoning that
// "contention inflates the first cycle and the last alike, so their ratio
// survives a busy machine". That reasoning is wrong, and it made `make ci` fail
// on a flat engine:
//
//	--- FAIL: TestDetachDeleteDoesNotDegradeAcrossCycles (13.61s)
//	    DETACH DELETE wipe time grew 3.25x ...; per-cycle
//	    [637.386625ms 730.918292ms 868.345292ms 1.894403292s 2.376355s 2.073945625s]
//
// A ratio does cancel CONSTANT load. It does not cancel load that CHANGES
// during the run, which is exactly what `make ci` does as sibling packages
// start and finish — note the last cycle DIPPING above, a load shape no
// algorithmic regression can produce. Reproduced deliberately: with the load
// ramped from 0 to 300 `yes > /dev/null` workers during the run, this gate's
// wall-clock form reported 12.96x and FAILED on the flat engine.
//
// Wall and CPU were then measured together, per cycle, in four load regimes
// (10-core darwin/arm64, under `-race`, DETACH arm, 5 000 nodes per cycle):
//
//	regime                        wall first … last    wall ratio   CPU ratio
//	idle                          239 ms …  240 ms       1.00x        1.03x
//	300 workers, constant         7.80 s …  9.10 s       1.17x        0.99x
//	300 workers, ramping up       620 ms …  9.91 s      15.98x        1.00x
//	idle first → saturated last   239 ms …  8.59 s      35.88x        1.50x
//
// So the short layer's wall-clock noise floor is 35.9x against a defect signal
// of 5.2x: the instrument cannot see the defect it exists to catch, in either
// shape. An absolute CEILING — the shape docs/test-layers.md recommends for the
// short layer, because it needs no window — fails here for the same reason and
// is REJECTED on the same measurements: it would have to sit above 9.10 s to
// survive constant saturation, while the pre-fix sixth cycle takes about 1.2 s
// on an idle machine. It would be strictly blinder than the ratio, not safer.
//
// CPU time is the load-invariant instrument, and it is also the FAITHFUL one:
// the defect is a serial scan burning one core, so it is a CPU regression, and
// wall time was only ever a proxy for it. Its worst-case noise is 1.50x (a
// cycle-1-idle, cycle-6-saturated run: contention costs about 50% more CPU per
// unit of work through cache and TLB pressure, then saturates), against the
// same 5.2x signal and the same 2.5x threshold. TestDeleteCycleGateDetectsDegradation
// below is the standing proof that the threshold is still reachable.
//
// Two instruments were measured and REJECTED, both of which would have looked
// like a fix:
//
//   - runtime/metrics `/cpu/classes/user:cpu-seconds` is not CPU time. It is
//     derived from GOMAXPROCS × wall-clock accounting, so a goroutine descheduled
//     by the OS still accrues it. In the very run where getrusage moved 1.50x it
//     moved 50.2x — it is wall clock wearing a CPU name.
//   - Allocation counts (runtime.MemStats.Mallocs) ARE perfectly load-invariant
//     here — 178 828 → 166 173 saturated against 178 819 → 166 536 idle, a 0.5%
//     difference across a 35.9x wall inflation — but nothing establishes that the
//     pre-fix O(k·n) Mapper.Walk allocated in proportion to the nodes it scanned.
//     An oracle whose power against the actual defect is unknown is cover, not a
//     gate, so allocations are not asserted on.
//
// The wall-clock ratio is preserved verbatim in delete_scaling_soak_test.go,
// where the soak layer's quiet machine is the precondition it needs.
//
// # These tests must not call t.Parallel()
//
// getrusage(RUSAGE_SELF) is PROCESS-scoped: it counts every goroutine in the
// test binary, so a sibling test running concurrently is charged to our cycle.
// Go runs non-parallel top-level tests to completion before resuming any test
// that called t.Parallel(), so dropping t.Parallel() is what makes the
// instrument attributable. Adding it back silently converts these gates into
// noise detectors. Load from OTHER packages' test binaries is irrelevant —
// it is a different process, and that is the whole point of measuring here.
//
// syscall.Getrusage is unix-only; the module carries no Windows provision
// (no build-tagged windows file anywhere in the tree) and is gated locally on
// darwin, so the file is left untagged rather than split.

import (
	"context"
	"fmt"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// runStmt executes q and drains it, failing the test on any error.
func runStmt(ctx context.Context, tb testing.TB, eng *cypher.Engine, q string) {
	tb.Helper()
	res, err := eng.RunInTx(ctx, q, nil)
	if err != nil {
		tb.Fatalf("RunInTx(%q): %v", q, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("result(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		tb.Fatalf("close(%q): %v", q, err)
	}
}

// countTmp returns the number of live :Tmp nodes, failing if the query yields
// no row at all — an oracle that silently reports zero would let every wipe
// loop below "converge" without deleting anything.
func countTmp(ctx context.Context, tb testing.TB, eng *cypher.Engine) int64 {
	tb.Helper()
	res, err := eng.RunInTx(ctx, `MATCH (t:Tmp) RETURN count(t) AS c`, nil)
	if err != nil {
		tb.Fatalf("count: %v", err)
	}
	var c int64
	var saw bool
	for res.Next() {
		saw = true
		switch v := res.Record()["c"].(type) {
		case expr.IntegerValue:
			c = int64(v)
		case int64:
			c = v
		default:
			tb.Fatalf("count column has unexpected type %T", res.Record()["c"])
		}
	}
	if err := res.Err(); err != nil {
		tb.Fatalf("count result: %v", err)
	}
	_ = res.Close()
	if !saw {
		tb.Fatal("count query returned no rows")
	}
	return c
}

// seedTmp creates n :Tmp nodes in batches.
func seedTmp(ctx context.Context, tb testing.TB, eng *cypher.Engine, n, batch int) {
	tb.Helper()
	for done := 0; done < n; done += batch {
		size := min(batch, n-done)
		runStmt(ctx, tb, eng, fmt.Sprintf(`UNWIND range(1, %d) AS i CREATE (:Tmp {i: i})`, size))
	}
}

// wipeTmp deletes every :Tmp node in batches and returns how long it took.
func wipeTmp(ctx context.Context, tb testing.TB, eng *cypher.Engine, batch int, detach bool) time.Duration {
	tb.Helper()
	verb := "DELETE"
	if detach {
		verb = "DETACH DELETE"
	}
	start := time.Now()
	for guard := 0; ; guard++ {
		if countTmp(ctx, tb, eng) == 0 {
			break
		}
		if guard > 1000 {
			tb.Fatal("wipe did not converge")
		}
		runStmt(ctx, tb, eng, fmt.Sprintf(`MATCH (t:Tmp) WITH t LIMIT %d %s t`, batch, verb))
	}
	return time.Since(start)
}

// processCPU returns the CPU time — user plus system — that this PROCESS has
// consumed since it started. It is the short layer's load-invariant instrument;
// see the file header for the measurements that chose it and for why its
// callers must not run in parallel with anything else in the binary.
func processCPU(tb testing.TB) time.Duration {
	tb.Helper()
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		// Refuse to measure rather than silently report a zero delta, which
		// would make every ratio below 0/0 and pass unconditionally.
		tb.Fatalf("getrusage(RUSAGE_SELF): %v", err)
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// cycleSample is one seed-and-wipe cycle's measurement of the WIPE alone; the
// seeding that precedes it is deliberately outside the window.
type cycleSample struct {
	wall time.Duration
	cpu  time.Duration
	// alloc is the bytes allocated by the wipe. It is the GATED statistic since
	// rmp #2589; wall and cpu are kept and logged as diagnostics.
	alloc uint64
}

func wallOf(s []cycleSample) []time.Duration {
	out := make([]time.Duration, len(s))
	for i, c := range s {
		out[i] = c.wall
	}
	return out
}

func cpuOf(s []cycleSample) []time.Duration {
	out := make([]time.Duration, len(s))
	for i, c := range s {
		out[i] = c.cpu
	}
	return out
}

// ratio is the last cycle's cost over the first's, for the duration diagnostics.
func ratio(d []time.Duration) float64 {
	return float64(d[len(d)-1]) / float64(d[0])
}

func allocOf(s []cycleSample) []uint64 {
	out := make([]uint64, len(s))
	for i, c := range s {
		out[i] = c.alloc
	}
	return out
}

// allocRatio is THE gated statistic (rmp #2589): the bytes the last wipe
// allocated over the bytes the first did.
func allocRatio(v []uint64) float64 {
	return float64(v[len(v)-1]) / float64(v[0])
}

// deleteCycles drives `cycles` seed-and-wipe rounds against ONE engine and
// returns the per-cycle wall and CPU cost of each wipe.
//
// The engine is never recreated, which is the whole point: what regressed was
// the cost of deleting from a graph that has already deleted a lot.
//
// When degrade is true, cycle i seeds i×perCycle nodes rather than perCycle —
// a workload that costs linearly more each cycle. That is not a model of the
// pre-fix engine's mechanism (which raised the per-NODE cost); it is a
// deliberately-degrading control used to prove that the gate can still fire.
func deleteCycles(t *testing.T, perCycle, batch, cycles int, detach, degrade bool) []cycleSample {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	took := make([]cycleSample, 0, cycles)
	for cycle := 1; cycle <= cycles; cycle++ {
		want := perCycle
		if degrade {
			want = perCycle * cycle
		}
		if detach {
			for done := 0; done < want; done += batch {
				runStmt(ctx, t, eng, fmt.Sprintf(
					`UNWIND range(1, %d) AS i CREATE (:Tmp)-[:R]->(:Tmp)`, min(batch, want-done)/2))
			}
		} else {
			seedTmp(ctx, t, eng, want, batch)
		}
		if seen := countTmp(ctx, t, eng); seen != int64(want) {
			t.Fatalf("cycle %d: seeded %d :Tmp nodes, want %d", cycle, seen, want)
		}
		var memBefore, memAfter runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&memBefore)
		cpu0 := processCPU(t)
		wall := wipeTmp(ctx, t, eng, batch, detach)
		cpu := processCPU(t) - cpu0
		runtime.ReadMemStats(&memAfter)
		took = append(took, cycleSample{
			wall:  wall,
			cpu:   cpu,
			alloc: memAfter.TotalAlloc - memBefore.TotalAlloc,
		})
	}
	return took
}

// maxCycleRatio is the threshold separating the two measured regimes. On the
// pre-fix build the sixth cycle cost about 5.2x the first (the growth is linear
// in the nodes ever interned, so the ratio rises with the cycle count); on the
// fixed build it is about 1.1x.
//
// 2.5 sits between them, and against the CPU instrument it has room on both
// sides that it never had against wall clock: the measured worst-case noise is
// 1.50x (see the file header), leaving 1.67x of headroom below the threshold
// and 2.08x of margin above it before the defect's 5.2x. The equivalent figures
// for wall clock were 35.9x of noise against 5.2x of signal — inverted.
//
// It is a THRESHOLD rather than a direction-only "must not grow", which noise
// alone would trip.
const maxCycleRatio = 2.5

// TestDeleteDoesNotDegradeAcrossCycles is the rmp #2400 gate: wiping a fixed
// number of nodes must cost the same however many nodes were deleted before it.
//
// Deliberately NOT parallel — see the file header.
func TestDeleteDoesNotDegradeAcrossCycles(t *testing.T) {
	got := deleteCycles(t, 20_000, 5_000, 6, false, false)
	alloc, cpu, wall := allocOf(got), cpuOf(got), wallOf(got)
	r := allocRatio(alloc)
	if r > maxCycleRatio {
		t.Fatalf("DELETE wipe ALLOCATION grew %.2fx from the first cycle to the last (limit %.1fx); "+
			"per-cycle bytes %v. Allocation volume cannot be moved by machine load, so this is the "+
			"engine (rmp #2589). Diagnostics: CPU %v, wall %v", r, maxCycleRatio, alloc, cpu, wall)
	}
	t.Logf("DELETE per-cycle alloc %v (last/first %.2fx); CPU %v (%.2fx); wall %v (%.2fx)",
		alloc, r, cpu, ratio(cpu), wall, ratio(wall))
}

// TestDetachDeleteDoesNotDegradeAcrossCycles is the same gate for nodes that
// carry relationships — the question section 8 of the assessment left open
// (rmp #2418). It degraded identically before the fix: 107 ms rising to 503 ms
// across five cycles.
//
// Deliberately NOT parallel — see the file header.
func TestDetachDeleteDoesNotDegradeAcrossCycles(t *testing.T) {
	got := deleteCycles(t, 5_000, 1_000, 6, true, false)
	alloc, cpu, wall := allocOf(got), cpuOf(got), wallOf(got)
	r := allocRatio(alloc)
	if r > maxCycleRatio {
		t.Fatalf("DETACH DELETE wipe ALLOCATION grew %.2fx from the first cycle to the last "+
			"(limit %.1fx); per-cycle bytes %v. Diagnostics: CPU %v, wall %v",
			r, maxCycleRatio, alloc, cpu, wall)
	}
	t.Logf("DETACH DELETE per-cycle alloc %v (last/first %.2fx); CPU %v (%.2fx); wall %v (%.2fx)",
		alloc, r, cpu, ratio(cpu), wall, ratio(wall))
}

// TestDeleteCycleGateDetectsDegradation is the power control for the two gates
// above: it runs the SAME statistic, on the same instrument, over a workload
// that is engineered to degrade, and requires the gate to FIRE.
//
// Without it the two gates are unfalsifiable in a green tree — the engine is
// flat, the pre-fix build cannot be rebuilt, and a gate that has quietly lost
// its power reads exactly like a gate that is passing. Cycle i wipes i×2 000
// nodes, so the last cycle does 6x the first cycle's work. Measured against the
// 2.5x threshold over five idle runs, the CPU ratio lands in 6.48x–6.77x (wall
// 6.34x–6.59x); under 300 competing CPU-bound processes on the same 10-core host
// it falls to 5.82x, and under a load ramped up during the run it rises to 8.80x.
// The margin over the threshold is therefore at least 2.3x in every regime
// measured, and the control has never come within 3.3x of passing.
//
// A failure here means the short-layer gates can no longer see a
// pre-fix-shaped regression, which is worse than a flaky gate: FIX THE GATE,
// do not delete this test.
//
// Deliberately NOT parallel — see the file header.
func TestDeleteCycleGateDetectsDegradation(t *testing.T) {
	// Guarded for the MIRROR reason to the two gates above, and it must be
	// guarded with them or the pair becomes incoherent: this control requires the
	// ratio to EXCEED the threshold, so where load inflates the gates' ratios it
	// can equally compress this one below the floor it needs (rmp #2589). A
	// control that fires or not according to machine load proves nothing about
	// the gates' power either way.
	got := deleteCycles(t, 2_000, 1_000, 6, true, true)
	alloc, cpu, wall := allocOf(got), cpuOf(got), wallOf(got)
	r := allocRatio(alloc)
	if r <= maxCycleRatio {
		t.Fatalf("a workload doing 6x more work in its last cycle than its first moved the ALLOCATION "+
			"ratio only %.2fx, which the %.1fx gate PASSES: the delete-scaling gates have lost their "+
			"power; per-cycle bytes %v. Diagnostics: CPU %v, wall %v",
			r, maxCycleRatio, alloc, cpu, wall)
	}
	t.Logf("degrading control: per-cycle alloc %v (last/first %.2fx, gate %.1fx); CPU %v (%.2fx); wall %v",
		alloc, r, maxCycleRatio, cpu, ratio(cpu), wall)
}

// singleStatementDeleteBudget bounds the one-statement delete below. The
// assessment found that a single-statement delete of about 90 000 nodes
// exceeded bolt/server's DefaultTxTimeout of 30 s and returned
// TransactionTimedOut, which is what made this defect a FAILURE rather than
// merely slowness. This budget is a third of that timeout: it is the margin
// that keeps the statement from being anywhere near the cliff, not a
// performance assertion.
const singleStatementDeleteBudget = 10 * time.Second

// TestSingleStatementDeleteOfNinetyThousandNodes deletes 90 000 nodes in ONE
// statement and requires it to finish well inside the transaction timeout that
// the pre-fix build blew through: 15.97 s before the fix, 375.6 ms after it.
//
// # Why this one is soak
//
// It asserts an ABSOLUTE wall-clock budget, and the short layer runs under
// `go test -race` with the rest of the package's parallel tests competing for
// the same cores. Measured there, this test took 40.61 s for work that takes
// 375.6 ms on its own — the budget was reading contention and the race
// detector, not the delete path, and it failed `make ci` for that reason
// rather than for a regression. The soak layer gives it a quiet machine, which
// is the precondition an absolute timing assertion needs.
//
// The REGRESSION property — that the cost does not grow with the nodes ever
// deleted — stays in the short layer, in the two cycle-ratio gates above,
// which is possible only because they measure CPU rather than wall time. Their
// wall-clock form is in delete_scaling_soak_test.go, alongside this test and
// for the same reason.
func TestSingleStatementDeleteOfNinetyThousandNodes(t *testing.T) {
	testlayers.RequireSoak(t)
	const nodes = 90_000
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	seedTmp(ctx, t, eng, nodes, 10_000)
	if seen := countTmp(ctx, t, eng); seen != nodes {
		t.Fatalf("seeded %d nodes, want %d", seen, nodes)
	}

	start := time.Now()
	runStmt(ctx, t, eng, `MATCH (t:Tmp) DELETE t`)
	took := time.Since(start)

	if left := countTmp(ctx, t, eng); left != 0 {
		t.Fatalf("after the delete, %d :Tmp nodes remain", left)
	}
	if took > singleStatementDeleteBudget {
		t.Fatalf("deleting %d nodes in one statement took %v, budget %v",
			nodes, took, singleStatementDeleteBudget)
	}
	t.Logf("deleted %d nodes in one statement in %v", nodes, took)
}

// BenchmarkDeleteAccumulated deletes a FIXED batch of nodes from engines that
// differ only in how many nodes were already deleted before it. Under #2400 the
// per-node cost rises with the accumulated total; once the cost is independent
// of it, every arm reports the same ns/node within noise. This is the arm-by-arm
// benchstat evidence for the fix.
func BenchmarkDeleteAccumulated(b *testing.B) {
	const batch = 5_000
	for _, accumulated := range []int{0, 20_000, 60_000} {
		b.Run(fmt.Sprintf("accumulated=%d", accumulated), func(b *testing.B) {
			ctx := context.Background()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				g := lpg.New[string, float64](adjlist.Config{Directed: true})
				eng := cypher.NewEngine(g)
				if accumulated > 0 {
					seedTmp(ctx, b, eng, accumulated, 10_000)
					wipeTmp(ctx, b, eng, 10_000, false)
				}
				seedTmp(ctx, b, eng, batch, batch)
				b.StartTimer()
				runStmt(ctx, b, eng, `MATCH (t:Tmp) DELETE t`)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*batch), "ns/node")
		})
	}
}

// BenchmarkCreateRelationships measures the END-TO-END cost of creating
// relationships through Cypher. It exists to price the in-edge index the delete
// fix added: the index makes every edge insertion do a little more work, and
// the number that matters is what that costs a real write, not what it costs
// the adjacency micro-benchmark in isolation.
func BenchmarkCreateRelationships(b *testing.B) {
	const perStatement = 500
	ctx := context.Background()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runStmt(ctx, b, eng, fmt.Sprintf(
			`UNWIND range(1, %d) AS i CREATE (:Src)-[:R]->(:Dst)`, perStatement))
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*perStatement), "ns/rel")
}
