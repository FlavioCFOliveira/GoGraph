package cypher

// index_nested_loop_plan_test.go — the differential and gate suite for the index
// nested-loop join (rmp #2233).
//
// # What is compared, and against how many references
//
// THREE answers, not two. The operator has to agree with BOTH plans it can
// replace — the plain nested loop and the hash join — because it is admitted in
// place of the hash join, which was itself admitted in place of the nested loop,
// and an error common to a pair would otherwise read as agreement. The comparison
// is the full row SEQUENCE, position by position: like the hash join, this
// substitution is order-PRESERVING, and a multiset comparison would not see it
// break.
//
// A hand-computed match count is asserted on top of that, because all three plans
// share one graph and one key-equality primitive. Two audits in this project lost
// a real defect to a differential whose arms shared the broken code.
//
// # Why the plan is asserted from a counter
//
// Acceptance criterion 4 requires it. Engine.Explain renders the planner's
// intent; #2222 found that intent and the built operator can diverge. Every case
// here asserts which of the three plans actually ran, via
// indexNestedLoopBuildCount and hashJoinBuildCount.

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/index"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// inljFixture seeds n :P nodes with an indexed numeric `age` and an `id`, then
// creates the index through Cypher so the numeric btree companion the operator
// seeks is built exactly as production builds it.
//
// EVERY node carries a numeric age, because that is the planner's admission
// condition: the numeric companion must hold an entry per scanned node
// (numericIndexCoversScan). A fixture with a gap or a string value would be
// DECLINED and the seek would never run — which is itself worth asserting, and
// TestIndexNestedLoopJoin_DeclinesWithoutFullNumericCoverage does.
//
// Within that constraint the distribution is still deliberate:
//   - ages repeat (i % mod), so a seek returns a multi-row posting list and the
//     within-outer-row order is actually exercised;
//   - every 11th carries an integral FLOAT age, so a seek for the integer 3 must
//     also find the node whose age is 3.0 — the cross-type numeric case AC3 names,
//     in both directions.
func inljFixture(t *testing.T, n, mod int) *Engine {
	t.Helper()
	eng := NewEngine(inljFixtureGraph(t, n, mod))
	mustCreateIndex(t, eng)
	return eng
}

// inljAges returns the age each node carries, as the fixture assigned it: an
// int64, a float64, a string, or nil for absent. It is the ORACLE's view of the
// graph, derived from the fixture's own rule rather than read back through the
// engine, so it cannot inherit an engine-side defect.
func inljAges(n, mod int) []any {
	out := make([]any, n)
	for i := 0; i < n; i++ {
		if i%11 == 0 {
			out[i] = float64(i % mod)
		} else {
			out[i] = int64(i % mod)
		}
	}
	return out
}

// inljOracleMatches counts, from the fixture rule alone, how many (outer key,
// node) pairs the join must produce for the given keys — openCypher `=`
// semantics: cross-type numeric equality holds, a string never equals a number,
// and an absent property never equals anything.
func inljOracleMatches(keys []any, n, mod int) int {
	ages := inljAges(n, mod)
	total := 0
	for _, k := range keys {
		for _, a := range ages {
			if a == nil || k == nil {
				continue
			}
			switch key := k.(type) {
			case int64:
				switch age := a.(type) {
				case int64:
					if age == key {
						total++
					}
				case float64:
					if age == float64(key) {
						total++
					}
				}
			case float64:
				if math.IsNaN(key) {
					continue
				}
				switch age := a.(type) {
				case int64:
					if float64(age) == key {
						total++
					}
				case float64:
					if age == key {
						total++
					}
				}
			case string:
				if age, ok := a.(string); ok && age == key {
					total++
				}
			}
		}
	}
	return total
}

// inljRun executes q and renders every row as one comparable string, preserving
// emission order.
func inljRun(t *testing.T, eng *Engine, q string, params map[string]any) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), q, params)
	if err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	var out []string
	for res.Next() {
		var b strings.Builder
		for i := range res.Columns() {
			fmt.Fprintf(&b, "%v\x1f", res.ValueAt(i))
		}
		out = append(out, b.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("run %q: %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("close %q: %v", q, err)
	}
	return out
}

// inljKeyRows wraps raw key values into the UNWIND parameter shape.
func inljKeyRows(keys []any) []any {
	rows := make([]any, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, map[string]any{"a": k})
	}
	return rows
}

// TestIndexNestedLoopJoin_DifferentialAgainstBothPlans is the correctness gate.
func TestIndexNestedLoopJoin_DifferentialAgainstBothPlans(t *testing.T) {
	const (
		n   = 400
		mod = 20
	)
	// B is kept small relative to n so the cost gate admits the seek; the gate
	// itself is tested separately below.
	cases := []struct {
		name string
		keys []any
	}{
		{"integer keys present in the index", []any{int64(1), int64(2), int64(3)}},
		{"a key absent from the index", []any{int64(9999)}},
		{"a NULL key", []any{nil}},
		{"cross-type: integer key against float-valued nodes", []any{int64(0), int64(11)}},
		{"cross-type: float key against integer-valued nodes", []any{float64(2), float64(3.0)}},
		{"a NaN key matches nothing", []any{math.NaN()}},
		// Under proven numeric coverage a string key matches nothing, and the
		// operator establishes that WITHOUT scanning. The oracle agrees for the same
		// reason: no node holds a string age.
		{"a STRING key matches nothing under proven coverage", []any{"s0", "s13"}},
		{"a BOOLEAN key matches nothing", []any{true}},
		{"mixed kinds in one batch", []any{int64(1), "s0", nil, float64(2), int64(9999), true}},
		{"a float key with a fractional part matches nothing", []any{float64(2.5)}},
		{"an integer beyond 2^53 takes the fallback", []any{int64(1) << 60}},
		{"duplicate keys repeat their matches", []any{int64(1), int64(1), int64(1)}},
	}

	const q = `UNWIND $rows AS r MATCH (b:P) WHERE b.age = r.a RETURN r.a AS k, b.id AS bid`

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := map[string]any{"rows": inljKeyRows(tc.keys)}

			eng := inljFixture(t, n, mod)
			beforeINLJ := indexNestedLoopBuildCount.Load()
			beforeHJ := hashJoinBuildCount.Load()
			gotSeek := inljRun(t, eng, q, params)
			if indexNestedLoopBuildCount.Load() == beforeINLJ {
				t.Fatalf("the index nested-loop join did not fire, so this case is comparing "+
					"the hash join with itself and proves nothing (B=%d, N=%d)", len(tc.keys), n)
			}
			if hashJoinBuildCount.Load() != beforeHJ {
				t.Fatal("the hash join ALSO fired; the two substitutions must be exclusive")
			}

			// Reference 1: the hash join.
			hjEng := inljFixture(t, n, mod)
			beforeHJ = hashJoinBuildCount.Load()
			beforeINLJ = indexNestedLoopBuildCount.Load()
			gotHash := inljRunWithoutINLJ(t, hjEng, q, params)
			if hashJoinBuildCount.Load() == beforeHJ {
				t.Fatal("the hash-join reference arm did not use the hash join")
			}
			if indexNestedLoopBuildCount.Load() != beforeINLJ {
				t.Fatal("the seek fired on the hash-join reference arm")
			}

			// Reference 2: the plain nested loop.
			nlEng := NewEngineWithOptions(inljFixtureGraph(t, n, mod), EngineOptions{DisableHashJoin: true})
			mustCreateIndex(t, nlEng)
			beforeHJ = hashJoinBuildCount.Load()
			beforeINLJ = indexNestedLoopBuildCount.Load()
			gotNested := inljRun(t, nlEng, q, params)
			if hashJoinBuildCount.Load() != beforeHJ || indexNestedLoopBuildCount.Load() != beforeINLJ {
				t.Fatal("the nested-loop reference arm took a substituted plan; DisableHashJoin " +
					"must switch off both")
			}

			assertSameSequence(t, "index nested-loop join", gotSeek, "hash join", gotHash)
			assertSameSequence(t, "index nested-loop join", gotSeek, "nested loop", gotNested)

			// The ABSOLUTE oracle, computed from the fixture rule alone.
			want := inljOracleMatches(tc.keys, n, mod)
			if len(gotSeek) != want {
				t.Fatalf("row count %d, oracle says %d — all three plans agreeing on a wrong "+
					"count means the defect is shared, which is exactly what this oracle is for",
					len(gotSeek), want)
			}
		})
	}
}

// assertSameSequence compares two row sequences position by position.
func assertSameSequence(t *testing.T, nameA string, a []string, nameB string, b []string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("row COUNT differs: %s %d, %s %d", nameA, len(a), nameB, len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("ORDER DIVERGED at row %d of %d:\n  %s %q\n  %s %q\n"+
				"The index nested-loop join must emit the same sequence as the plan it "+
				"replaces — it is outer-major with ascending node ids within an outer row.",
				i, len(a), nameA, a[i], nameB, b[i])
		}
	}
}

// inljRunWithoutINLJ runs q on an engine whose index nested-loop join is
// suppressed, leaving the hash join to serve the shape. Suppression is done by
// clearing the build-time gate rather than by changing the query, so the
// reference arm runs the SAME plan shape the seek arm would have.
func inljRunWithoutINLJ(t *testing.T, eng *Engine, q string, params map[string]any) []string {
	t.Helper()
	eng.disableIndexNestedLoopForTest = true
	defer func() { eng.disableIndexNestedLoopForTest = false }()
	eng.ClearPlanCache()
	out := inljRun(t, eng, q, params)
	eng.ClearPlanCache()
	return out
}

// inljFixtureGraph is the SINGLE fixture builder. Both the seek arm and the
// nested-loop reference arm come from it, which is not a tidiness point: an
// earlier version of this file had two builders, they drifted apart, and the
// differential then compared answers from two DIFFERENT graphs — reporting a row
// count mismatch that looked exactly like an operator defect.
//
// EVERY node carries a numeric age, because that is the planner's admission
// condition: the numeric companion must hold an entry per scanned node
// (numericIndexCoversScan). A fixture with a gap or a string value would be
// DECLINED and the seek would never run — which
// TestIndexNestedLoopJoin_DeclinesWithoutFullNumericCoverage asserts separately.
//
// Within that constraint the distribution is still deliberate: ages repeat
// (i % mod) so a seek returns a multi-row posting list and the within-outer-row
// order is exercised, and every 11th age is an integral FLOAT so a seek for the
// integer 3 must also find the node whose age is 3.0 — the cross-type numeric case
// AC3 names, in both directions.
func inljFixtureGraph(t *testing.T, n, mod int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	g.SetIndexManager(index.NewManager())
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("n%d", i)
		if err := g.AddNode(key); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(key, "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "id", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty(id): %v", err)
		}
		if i%11 == 0 {
			if err := g.SetNodeProperty(key, "age", lpg.Float64Value(float64(i%mod))); err != nil {
				t.Fatalf("SetNodeProperty(age float): %v", err)
			}
		} else if err := g.SetNodeProperty(key, "age", lpg.Int64Value(int64(i%mod))); err != nil {
			t.Fatalf("SetNodeProperty(age int): %v", err)
		}
	}
	return g
}

func mustCreateIndex(t *testing.T, eng *Engine) {
	t.Helper()
	if _, err := eng.RunAny(context.Background(), `CREATE INDEX p_age FOR (x:P) ON (x.age)`, nil); err != nil {
		t.Fatalf("CREATE INDEX: %v", err)
	}
}

// TestIndexNestedLoopJoin_CostGatePicksTheRightPlan is the gate's own test, and
// the one that keeps #2228's 957× safe: at the large-batch shape the hash join
// must still win.
func TestIndexNestedLoopJoin_CostGatePicksTheRightPlan(t *testing.T) {
	const mod = 20
	cases := []struct {
		name string
		n, b int
		// wantSeek and wantHash together say WHICH plan ran, so a case cannot pass
		// by neither substitution firing when the point is that one of them chose.
		wantSeek bool
		wantHash bool
	}{
		{"small batch against a large population: seek", 20000, 500, true, false},
		// MEASURED, not derived: the unit-count model #2228 recorded said the hash
		// join led here (25 000 units against 71 500), and the A/B in
		// docs/benchmarks/index-nested-loop-join-2026-07-28.md puts the seek 6.8×
		// AHEAD. The gate's constant is calibrated from that measurement, so this
		// cell now takes the seek. If it ever flips back, re-run
		// TestIndexNestedLoopCrossover before changing the constant.
		{"large batch against the same population: still the seek", 20000, 5000, true, false},
		{"batch as large as the population: still the seek", 400, 400, true, false},
		// Below indexNestedLoopMinPopulation neither substitution is offered: the
		// seek has its own floor and the hash join's (hashJoinSizeFloor) is the same
		// 64, so this is the plain nested loop — the correct plan for a population
		// that fits in a handful of cache lines.
		{"population below both floors: plain nested loop", 40, 4, false, false},
	}
	const q = `UNWIND $rows AS r MATCH (b:P) WHERE b.age = r.a RETURN count(*) AS n`

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys := make([]any, tc.b)
			for i := range keys {
				keys[i] = int64(i % mod)
			}
			eng := inljFixture(t, tc.n, mod)

			beforeINLJ := indexNestedLoopBuildCount.Load()
			beforeHJ := hashJoinBuildCount.Load()
			inljRun(t, eng, q, map[string]any{"rows": inljKeyRows(keys)})
			gotSeek := indexNestedLoopBuildCount.Load() > beforeINLJ
			gotHash := hashJoinBuildCount.Load() > beforeHJ

			if gotSeek != tc.wantSeek {
				t.Fatalf("index nested-loop join fired = %v, want %v (B=%d, N=%d). The gate is "+
					"what keeps the Θ(N+B) plan for a large batch — #2228's 2.206s bulk load "+
					"depends on it", gotSeek, tc.wantSeek, tc.b, tc.n)
			}
			if gotHash != tc.wantHash {
				t.Fatalf("hash join fired = %v, want %v (B=%d, N=%d) — the case is pinned to a "+
					"SPECIFIC plan, so it cannot pass by falling through to a third one",
					gotHash, tc.wantHash, tc.b, tc.n)
			}
			if gotSeek && gotHash {
				t.Fatal("both substitutions fired; they must be exclusive")
			}
		})
	}
}

// TestIndexNestedLoopWins pins the gate's arithmetic directly, including the two
// figures #2228's decision record recorded.
func TestIndexNestedLoopWins(t *testing.T) {
	cases := []struct {
		name       string
		outer, pop int
		want       bool
	}{
		{"B=500, N=20000: seek, by a measured 40.2×", 500, 20000, true},
		// The cell the unit model got backwards. Measured 2.051ms for the seek
		// against 13.851ms for the hash join — 6.8× — so the calibrated gate must
		// award it to the seek.
		{"B=5000, N=20000: seek, by a measured 6.8×", 5000, 20000, true},
		{"B == N: seek, by a measured 2.4×", 20000, 20000, true},
		{"B = 25×N: seek, by a measured 1.15× — still ahead", 500000, 20000, true},
		{"a single outer row against a large population", 1, 80000, true},
		// The far side of the boundary the gate exists to hold. Beyond it the seek's
		// narrowing advantage is projected to invert; it is outside the measured
		// range, which is exactly why a gate is kept rather than removed.
		{"B far beyond the measured range against a deep index: hash join", 40_000_000, 80000, false},
		{"zero rows is never a win", 0, 20000, false},
		{"an empty population is never a win", 500, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := indexNestedLoopWins(tc.outer, tc.pop); got != tc.want {
				t.Fatalf("indexNestedLoopWins(%d, %d) = %v, want %v (seek %.0f vs hash %d)",
					tc.outer, tc.pop, got, tc.want,
					float64(tc.outer)*math.Log2(math.Max(float64(tc.pop), 1)), tc.pop+tc.outer)
			}
		})
	}
}

// TestIndexNestedLoopJoin_DeclinesWithoutFullNumericCoverage is the regression gate
// for the worst defect this task produced before measurement caught it.
//
// The first version of the planner admitted the seek whenever a numeric companion
// EXISTED. A companion exists for every Cypher-created index, including one on a
// string-valued property — where it is empty. So a string-keyed bulk-load join was
// admitted, every outer row fell through to the operator's scan fallback, and the
// shape went Θ(B·N): the three-way load harness measured 9.157 ms → 3.336 s at
// N=16000, a 364× regression, which is precisely what #2233's "do not add the
// operator without the gate" warned of.
//
// The fix is a COVERAGE proof: the companion must hold an entry per scanned node.
// Each case below is a population the proof must reject, and rejecting it means
// falling through to the hash join — which must therefore be observed firing,
// otherwise the case could pass by failing some unrelated part of the trigger.
func TestIndexNestedLoopJoin_DeclinesWithoutFullNumericCoverage(t *testing.T) {
	const (
		n = 400
		b = 20
	)
	cases := []struct {
		name string
		// age assigns the indexed property; ok == false leaves it unset.
		age func(i int) (lpg.PropertyValue, bool)
	}{
		{
			name: "a string-valued property: the companion is empty",
			age: func(i int) (lpg.PropertyValue, bool) {
				return lpg.StringValue(fmt.Sprintf("s%d", i%20)), true
			},
		},
		{
			name: "one node in fourteen has no value at all",
			age: func(i int) (lpg.PropertyValue, bool) {
				if i%14 == 0 {
					// The value is ignored when set is false; PropertyValue is a concrete
					// type, so there is no nil to return.
					return lpg.Int64Value(0), false
				}
				return lpg.Int64Value(int64(i % 20)), true
			},
		},
		{
			name: "one node in fourteen holds a string among the numbers",
			age: func(i int) (lpg.PropertyValue, bool) {
				if i%14 == 0 {
					return lpg.StringValue("x"), true
				}
				return lpg.Int64Value(int64(i % 20)), true
			},
		},
	}

	const q = `UNWIND $rows AS r MATCH (x:P) WHERE x.age = r.a RETURN count(x)`
	keys := make([]any, b)
	for i := range keys {
		keys[i] = int64(i % 20)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
			g.SetIndexManager(index.NewManager())
			for i := 0; i < n; i++ {
				key := fmt.Sprintf("n%d", i)
				if err := g.AddNode(key); err != nil {
					t.Fatalf("AddNode: %v", err)
				}
				if err := g.SetNodeLabel(key, "P"); err != nil {
					t.Fatalf("SetNodeLabel: %v", err)
				}
				if v, set := tc.age(i); set {
					if err := g.SetNodeProperty(key, "age", v); err != nil {
						t.Fatalf("SetNodeProperty: %v", err)
					}
				}
			}
			eng := NewEngine(g)
			mustCreateIndex(t, eng)

			beforeINLJ := indexNestedLoopBuildCount.Load()
			beforeHJ := hashJoinBuildCount.Load()
			inljRun(t, eng, q, map[string]any{"rows": inljKeyRows(keys)})

			if indexNestedLoopBuildCount.Load() != beforeINLJ {
				t.Fatal("the seek was admitted without proven numeric coverage. Every outer row " +
					"whose key the companion cannot serve then drives a FULL LABEL SCAN, which is " +
					"the Θ(B·N) regression measured at 364× on the three-way load harness")
			}
			if hashJoinBuildCount.Load() == beforeHJ {
				t.Fatal("neither plan was substituted, so this case does not show the COVERAGE " +
					"proof declining — it may have failed the structural trigger instead")
			}
		})
	}
}
