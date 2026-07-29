package csrorder

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
)

// query_bench_test.go — the traversal-heavy query, end to end, per degree.
//
// This is the PRIMARY evidence for rmp #2145. The probe sweep in
// probe_bench_test.go measures the primitive; only these benchmarks measure the
// effect through the real planner, executor and CSR pair cache, which is where
// the change either pays for itself or does not.
//
// # Why the queries expand BACKWARDS
//
// The #2141 ordering is consumed by the forward-position membership probes, and
// every one of them fires on the REVERSE or UNDIRECTED expand path
// ([cypher/exec.Expand.advanceRevEdge]): a reverse slot must locate its
// corresponding forward edge, which costs O(deg(dst)) in the destination's
// forward run. A purely OUTWARD expand never probes at all — it walks a
// contiguous run and emits it. So a benchmark built on `(a)-[:LINK]->(b)` would
// exercise none of the changed code and would report the ordering as pure
// overhead. Both query shapes below therefore traverse `<-[:LINK]-` or
// `-[:LINK]-`.
//
// The relationship-type filter is also deliberate: it is what routes through
// reverseEdgePassesFilter, the third of the three probes #2142 converted.
//
// # How the ordered/unordered comparison is obtained
//
// NOT by a switch. Ordering is an invariant of every CSR graph/csr builds
// (order.go states it as such) and the executor's probes are only correct against
// an ordered run, so a production flag that disables ordering would be a path
// that hands the executor a snapshot it silently mis-reads. Instead these
// benchmarks are run twice — once at HEAD and once in a worktree at the
// sprint's design-doc-only commit, whose code is identical to the pre-sprint
// tree — and compared with benchstat. That also captures the #2142 probe change,
// which a same-binary switch could not reach at all.
//
// The recipe and the resulting figures are recorded in
// docs/benchmarks/csr-neighbour-ordering-2026-07-29.md.

// probeThreshold is the threshold the reported degree fractions are computed
// above. It is the CALIBRATED crossover from
// docs/design-degree-adaptive-adjacency.md §2.2 — 16, not the 64 the refuted
// audit §2.4 assumed.
const probeThreshold = 16

// auditThreshold is the refuted audit's threshold, reported alongside so the
// two are directly comparable and the cost of having believed 64 stays visible.
const auditThreshold = 64

// reverseExpandQuery is the traversal-heavy shape under test: for every :Target
// it walks the reverse :LINK arcs, and each reverse slot issues one
// forward-position probe into the hub's run of length `degree`.
//
// count(*) rather than a projection of properties: the measurement must be
// dominated by traversal, not by result materialisation, which has its own
// benchmarks in bench/cypher_scale.
const reverseExpandQuery = `MATCH (t:Target)<-[:LINK]-(h:Hub) RETURN count(*) AS n`

// undirectedExpandQuery drives BOTH the forward pass and the reverse pass,
// including the undirected self-loop dedup and the cyphermorphism edge-ID
// recovery that also reads a forward position.
const undirectedExpandQuery = `MATCH (t:Target)-[:LINK]-(h:Hub) RETURN count(*) AS n`

// powerLawQuery is the same reverse-expand shape over the power-law fixture,
// whose nodes are :Person/:KNOWS rather than :Hub/:Target.
const powerLawQuery = `MATCH (a:Person)<-[:KNOWS]-(b:Person) RETURN count(*) AS n`

// reportProfile attaches the fixture's measured degree distribution to the
// benchmark result, at both the calibrated and the refuted-audit thresholds.
//
// This is what makes a result unreadable without its skew: rmp #2145 requires
// the distribution alongside EVERY result, because §2.4's RMAT-versus-power-law
// gap (97.78% against 67.18% at T=64) is the difference between a number that
// reproduces on a real graph and one that does not. b.ReportMetric is used rather
// than b.Log because log lines interleave with the result lines and have
// previously made benchstat parse only a fraction of a run's benchmarks.
func reportProfile(b *testing.B, f *Fixture) {
	b.Helper()
	audit := ProfileDegrees(f.Graph.AdjList(), auditThreshold)
	b.ReportMetric(f.Profile.MeanDegree, "meanDeg")
	b.ReportMetric(float64(f.Profile.MaxDegree), "maxDeg")
	b.ReportMetric(100*f.Profile.VertexFrac, "%vtxT16")
	b.ReportMetric(100*f.Profile.EdgeFrac, "%edgeT16")
	b.ReportMetric(100*f.Profile.CostFrac, "%costT16")
	b.ReportMetric(100*audit.CostFrac, "%costT64")
}

// runTraversal executes query against f for b.N iterations, draining every row.
func runTraversal(b *testing.B, f *Fixture, query string) {
	b.Helper()
	engine := cypher.NewEngine(f.Graph)
	ctx := context.Background()

	// One warm-up run outside the timer so the Engine-level csrPairCache (#2143)
	// is populated. Without it the first iteration pays a full O(V+E) pair build
	// plus the ordering pass, which at low b.N dominates the sample and makes the
	// result an amortisation artefact rather than a steady-state cost — the same
	// class of artefact that made an apparent +18.7% allocation regression
	// evaporate under a fixed b.N during #2143.
	drain(ctx, b, engine, query)

	reportProfile(b, f)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		drain(ctx, b, engine, query)
	}
}

// drain runs query once and consumes every row, failing the benchmark on any
// error. Errors surface through Result.Err() as well as Run(), so both are
// checked.
func drain(ctx context.Context, b *testing.B, engine *cypher.Engine, query string) {
	b.Helper()
	res, err := engine.Run(ctx, query, nil)
	if err != nil {
		b.Fatalf("Run(%s): %v", query, err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		b.Fatalf("Result.Err(%s): %v", query, err)
	}
	if err := res.Close(); err != nil {
		b.Fatalf("Result.Close(%s): %v", query, err)
	}
}

// BenchmarkTraversalReverse sweeps the controlled out-degree across the
// crossover, reported per degree.
//
// The arc count is held constant by [HubFixtureArcs], so the number of probes is
// the same at every degree and only the cost of one probe varies. Degrees 8 and 16
// are the no-regression rows: at 8 the scan was cheaper and the ordering must not
// cost more than it saves; at 16 the two are at parity.
func BenchmarkTraversalReverse(b *testing.B) {
	for _, d := range SweptDegrees {
		f, err := HubFixture(d, probeThreshold)
		if err != nil {
			b.Fatalf("HubFixture(%d): %v", d, err)
		}
		b.Run(degreeName(d), func(b *testing.B) { runTraversal(b, f, reverseExpandQuery) })
	}
}

// BenchmarkTraversalUndirected sweeps the same degrees over the undirected shape,
// which runs the forward pass as well and so dilutes the probe's share of the
// total — the honest end of the range a real mixed query sits in.
func BenchmarkTraversalUndirected(b *testing.B) {
	for _, d := range SweptDegrees {
		f, err := HubFixture(d, probeThreshold)
		if err != nil {
			b.Fatalf("HubFixture(%d): %v", d, err)
		}
		b.Run(degreeName(d), func(b *testing.B) { runTraversal(b, f, undirectedExpandQuery) })
	}
}

// BenchmarkTraversalPowerLaw is the PRIMARY realistic measurement: the same
// reverse-expand shape over a Barabási–Albert fixture, whose out-degree
// distribution is a power law rather than a single controlled value.
//
// Its result is the one to quote for "what does this change buy in practice",
// because the degree sweep above deliberately includes degrees (512, 4096) that
// no real property graph reaches. The reported %costT16/%costT64 metrics say how
// much of this fixture's scan cost sits above each threshold.
func BenchmarkTraversalPowerLaw(b *testing.B) {
	f, err := PowerLawFixture(probeThreshold)
	if err != nil {
		b.Fatalf("PowerLawFixture: %v", err)
	}
	runTraversal(b, f, powerLawQuery)
}

// BenchmarkTraversalRMAT is the CONTRAST, not a target. RMAT's extreme skew puts
// nearly all scan cost above the threshold, so this benchmark should show a
// larger win than BenchmarkTraversalPowerLaw — and that gap is the point. A
// result quoted from here alone would not reproduce on a real graph.
func BenchmarkTraversalRMAT(b *testing.B) {
	f, err := RMATFixture(probeThreshold)
	if err != nil {
		b.Fatalf("RMATFixture: %v", err)
	}
	runTraversal(b, f, powerLawQuery)
}
