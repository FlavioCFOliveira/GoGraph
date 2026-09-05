package cypher

// projection_fusion_bench_test.go — the measurement for rmp #2658: one shared
// [expr.RowContext] per input row for a whole projection body.
//
// # Why the arms live in ONE binary
//
// Both are built from the same tree and toggled by [projFusionDisabled], which the
// plan builder reads. That is deliberate: the counters the acceptance oracle needs
// ([projRowCtxBuildCount], [relRowBindCount]) are compiled into BOTH arms, so the
// comparison cannot be an artefact of instrumenting only the new one. A
// commit-to-commit comparison could not offer that.
//
// # The shapes
//
//	MultiAccess  — two property reads off ONE bound relationship. The shape the
//	               change targets: two mapper resolutions, two relStoredInverted
//	               decisions and two by-handle routings per row become one of each.
//	MultiAccess3 — three reads, to show the saving scales with the column count
//	               rather than being a fixed one-off.
//	SingleAccess — ONE property read: the null control. Fusion declines below two
//	               general-path items, so the two arms must be indistinguishable.
//	               A difference here is the instrumentation or the noise floor
//	               talking, not the change.
//	NoiseFloor   — the SAME (fused) arm under two names, so benchstat quantifies
//	               the floor the deltas above must clear.
//
// Run interleaved, on an idle host, with the load bracketed:
//
//	uptime
//	go test -run=^$ -bench='BenchmarkProjectionFusion' -benchmem -count=10 ./cypher/
//	uptime
//
// Layer: short (benchmarks are compiled, not run, by `go test`).

import (
	"context"
	"strconv"
	"testing"
)

// benchFusionEdges is the number of (a:P)-[:R]->(b:P) pairs each arm projects. Big
// enough that the per-row cost dominates the per-query plan lookup and drain, small
// enough that the seed (two write statements per pair) stays affordable.
const benchFusionEdges = 400

// seedFusionBenchGraph builds the fixture through CYPHER, so every relationship
// records its type by-handle and property reads take the per-instance route — the
// route the overwhelming majority of real relationship materialisations take.
func seedFusionBenchGraph(b *testing.B, n int) *Engine {
	b.Helper()
	e := NewEngine(newFusionGraph())
	run := func(q string) {
		res, err := e.RunInTx(context.Background(), q, nil)
		if err != nil {
			b.Fatalf("seed %q: %v", q, err)
		}
		for res.Next() { // intentional full drain
		}
		if err := res.Err(); err != nil {
			b.Fatalf("seed drain %q: %v", q, err)
		}
		_ = res.Close()
	}
	for i := 0; i < n; i++ {
		s := strconv.Itoa(i)
		run(`CREATE (a:P {nm:'a` + s + `'}), (b:P {nm:'b` + s + `'})`)
		run(`MATCH (a:P {nm:'a` + s + `'}), (b:P {nm:'b` + s + `'}) ` +
			`CREATE (a)-[:R {p_int:` + s + `, p_str:'s` + s + `', p_extra:` + s + `}]->(b)`)
	}
	return e
}

// benchFusionArm drives q to completion b.N times over a fixture built with fusion
// enabled or disabled.
//
// The engine is created INSIDE the arm and after the toggle is set, because the
// toggle is read at plan-build time and a cached plan carries the decision it was
// built with. The seed runs before ResetTimer, so only the read path is measured.
func benchFusionArm(b *testing.B, q string, fused bool) {
	projFusionDisabled.Store(!fused)
	defer projFusionDisabled.Store(false)
	e := seedFusionBenchGraph(b, benchFusionEdges)

	// One warm run, so the plan cache entry and the lazily-built per-query
	// resolvers exist before the timer starts and the first iteration is not an
	// outlier. Its row count is also the guard that the arm measures real work.
	rows := drainCount(b, e, q)
	if rows != benchFusionEdges {
		b.Fatalf("%q returned %d rows, want %d: the arm is not projecting the fixture",
			q, rows, benchFusionEdges)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if got := drainCount(b, e, q); got != benchFusionEdges {
			b.Fatalf("iteration %d returned %d rows, want %d", i, got, benchFusionEdges)
		}
	}
}

// drainCount runs q, touches every projected cell (so a lazily materialised value
// is actually dereferenced rather than measured as a deferred cost) and returns the
// row count.
func drainCount(b *testing.B, e *Engine, q string) int {
	b.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		b.Fatalf("Run(%q): %v", q, err)
	}
	cols := len(res.Columns())
	n := 0
	for res.Next() {
		for i := 0; i < cols; i++ {
			_ = res.ValueAt(i)
		}
		n++
	}
	if err := res.Err(); err != nil {
		b.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		b.Fatalf("Close(%q): %v", q, err)
	}
	return n
}

const (
	benchFusionQ2 = `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i, r.p_str AS s`
	benchFusionQ3 = `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i, r.p_str AS s, r.p_extra AS x`
	benchFusionQ1 = `MATCH (a:P)-[r:R]->(b:P) RETURN r.p_int AS i`
)

// BenchmarkProjectionFusion_MultiAccess2_Fused / _Unfused are the primary arms.
func BenchmarkProjectionFusion_MultiAccess2_Fused(b *testing.B) {
	benchFusionArm(b, benchFusionQ2, true)
}

// BenchmarkProjectionFusion_MultiAccess2_Unfused is the same shape with fusion off.
func BenchmarkProjectionFusion_MultiAccess2_Unfused(b *testing.B) {
	benchFusionArm(b, benchFusionQ2, false)
}

// BenchmarkProjectionFusion_MultiAccess3_Fused shows the saving scaling with the
// column count.
func BenchmarkProjectionFusion_MultiAccess3_Fused(b *testing.B) {
	benchFusionArm(b, benchFusionQ3, true)
}

// BenchmarkProjectionFusion_MultiAccess3_Unfused is its unfused arm.
func BenchmarkProjectionFusion_MultiAccess3_Unfused(b *testing.B) {
	benchFusionArm(b, benchFusionQ3, false)
}

// BenchmarkProjectionFusion_SingleAccess_Fused is the NULL CONTROL: one
// general-path item, so fusion declines and this arm must match the one below.
func BenchmarkProjectionFusion_SingleAccess_Fused(b *testing.B) {
	benchFusionArm(b, benchFusionQ1, true)
}

// BenchmarkProjectionFusion_SingleAccess_Unfused is the null control's other arm.
func BenchmarkProjectionFusion_SingleAccess_Unfused(b *testing.B) {
	benchFusionArm(b, benchFusionQ1, false)
}

// BenchmarkProjectionFusion_NoiseFloorA and _NoiseFloorB are the SAME arm under two
// names. Whatever benchstat reports between them is the floor every delta above has
// to clear to mean anything.
func BenchmarkProjectionFusion_NoiseFloorA(b *testing.B) {
	benchFusionArm(b, benchFusionQ2, true)
}

// BenchmarkProjectionFusion_NoiseFloorB is the noise floor's second name.
func BenchmarkProjectionFusion_NoiseFloorB(b *testing.B) {
	benchFusionArm(b, benchFusionQ2, true)
}
