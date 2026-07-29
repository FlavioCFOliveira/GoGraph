package expandinto_test

// fixture_test.go — the guards that keep this benchmark package honest (#2152).
//
// Layer: short.
//
// A benchmark harness is code, and a wrong harness reports a wrong number with full
// confidence. This sprint has two recorded instances already: the audit figure the
// sprint was opened on was not reproducible, and this package's own OpenControl
// benchmark was first sized so that one operation cost 36 seconds and measured row
// construction rather than the access path. Both were caught by checking the harness
// against something independent, so every property the benchmarks rely on is asserted
// here rather than trusted.
//
// The properties that matter are: the fixtures have the DEGREES the benchmark names
// claim, the closing queries actually match something when they are supposed to (and
// nothing when they are not), and the anchor-swap fixture really is lopsided in the
// direction the cost model is supposed to notice.

import (
	"context"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/bench/expandinto"
	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// scalar runs q, which must return exactly one row of one column, and returns that
// cell as an int64.
func scalar(t *testing.T, g *lpg.Graph[string, float64], q string) int64 {
	t.Helper()
	eng := cypher.NewEngine(g)
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	defer func() { _ = res.Close() }()
	if !res.Next() {
		t.Fatalf("Run(%q) returned no rows", q)
	}
	iv, ok := res.ValueAt(0).(expr.IntegerValue)
	if !ok {
		// Fail rather than coerce: a silent zero from an unexpected type would make
		// every count assertion below pass for the wrong reason.
		t.Fatalf("Run(%q) first column has type %T, want expr.IntegerValue", q, res.ValueAt(0))
	}
	got := int64(iv)
	if res.Next() {
		t.Fatalf("Run(%q) returned more than one row", q)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	return got
}

func TestSeedRing_HasTheClaimedShape(t *testing.T) {
	const (
		n      = 40
		degree = 4
	)
	for _, mutual := range []bool{false, true} {
		g, err := expandinto.SeedRing(n, degree, mutual)
		if err != nil {
			t.Fatalf("SeedRing(%d, %d, %v): %v", n, degree, mutual, err)
		}
		if got := scalar(t, g, `MATCH (p:P) RETURN count(p) AS n`); got != n {
			t.Fatalf("mutual=%v: node count = %d, want %d", mutual, got, n)
		}
		// Out-degree is `degree`, plus one back-edge per node when mutual.
		wantEdges := int64(n * degree)
		if mutual {
			wantEdges += int64(n)
		}
		if got := scalar(t, g, `MATCH ()-[r:K]->() RETURN count(r) AS n`); got != wantEdges {
			t.Fatalf("mutual=%v: K edge count = %d, want %d", mutual, got, wantEdges)
		}
	}
}

// TestSeedRing_MutualDecidesWhetherCyclesExist is the assertion that stops the closing
// benchmarks from being vacuous.
//
// With a plain ring of small degree there is NO 2-cycle: closing needs
// d1 + d2 ≡ 0 (mod n), and d1, d2 ≤ degree ≪ n. The shipped
// cypher/expand_into_bench_test.go fixture claims "arranged so that cycles exist" and
// is wrong about it for exactly this reason. That fixture is still a valid measurement —
// it is the pure-waste worst case, where the enumeration finds nothing — but a reader
// must not mistake it for one that also emits rows, so both cases are pinned.
func TestSeedRing_MutualDecidesWhetherCyclesExist(t *testing.T) {
	const (
		n      = 40
		degree = 4
	)
	plain, err := expandinto.SeedRing(n, degree, false)
	if err != nil {
		t.Fatalf("SeedRing plain: %v", err)
	}
	if got := scalar(t, plain, expandinto.ClosingQuery); got != 0 {
		t.Fatalf("a plain ring of degree %d over %d nodes closed %d 2-cycles; it must close "+
			"none, since d1+d2 <= %d < %d cannot be 0 mod n", degree, n, got, 2*degree, n)
	}

	mutual, err := expandinto.SeedRing(n, degree, true)
	if err != nil {
		t.Fatalf("SeedRing mutual: %v", err)
	}
	// Each node's back-edge to k−1 pairs with that node's forward edge to k, so every
	// node contributes one 2-cycle in each direction: 2n ordered (a,b) pairs.
	if want := int64(2 * n); scalar(t, mutual, expandinto.ClosingQuery) != want {
		t.Fatalf("the mutual ring closed %d 2-cycles, want %d — the closing benchmarks would "+
			"not be measuring a shape that emits rows",
			scalar(t, mutual, expandinto.ClosingQuery), want)
	}
	// The open control must match on both fixtures, or it is not a control.
	if got := scalar(t, mutual, expandinto.OpenControlQuery); got == 0 {
		t.Fatal("the open control matched nothing on the mutual ring, so it cannot bound how " +
			"much of a paired difference is the fixture")
	}
	if got := scalar(t, mutual, expandinto.TriangleQuery); got == 0 {
		t.Fatal("the mutual ring closed no triangle, so BenchmarkTriangle would measure an " +
			"empty result rather than the shape it names")
	}
}

// TestSeedReverseHub_IsLopsidedInTheDirectionThatMatters pins the property the
// symmetric anchor swap's benchmark depends on: the written anchor must be the
// EXPENSIVE one.
//
// If the hub's out-degree were not far larger than the Leaf label's total in-degree,
// the cost model would rightly decline the swap and the benchmark would measure nothing.
func TestSeedReverseHub_IsLopsidedInTheDirectionThatMatters(t *testing.T) {
	const (
		hubOut = 200
		nLeaf  = 50
	)
	g, err := expandinto.SeedReverseHub(hubOut, nLeaf)
	if err != nil {
		t.Fatalf("SeedReverseHub(%d, %d): %v", hubOut, nLeaf, err)
	}
	if got := scalar(t, g, `MATCH (:Hub)-[r:R]->() RETURN count(r) AS n`); got != hubOut {
		t.Fatalf("hub out-degree = %d, want %d", got, hubOut)
	}
	if got := scalar(t, g, `MATCH (l:Leaf) RETURN count(l) AS n`); got != nLeaf {
		t.Fatalf("Leaf count = %d, want %d", got, nLeaf)
	}
	// Exactly ONE R edge lands on the whole Leaf label. That asymmetry — hubOut against
	// 1 — is what the swap exploits.
	if got := scalar(t, g, `MATCH ()-[r:R]->(:Leaf) RETURN count(r) AS n`); got != 1 {
		t.Fatalf("R edges into :Leaf = %d, want exactly 1; without that asymmetry the cost "+
			"model has no reason to re-root and the benchmark measures nothing", got)
	}
	// And the query the benchmark runs must return that single row on both plans.
	if got := scalar(t, g, `MATCH (a:Hub)-[:R]->(b:Leaf) RETURN count(*) AS n`); got != 1 {
		t.Fatalf("the swap query matched %d rows, want 1", got)
	}
}

// TestFitExponent_RecoversAKnownPowerLaw complements the NaN-rejection cases with the
// positive direction: a series the harness is meant to characterise must come back with
// the right exponent, including a sub-linear and a super-quadratic one.
func TestFitExponent_RecoversAKnownPowerLaw(t *testing.T) {
	degrees := []int{2, 4, 8, 16, 32}
	for _, want := range []float64{0.5, 1, 1.5, 2, 3} {
		costs := make([]float64, len(degrees))
		for i, d := range degrees {
			costs[i] = math.Pow(float64(d), want)
		}
		if got := expandinto.FitExponent(degrees, costs); math.Abs(got-want) > 1e-9 {
			t.Fatalf("FitExponent on an exact d^%v series = %v", want, got)
		}
	}
}
