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

import (
	"context"
	"fmt"
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

// deleteCycleRatio drives `cycles` seed-and-wipe rounds against ONE engine and
// returns the ratio of the last cycle's wipe time to the first's, together with
// the per-cycle timings for the failure message.
//
// The engine is never recreated, which is the whole point: what regressed was
// the cost of deleting from a graph that has already deleted a lot.
func deleteCycleRatio(t *testing.T, perCycle, batch, cycles int, detach bool) (float64, []time.Duration) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()

	took := make([]time.Duration, 0, cycles)
	for cycle := 1; cycle <= cycles; cycle++ {
		if detach {
			for done := 0; done < perCycle; done += batch {
				runStmt(ctx, t, eng, fmt.Sprintf(
					`UNWIND range(1, %d) AS i CREATE (:Tmp)-[:R]->(:Tmp)`, min(batch, perCycle-done)/2))
			}
		} else {
			seedTmp(ctx, t, eng, perCycle, batch)
		}
		if seen := countTmp(ctx, t, eng); seen != int64(perCycle) {
			t.Fatalf("cycle %d: seeded %d :Tmp nodes, want %d", cycle, seen, perCycle)
		}
		took = append(took, wipeTmp(ctx, t, eng, batch, detach))
	}
	return float64(took[len(took)-1]) / float64(took[0]), took
}

// maxCycleRatio is the threshold separating the two measured regimes. On the
// pre-fix build the sixth cycle cost about 5.2x the first (the growth is linear
// in the nodes ever interned, so the ratio rises with the cycle count); on the
// fixed build it is about 1.1x. 2.5 sits between them with room on both sides
// for a loaded machine, and it is a THRESHOLD rather than a direction-only
// "must not grow", which noise alone would trip.
const maxCycleRatio = 2.5

// TestDeleteDoesNotDegradeAcrossCycles is the rmp #2400 gate: wiping a fixed
// number of nodes must cost the same however many nodes were deleted before it.
func TestDeleteDoesNotDegradeAcrossCycles(t *testing.T) {
	t.Parallel()
	ratio, took := deleteCycleRatio(t, 20_000, 5_000, 6, false)
	if ratio > maxCycleRatio {
		t.Fatalf("wipe time grew %.2fx from the first cycle to the last (limit %.1fx); per-cycle %v",
			ratio, maxCycleRatio, took)
	}
	t.Logf("DELETE per-cycle %v (last/first %.2fx)", took, ratio)
}

// TestDetachDeleteDoesNotDegradeAcrossCycles is the same gate for nodes that
// carry relationships — the question section 8 of the assessment left open
// (rmp #2418). It degraded identically before the fix: 107 ms rising to 503 ms
// across five cycles.
func TestDetachDeleteDoesNotDegradeAcrossCycles(t *testing.T) {
	t.Parallel()
	ratio, took := deleteCycleRatio(t, 5_000, 1_000, 6, true)
	if ratio > maxCycleRatio {
		t.Fatalf("DETACH DELETE wipe time grew %.2fx from the first cycle to the last (limit %.1fx); per-cycle %v",
			ratio, maxCycleRatio, took)
	}
	t.Logf("DETACH DELETE per-cycle %v (last/first %.2fx)", took, ratio)
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
// # Why this one is soak and its two siblings are not
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
// deleted — stays in the short layer, in the two cycle-ratio tests above.
// Those are self-normalising: contention inflates the first cycle and the last
// alike, so their ratio survives a busy machine, and they fail on the pre-fix
// build by 4.67x and 5.04x against a 2.5x threshold.
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
