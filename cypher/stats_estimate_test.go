package cypher

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index/stats"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// statsTestSource wires a resolver over the engine's graph exactly as the
// read-path build does, so the (inert) providers can be exercised.
func statsTestSource(e *Engine) *lpgLabelResolver {
	return &lpgLabelResolver{g: e.g, eng: e}
}

// seedPersonGraph builds a directed multigraph of n :Person nodes. age is skewed:
// a fraction heavyFrac carry the heavy value 30, the rest are spread uniformly
// over [0,100). name is distinct per node (a high-NDV string column). It returns
// the engine and the exact ground-truth counts.
func seedPersonGraph(t *testing.T, n int, heavyFrac float64) (e *Engine, heavyCount, ltHeavy int64) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("p%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "Person"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		var age int64
		if float64(i%100)/100.0 < heavyFrac {
			age = 30
		} else {
			age = int64((i * 37) % 100)
		}
		if err := g.SetNodeProperty(key, "age", lpg.Int64Value(age)); err != nil {
			t.Fatalf("SetNodeProperty age: %v", err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(fmt.Sprintf("name-%d", i))); err != nil {
			t.Fatalf("SetNodeProperty name: %v", err)
		}
		if age == 30 {
			heavyCount++
		}
		if age < 30 {
			ltHeavy++
		}
	}
	e = NewEngine(g)
	// Sanity: the label index (the exact N denominator) must reflect the seed.
	if got, _ := statsTestSource(e).ResolveLabelCount("Person"); got != int64(n) {
		t.Fatalf("ResolveLabelCount(Person) = %d, want %d — direct seed did not populate the label index", got, n)
	}
	return e, heavyCount, ltHeavy
}

func TestStats_MCVExactAndNDVHeuristic(t *testing.T) {
	const n = 3000
	e, heavyCount, _ := seedPersonGraph(t, n, 0.40)
	src := statsTestSource(e)

	// Absence before a rebuild → estFallback (the safe default).
	if est := statsEqualityEstimate(src, "Person", "age", expr.IntegerValue(30)); est.source != estFallback {
		t.Fatalf("before rebuild: source = %v, want fallback", est.source)
	}

	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	// The heavy value 30 must be an MCV hit with its EXACT count (estExact).
	est := statsEqualityEstimate(src, "Person", "age", expr.IntegerValue(30))
	if est.source != estExact {
		t.Errorf("heavy value 30: source = %v, want exact", est.source)
	}
	if int64(est.rows) != heavyCount {
		t.Errorf("heavy value 30: rows = %v, want exact %d", est.rows, heavyCount)
	}

	// A rare / non-MCV value falls back to the 1/NDV heuristic (a non-gating hint).
	rare := statsEqualityEstimate(src, "Person", "name", expr.StringValue("name-7"))
	if rare.source != estExact && rare.source != estHeuristic {
		t.Errorf("name lookup: source = %v, want exact or heuristic", rare.source)
	}
	// name is a distinct-per-node column: NDV ≈ n, so 1/NDV × N ≈ 1 row.
	heur := statsEqualityEstimate(src, "Person", "name", expr.StringValue("does-not-exist"))
	if heur.source != estHeuristic {
		t.Errorf("absent name: source = %v, want heuristic", heur.source)
	}
	if heur.rows < 0.5 || heur.rows > 3.0 {
		t.Errorf("1/NDV heuristic on a distinct column: rows = %v, want ≈ 1", heur.rows)
	}

	// An unknown label / property → fallback.
	if est := statsEqualityEstimate(src, "Ghost", "age", expr.IntegerValue(1)); est.source != estFallback {
		t.Errorf("unknown label: source = %v, want fallback", est.source)
	}
	if est := statsEqualityEstimate(src, "Person", "ghostprop", expr.IntegerValue(1)); est.source != estFallback {
		t.Errorf("unknown prop: source = %v, want fallback", est.source)
	}
}

func TestStats_RangeEstimateAndCertifiedError(t *testing.T) {
	const n = 5000
	e, _, ltHeavy := seedPersonGraph(t, n, 0.30)
	src := statsTestSource(e)
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	invB := 1.0 / float64(statsHistogramBuckets)

	est, absErr := statsRangeEstimate(src, "Person", "age", stats.OpLt, expr.IntegerValue(30))
	if est.source != estStats {
		t.Errorf("fresh range estimate: source = %v, want stats", est.source)
	}
	// Δ = 0 immediately after rebuild, so the certified error is exactly 1/B.
	if math.Abs(absErr-invB) > 1e-12 {
		t.Errorf("fresh absErr = %v, want 1/B = %v", absErr, invB)
	}
	// The estimate must be within the certified 1/B (as a fraction of N) of ground truth.
	tol := invB*float64(n) + 1
	if math.Abs(est.rows-float64(ltHeavy)) > tol {
		t.Errorf("age<30: rows = %v, truth = %d, |err| = %v > 1/B·N = %v",
			est.rows, ltHeavy, math.Abs(est.rows-float64(ltHeavy)), tol)
	}

	// Int-vs-float boundary equivalence: 30 and 30.0 must yield the same estimate,
	// because the boundary comparison is the exact cmpInt64Float64, not raw IEEE.
	estF, _ := statsRangeEstimate(src, "Person", "age", stats.OpLt, expr.FloatValue(30.0))
	if estF.rows != est.rows {
		t.Errorf("float bound 30.0 rows = %v != int bound 30 rows = %v (cmpInt64Float64 must agree)",
			estF.rows, est.rows)
	}

	// −0.0 == +0.0: the two zero bounds must yield identical estimates.
	estNegZero, _ := statsRangeEstimate(src, "Person", "age", stats.OpLe, expr.FloatValue(math.Copysign(0, -1)))
	estPosZero, _ := statsRangeEstimate(src, "Person", "age", stats.OpLe, expr.FloatValue(0))
	if estNegZero.rows != estPosZero.rows {
		t.Errorf("−0.0 bound rows = %v != +0.0 bound rows = %v (must be equal)", estNegZero.rows, estPosZero.rows)
	}

	// A NaN bound (a range predicate yields null on NaN) is never a trustworthy
	// range estimate.
	estNaN, _ := statsRangeEstimate(src, "Person", "age", stats.OpLt, expr.FloatValue(math.NaN()))
	if estNaN.source != estFallback {
		t.Errorf("NaN bound: source = %v, want fallback", estNaN.source)
	}

	// A string bound on the numeric-only column has no matching histogram domain.
	estWrongDom, _ := statsRangeEstimate(src, "Person", "age", stats.OpLt, expr.StringValue("x"))
	if estWrongDom.source != estFallback {
		t.Errorf("wrong-domain bound: source = %v, want fallback", estWrongDom.source)
	}
}

// TestStats_IntFloatLargeMagnitudeBoundary pins the exact cross-type comparison
// at magnitudes where float64 loses precision: ints 2^53 and 2^53+1 are distinct
// but round to the same float64, and the histogram boundary comparison must honour
// the exact order.
func TestStats_IntFloatLargeMagnitudeBoundary(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	const big = int64(1) << 53
	// 200 nodes at 2^53, 200 at 2^53+1.
	for i := 0; i < 400; i++ {
		key := fmt.Sprintf("b%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(key, "Big"); err != nil {
			t.Fatal(err)
		}
		v := big
		if i >= 200 {
			v = big + 1
		}
		if err := g.SetNodeProperty(key, "v", lpg.Int64Value(v)); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(g)
	src := statsTestSource(e)
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatal(err)
	}
	// v < 2^53 : none (both values are ≥ 2^53).
	est, _ := statsRangeEstimate(src, "Big", "v", stats.OpLt, expr.IntegerValue(big))
	if est.rows != 0 {
		t.Errorf("v < 2^53 rows = %v, want 0", est.rows)
	}
	// v <= 2^53 : exactly the 200 at 2^53 (the 2^53+1 nodes must NOT be counted,
	// which raw float64 widening would wrongly do).
	est, _ = statsRangeEstimate(src, "Big", "v", stats.OpLe, expr.IntegerValue(big))
	if int64(est.rows) != 200 {
		t.Errorf("v <= 2^53 rows = %v, want exactly 200 (exact int/float order)", est.rows)
	}
}

// TestStats_NaNExcludedFromNumerator confirms NaN rows are excluded from the
// histogram's in-domain total, so a range estimate is a fraction of the non-NaN
// rows (a range predicate yields null on NaN).
func TestStats_NaNExcludedFromNumerator(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	// 100 finite values in [0,100), plus 50 NaN.
	for i := 0; i < 150; i++ {
		key := fmt.Sprintf("m%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(key, "M"); err != nil {
			t.Fatal(err)
		}
		v := float64(i)
		if i >= 100 {
			v = math.NaN()
		}
		if err := g.SetNodeProperty(key, "v", lpg.Float64Value(v)); err != nil {
			t.Fatal(err)
		}
	}
	e := NewEngine(g)
	src := statsTestSource(e)
	if err := e.RefreshStatistics(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, ok := lookupStats(src, "M", "v")
	if !ok {
		t.Fatal("no stats for (M,v)")
	}
	h, ok := st.Histogram(statsDomainNumeric)
	if !ok {
		t.Fatal("no numeric histogram")
	}
	// The histogram total must be the 100 finite rows, not 150.
	if h.Total() != 100 {
		t.Errorf("histogram total = %d, want 100 (NaN excluded)", h.Total())
	}
	// NDV still counts NaN as one distinct value: ~101 distinct (0..99 + NaN).
	if d := st.NDV.Estimate(); d < 95 || d > 108 {
		t.Errorf("NDV = %v, want ≈ 101", d)
	}
}

// TestStats_StalenessDemotionEndToEnd drives real engine SET writes and asserts Δ
// crosses the b − 1/B threshold and demotes the range estimate from estStats to
// estFallback — the full write-path → staleness → veto wiring.
func TestStats_StalenessDemotionEndToEnd(t *testing.T) {
	const n = 400
	e, _, _ := seedPersonGraph(t, n, 0.30)
	src := statsTestSource(e)
	ctx := context.Background()
	if err := e.RefreshStatistics(ctx); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	// Freshly rebuilt: the range estimate is trustworthy with a zero staleness term.
	est, _ := statsRangeEstimate(src, "Person", "age", stats.OpLt, expr.IntegerValue(30))
	if est.source != estStats {
		t.Fatalf("fresh: source = %v, want stats", est.source)
	}
	st, _ := lookupStats(src, "Person", "age")
	if st.Delta() != 0 {
		t.Fatalf("fresh Δ = %d, want 0", st.Delta())
	}

	// The firing region closes at Δ/N ≥ b − 1/B.
	threshold := statsRangeBreakEven - 1.0/float64(statsHistogramBuckets)
	crossAt := int64(threshold*float64(n)) + 1

	// Drive engine SET writes on distinct Person nodes; each bumps Δ for (Person, age).
	for i := int64(0); i < crossAt+2; i++ {
		q := fmt.Sprintf("MATCH (p:Person {name:'name-%d'}) SET p.age = %d", i, 200+i)
		if _, err := e.RunInTx(ctx, q, nil); err != nil {
			t.Fatalf("SET write %d: %v", i, err)
		}
	}
	if st.Delta() < crossAt {
		t.Fatalf("after %d SET writes, Δ = %d, want ≥ %d (write-path hook must fire)", crossAt+2, st.Delta(), crossAt)
	}

	// Stale: the range estimate must have demoted to estFallback.
	est, _ = statsRangeEstimate(src, "Person", "age", stats.OpLt, expr.IntegerValue(30))
	if est.source != estFallback {
		t.Errorf("after Δ/N crossed %.4f (Δ=%d N=%d): source = %v, want fallback",
			threshold, st.Delta(), n, est.source)
	}
}

// TestStats_ShipInert proves the statistics change no query result: the same query
// returns identical rows before and after a rebuild (no consumer reads them yet).
func TestStats_ShipInert(t *testing.T) {
	const n = 500
	e, _, _ := seedPersonGraph(t, n, 0.30)
	ctx := context.Background()
	q := "MATCH (p:Person) WHERE p.age < 30 RETURN count(p) AS c"

	beforeRows := drainRows(t, e, q)

	if err := e.RefreshStatistics(ctx); err != nil {
		t.Fatalf("RefreshStatistics: %v", err)
	}

	afterRows := drainRows(t, e, q)

	if len(beforeRows) != len(afterRows) || len(beforeRows) != 1 {
		t.Fatalf("row count changed: before %d after %d", len(beforeRows), len(afterRows))
	}
	if beforeRows[0] != afterRows[0] {
		t.Errorf("statistics changed the result: before %v after %v", beforeRows[0], afterRows[0])
	}
}

// TestStats_RefreshCancellation confirms the scan honours context cancellation
// without publishing a partial snapshot.
func TestStats_RefreshCancellation(t *testing.T) {
	e, _, _ := seedPersonGraph(t, 100, 0.3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.RefreshStatistics(ctx); err == nil {
		t.Error("cancelled RefreshStatistics returned nil, want ctx.Err()")
	}
	// A cancelled rebuild publishes nothing: with the lazy collector (task #2101)
	// it never even allocates one, so the pointer stays nil.
	if c := e.statsCollector.Load(); c != nil && c.Tracking() {
		t.Error("cancelled rebuild published a snapshot")
	}
}
