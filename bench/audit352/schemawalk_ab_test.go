package audit352_test

// schemawalk_ab_test.go — the wall-clock and allocation A/B for rmp #2645
// (hoisting the row-context schema walk to the plan-time [cypher.rowSchema]).
//
// # Why this A/B is CROSS-BINARY and not same-process
//
// Every other A/B in this package flips its arm inside one process, through a
// seam (internal/sortseam). #2645 cannot: its entire point is that the
// map-taking forms of buildRowCtx, buildRowCtxWithUse and evalRowPooled are
// DELETED, so the per-row path can no longer be reached with a bare map. A
// process built from the converted source has no second arm to select — keeping
// one would defeat the change it is meant to measure.
//
// The arms are therefore two binaries, run ALTERNATELY (a/b/a/b…), never all of
// A then all of B, so thermal drift and frequency scaling cannot bias one arm.
// The confound a single binary would have removed — code layout, ASLR, build
// nondeterminism — is instead MEASURED: the noise floor is taken between two
// independent builds of the SAME source, so whatever layout costs, it is inside
// the floor rather than mistaken for the effect.
//
//	# noise floor: same source, two builds, interleaved
//	go test -c -o /tmp/wf1.test ./bench/audit352/
//	go test -c -o /tmp/wf2.test ./bench/audit352/
//	for i in $(seq 1 6); do
//	  for x in wf1 wf2; do
//	    /tmp/$x.test -test.run='^$' -test.bench=BenchmarkSchemaWalkShape \
//	      -test.benchtime=1s -test.count=1 -test.benchmem >> /tmp/floor.$x.txt
//	  done
//	done
//	benchstat /tmp/floor.wf1.txt /tmp/floor.wf2.txt
//
//	# the A/B: same protocol, arm A built from the pre-#2645 tree
//	benchstat /tmp/ab.a.txt /tmp/ab.b.txt
//
// # Shape selection, and the control that had to be replaced
//
// The four measured shapes are the ones an exact allocation profile showed the
// walk actually costing — NOT the ones the original audit named, which were
// re-measured at HEAD and found stale.
//
// `whole_node` is the NULL CONTROL, and it is load-bearing rather than
// decorative: it must NOT move. If it does, the reading is machine drift and
// every other number here is void.
//
// Choosing it took two attempts, and the first attempt is worth recording. The
// obvious control was `RETURN p.salary`, which an exact profile showed reaching
// the walk ZERO times — but that profile was taken on a small, edgeless fixture
// where the shape plans `ColumnarProject`. On this package's 120 000-node
// benchGraph the SAME query plans `Project -> ParallelScanProject` and took the
// row path, allocating one walk per row (120 000 objects, 39.37% of the query's
// entire allocation window). It was not a control at all; it was one of the
// largest beneficiaries, and it is now measured as one.
//
// `whole_node` is the correct control precisely because it holds everything
// else fixed: the same fixture, the same 120 000 rows, and the same
// `Project -> ParallelScanProject` plan as `salary_proj` — differing only in
// that a whole-node projection never takes the converted general path. Measured
// beneath newSchemaWalk on benchGraph: 0 objects before the change, 0 after.

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// schemaWalkShapes run against the shared 120 000-node benchGraph, so no arm can
// differ from another by its fixture.
var schemaWalkShapes = []struct {
	name  string
	query string
}{
	// 64.57% of all objects allocated by this query were the per-row walk.
	{"distinct", `MATCH (p:Person) RETURN DISTINCT p.bucket`},
	// 17.75% — the aggregation pre-projection closure.
	{"agg_expr", `MATCH (p:Person) RETURN p.bucket + 1 AS b, count(*) AS c`},
	// 15.70% — the UNWIND list closure plus the projection.
	{"unwind", `MATCH (p:Person) UNWIND [1,2,3] AS x RETURN p.firstName, x`},
	// 39.37% on THIS fixture — the projection closure driven by the morsel
	// workers of ParallelScanProject, which is also what exercises the
	// rowSchema concurrency contract: one carrier, many worker goroutines.
	{"salary_proj", `MATCH (p:Person) RETURN p.salary`},
	// 0.00% before and after — the null control. Same fixture, same row count,
	// same plan as salary_proj; only the projection differs. Must not move.
	{"whole_node", `MATCH (p:Person) RETURN p`},
}

// BenchmarkSchemaWalkShape drives each shape to completion through the shared
// [runQuery] primitive, which also fails the run if the physical plan drifts
// mid-measurement.
func BenchmarkSchemaWalkShape(b *testing.B) {
	engine := cypher.NewEngine(benchGraph)
	for _, s := range schemaWalkShapes {
		s := s
		b.Run("shape="+s.name, func(b *testing.B) { runQuery(b, engine, s.query) })
	}
}
