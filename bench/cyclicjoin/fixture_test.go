package cyclicjoin_test

// fixture_test.go — validates that the benchmark fixtures actually contain what the
// measurements assume (rmp #2159).
//
// Layer: short.
//
// THIS IS NOT COVERAGE PADDING. Sprint 314 recorded a fixture that left its far
// endpoints at out-degree ZERO and consequently reported 1.06× for an operator that
// genuinely worked — a silent, total invalidation of the measurement, with every
// number in the report technically correct and completely meaningless. A fixture
// that does not contain the shape under test cannot be detected from the benchmark
// output, only from an assertion like the ones below.
//
// So each generator is checked against the properties the report leans on:
// uniform degree really is uniform, triangles really exist, and the power-law
// fixture really is skewed.

import (
	"context"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/cyclicjoin"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// countOf runs a count(*) query and returns the single scalar.
func countOf(t *testing.T, g *lpg.Graph[string, float64], q string) int64 {
	t.Helper()
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%s): %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var n int64
	for res.Next() {
		if v := res.ValueAt(0); v != nil {
			// The count is an integer cell; parse via its string form so this helper
			// needs no dependency on the value type's concrete identity.
			var acc int64
			for _, ch := range v.String() {
				if ch < '0' || ch > '9' {
					acc = -1
					break
				}
				acc = acc*10 + int64(ch-'0')
			}
			n = acc
		}
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%s): %v", q, err)
	}
	return n
}

// TestSeedUniform_IsUniformAndHasTriangles pins the two properties the uniform rows
// of the report depend on.
func TestSeedUniform_IsUniformAndHasTriangles(t *testing.T) {
	for _, degree := range []int{1, 2, 4, 16} {
		const n = 200
		g, err := cyclicjoin.SeedUniform(n, degree)
		if err != nil {
			t.Fatalf("SeedUniform(%d, %d): %v", n, degree, err)
		}
		// Every node has exactly degree+1 out-edges (degree forward, one back).
		wantEdges := int64(n * (degree + 1))
		if got := countOf(t, g, `MATCH ()-[:K]->() RETURN count(*) AS n`); got != wantEdges {
			t.Fatalf("degree %d: edge count = %d, want %d — the fixture is not what the "+
				"report's m = n*(d+1) assumes", degree, got, wantEdges)
		}
		// Out-degree must be IDENTICAL for every node: the report's claim that this
		// fixture carries no skew advantage rests entirely on that.
		minMax := countOf(t, g,
			`MATCH (a)-[:K]->() WITH a, count(*) AS d RETURN count(DISTINCT d) AS n`)
		if minMax != 1 {
			t.Fatalf("degree %d: found %d DISTINCT out-degrees; a uniform fixture must have "+
				"exactly 1, or the 'no skew advantage' claim in the report is false", degree, minMax)
		}
		// Triangles must genuinely exist for degree >= 2, or the benchmark measures
		// wasted enumeration only and every ratio in the report describes the empty case.
		//
		// DEGREE 1 IS THE DOCUMENTED EXCEPTION, and pinning it is the point rather than
		// an inconvenience. At degree 1 a node reaches only k+1 and k-1, so closing a
		// 3-cycle needs three offsets from {+1, -1} summing to 0 mod n, which is
		// impossible for n > 3. The d=1 benchmark row therefore measures the NO-OUTPUT
		// path — still a meaningful data point, because that is exactly where the SPIKE
		// asked whether the merge's setup loses when there is nothing to find, but it is
		// NOT evidence about output-producing work and the report must not read it as
		// such. This assertion exists so that distinction cannot be lost later.
		tri := countOf(t, g, cyclicjoin.TriangleQuery)
		switch {
		case degree == 1 && tri != 0:
			t.Fatalf("degree 1 produced %d triangles; the ring cannot close a 3-cycle with "+
				"only +/-1 steps, so this is a fixture change that invalidates the d=1 row's "+
				"documented meaning", tri)
		case degree >= 2 && tri <= 0:
			t.Fatalf("degree %d: the fixture contains NO triangles (count=%d), so the "+
				"triangle benchmark would measure only wasted enumeration", degree, tri)
		}
		// And 2-cycles, for the ClosingQuery row.
		if two := countOf(t, g, cyclicjoin.ClosingQuery); two <= 0 {
			t.Fatalf("degree %d: the fixture contains no 2-cycles (count=%d)", degree, two)
		}
	}
}

// TestSeedPowerLaw_IsActuallySkewed pins the property that makes the power-law row
// mean anything: a heavy tail. A generator that silently produced a near-regular
// graph would make that row a duplicate of the uniform one.
func TestSeedPowerLaw_IsActuallySkewed(t *testing.T) {
	const n = 600
	g, err := cyclicjoin.SeedPowerLaw(n, 6, 0.9, 315201)
	if err != nil {
		t.Fatalf("SeedPowerLaw: %v", err)
	}
	distinct := countOf(t, g,
		`MATCH (a)-[:K]->() WITH a, count(*) AS d RETURN count(DISTINCT d) AS n`)
	if distinct < 5 {
		t.Fatalf("only %d distinct out-degrees; the power-law fixture is not skewed and would "+
			"duplicate the uniform row rather than testing the skewed regime", distinct)
	}
	maxDeg := countOf(t, g,
		`MATCH (a)-[:K]->() WITH a, count(*) AS d RETURN max(d) AS n`)
	avgish := int64(2 * 6) // each node contributes ~mEdges pairs, both directions
	if maxDeg < 3*avgish {
		t.Fatalf("max out-degree %d is not a heavy tail against a mean near %d; the fixture "+
			"lacks the hubs the skewed regime is about", maxDeg, avgish)
	}
	if tri := countOf(t, g, cyclicjoin.TriangleQuery); tri <= 0 {
		t.Fatalf("the power-law fixture contains NO triangles (count=%d) — triadic closure "+
			"is not producing closed motifs", tri)
	}
}

// TestFixtures_DeterministicAcrossBuilds guards the reproducibility of every recorded
// figure: the same seed must produce the same graph, or the report's numbers cannot
// be re-derived.
func TestFixtures_DeterministicAcrossBuilds(t *testing.T) {
	const n = 300
	a, err := cyclicjoin.SeedPowerLaw(n, 5, 0.9, 4242)
	if err != nil {
		t.Fatalf("SeedPowerLaw a: %v", err)
	}
	b, err := cyclicjoin.SeedPowerLaw(n, 5, 0.9, 4242)
	if err != nil {
		t.Fatalf("SeedPowerLaw b: %v", err)
	}
	for _, q := range []string{
		`MATCH ()-[:K]->() RETURN count(*) AS n`,
		cyclicjoin.TriangleQuery,
		cyclicjoin.ClosingQuery,
	} {
		if ca, cb := countOf(t, a, q), countOf(t, b, q); ca != cb {
			t.Fatalf("the same seed produced different graphs for %q: %d vs %d — every recorded "+
				"figure would be unreproducible", q, ca, cb)
		}
	}
}

// TestNonQualifyingQueries_ReturnRows guards the controls. A control that returns no
// rows would show parity trivially and prove nothing about the recognition predicate.
func TestNonQualifyingQueries_ReturnRows(t *testing.T) {
	g, err := cyclicjoin.SeedUniform(200, 4)
	if err != nil {
		t.Fatalf("SeedUniform: %v", err)
	}
	for _, q := range []string{cyclicjoin.LabelledTriangleQuery, cyclicjoin.AcyclicQuery} {
		if n := countOf(t, g, q); n <= 0 {
			t.Fatalf("non-qualifying control %q returned %d rows; a control that matches "+
				"nothing shows parity trivially and proves nothing", q, n)
		}
	}
	// The square must match too, or its row measures the empty case.
	if n := countOf(t, g, cyclicjoin.SquareQuery); n <= 0 {
		t.Fatalf("SquareQuery matched nothing (%d)", n)
	}
}
