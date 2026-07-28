package cypher

// hash_join_diff_test.go — differential and guard tests for the disconnected
// equi-join hash join (#1506).
//
// The differential test runs each representative query with the hash join
// ENABLED and DISABLED and asserts an IDENTICAL result multiset. The result
// order is unspecified for these queries (no ORDER BY), so both result sets are
// sorted to canonical form before comparison — a hash join legitimately changes
// emission order, and the test must accept that while proving the multiset is
// the same. A separate assertion confirms the optimisation was actually engaged
// (hashJoinBuildCount advanced) for the enabled run of the large cases, so the
// test cannot silently pass by never triggering.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// hjProp is one node's label + join-property assignment for the test graph
// builder. A nil value omits the property (so the join key evaluates to NULL).
type hjProp struct {
	label string
	value lpg.PropertyValue // zero value => omit property
	set   bool
}

// buildHJTestGraph creates a graph of A-labelled and B-labelled nodes carrying
// join properties a.x / b.y. as and bs index by position.
func buildHJTestGraph(t *testing.T, as, bs []hjProp) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	for i, p := range as {
		k := fmt.Sprintf("a%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, p.label); err != nil {
			t.Fatal(err)
		}
		if p.set {
			if err := g.SetNodeProperty(k, "x", p.value); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i, p := range bs {
		k := fmt.Sprintf("b%d", i)
		if err := g.AddNode(k); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(k, p.label); err != nil {
			t.Fatal(err)
		}
		if p.set {
			if err := g.SetNodeProperty(k, "y", p.value); err != nil {
				t.Fatal(err)
			}
		}
	}
	return g
}

// drainSorted runs q and returns every row rendered as a canonical string,
// sorted so two result sets that differ only in order compare equal.
func drainSorted(t *testing.T, e *Engine, q string) []string {
	t.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	cols := res.Columns()
	var out []string
	for res.Next() {
		rec := res.Record()
		var sb []byte
		for i, c := range cols {
			if i > 0 {
				sb = append(sb, '|')
			}
			sb = append(sb, fmt.Sprintf("%s=%v", c, rec[c])...)
		}
		out = append(out, string(sb))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
	sort.Strings(out)
	return out
}

// assertIdenticalMultiset runs q on a hash-join-ENABLED and a hash-join-DISABLED
// engine over the same graph and asserts the sorted result rows are identical.
// When wantTrigger is true it also asserts the enabled run actually substituted
// a hash join.
func assertIdenticalMultiset(t *testing.T, g *lpg.Graph[string, float64], q string, wantTrigger bool) {
	t.Helper()
	on := NewEngine(g)
	off := NewEngineWithOptions(g, EngineOptions{DisableHashJoin: true})

	before := hashJoinBuildCount.Load()
	gotOn := drainSorted(t, on, q)
	triggered := hashJoinBuildCount.Load() > before
	gotOff := drainSorted(t, off, q)

	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch for %q: hashjoin=%d nestedloop=%d", q, len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("row %d differs for %q:\n  hashjoin   = %s\n  nestedloop = %s", i, q, gotOn[i], gotOff[i])
		}
	}
	if wantTrigger && !triggered {
		t.Fatalf("expected hash join to be substituted for %q, but it was not (count did not advance)", q)
	}
	if !wantTrigger && triggered {
		t.Fatalf("did NOT expect hash join to be substituted for %q, but it was", q)
	}
}

func intP(v int64) hjProp   { return hjProp{label: "A", value: lpg.Int64Value(v), set: true} }
func intPB(v int64) hjProp  { return hjProp{label: "B", value: lpg.Int64Value(v), set: true} }
func fltP(v float64) hjProp { return hjProp{label: "A", value: lpg.Float64Value(v), set: true} }

// makeRange builds n A-nodes and m B-nodes with integer keys modulo `mod`, so
// the join has predictable multiplicity and the build side clears the size floor.
func makeRange(t *testing.T, n, m, mod int) *lpg.Graph[string, float64] {
	as := make([]hjProp, n)
	bs := make([]hjProp, m)
	for i := 0; i < n; i++ {
		as[i] = intP(int64(i % mod))
	}
	for i := 0; i < m; i++ {
		bs[i] = intPB(int64(i % mod))
	}
	return buildHJTestGraph(t, as, bs)
}

func TestHashJoin_Differential_MultiRowBothSides(t *testing.T) {
	g := makeRange(t, 100, 80, 10)
	q := "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax, b.y AS bv"
	assertIdenticalMultiset(t, g, q, true)
}

func TestHashJoin_Differential_NoMatch(t *testing.T) {
	// A keys in [0,10), B keys in [100,110): no overlap, empty result.
	as := make([]hjProp, 100)
	bs := make([]hjProp, 100)
	for i := 0; i < 100; i++ {
		as[i] = intP(int64(i % 10))
		bs[i] = intPB(int64(100 + i%10))
	}
	g := buildHJTestGraph(t, as, bs)
	q := "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax, b.y AS bv"
	assertIdenticalMultiset(t, g, q, true)
}

func TestHashJoin_Differential_NullKeys(t *testing.T) {
	// Some A and B nodes omit the join property → NULL key → must match nothing.
	as := make([]hjProp, 100)
	bs := make([]hjProp, 100)
	for i := 0; i < 100; i++ {
		if i%3 == 0 {
			as[i] = hjProp{label: "A"} // no x property
		} else {
			as[i] = intP(int64(i % 5))
		}
		if i%4 == 0 {
			bs[i] = hjProp{label: "B"} // no y property
		} else {
			bs[i] = intPB(int64(i % 5))
		}
	}
	g := buildHJTestGraph(t, as, bs)
	q := "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax, b.y AS bv"
	assertIdenticalMultiset(t, g, q, true)
}

func TestHashJoin_Differential_CrossTypeNumeric(t *testing.T) {
	// A nodes carry integer keys, B nodes carry float keys. openCypher numeric
	// equality treats 1 = 1.0 as true, so the join MUST match across types.
	as := make([]hjProp, 100)
	bs := make([]hjProp, 100)
	for i := 0; i < 100; i++ {
		as[i] = intP(int64(i % 8))    // integers 0..7
		bs[i] = fltPB(float64(i % 8)) // floats 0.0..7.0
	}
	g := buildHJTestGraph(t, as, bs)
	q := "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax, b.y AS bv"
	assertIdenticalMultiset(t, g, q, true)
}

func fltPB(v float64) hjProp { return hjProp{label: "B", value: lpg.Float64Value(v), set: true} }

func TestHashJoin_Differential_CrossTypeNonNumericNoMatch(t *testing.T) {
	// A nodes carry integer keys, B nodes carry string keys that look numeric.
	// openCypher: "1" != 1 → no match. The hash join must NOT over-match.
	as := make([]hjProp, 80)
	bs := make([]hjProp, 80)
	for i := 0; i < 80; i++ {
		as[i] = intP(int64(i % 5))
		bs[i] = hjProp{label: "B", value: lpg.StringValue(fmt.Sprintf("%d", i%5)), set: true}
	}
	g := buildHJTestGraph(t, as, bs)
	q := "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax, b.y AS bv"
	assertIdenticalMultiset(t, g, q, true)
}

func TestHashJoin_Differential_NaNKeys(t *testing.T) {
	// NaN keys must match nothing (NaN = NaN is false in openCypher).
	as := make([]hjProp, 80)
	bs := make([]hjProp, 80)
	for i := 0; i < 80; i++ {
		if i%2 == 0 {
			as[i] = fltP(math.NaN())
		} else {
			as[i] = fltP(float64(i % 4))
		}
		if i%2 == 0 {
			bs[i] = fltPB(math.NaN())
		} else {
			bs[i] = fltPB(float64(i % 4))
		}
	}
	g := buildHJTestGraph(t, as, bs)
	q := "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax, b.y AS bv"
	assertIdenticalMultiset(t, g, q, true)
}

func TestHashJoin_Differential_ResidualPredicate(t *testing.T) {
	// An additional non-join conjunct must be re-applied as a Filter.
	g := makeRange(t, 100, 100, 10)
	q := "MATCH (a:A),(b:B) WHERE a.x = b.y AND a.x > 4 RETURN a.x AS ax, b.y AS bv"
	assertIdenticalMultiset(t, g, q, true)
}

func TestHashJoin_Differential_IdEquiJoin(t *testing.T) {
	// id(a) = id(b) is an equi-join on the synthetic identity. Since a and b come
	// from disjoint label sets here, the only matches are where the two scans
	// land on the same physical node — but A and B are disjoint, so the result is
	// empty, and must be empty under both plans.
	g := makeRange(t, 100, 100, 10)
	q := "MATCH (a:A),(b:B) WHERE id(a) = id(b) RETURN id(a) AS ia"
	assertIdenticalMultiset(t, g, q, true)
}

// TestHashJoin_BareLimitTakesTheHashJoin replaces
// TestHashJoin_Guard_BareLimitKeepsNestedLoop, which asserted the opposite: that
// a bare LIMIT without ORDER BY disabled the substitution for the whole query.
//
// rmp #2234 retired that guard. The reasoning it rested on was that a bare LIMIT
// makes the SPECIFIC rows observable — true — and that the hash join changes which
// rows those are — false. The substitution emits the nested loop's row sequence
// position for position ([hashJoinBuildOnLeft]), so the same LIMIT selects the
// same prefix. This test asserts exactly that: the join fires, AND the five rows
// are the nested loop's five rows in the nested loop's order.
//
// A bare LIMIT is the sharpest shape in the change. openCypher 9 §8.4 leaves which
// rows come back unspecified absent ORDER BY, so a divergence would be conformant
// and would still be a visible behaviour change — which is why this compares the
// prefix rather than settling for the row count.
func TestHashJoin_BareLimitTakesTheHashJoin(t *testing.T) {
	const q = "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax, b.y AS bv LIMIT 5"
	g := makeRange(t, 100, 100, 10)
	on := NewEngine(g)
	off := NewEngineWithOptions(g, EngineOptions{DisableHashJoin: true})

	before := hashJoinBuildCount.Load()
	gotOn := drainOrdered(t, on, q)
	if hashJoinBuildCount.Load() == before {
		t.Fatal("the hash join did NOT fire under a bare LIMIT; rmp #2234 retired the guard " +
			"that used to prevent it, so either the retirement regressed or another guard " +
			"(size floor, structural trigger) is now rejecting this shape")
	}

	before = hashJoinBuildCount.Load()
	gotOff := drainOrdered(t, off, q)
	if hashJoinBuildCount.Load() != before {
		t.Fatal("the hash join fired on the OFF arm; DisableHashJoin is not taking effect")
	}

	if len(gotOn) != 5 {
		t.Fatalf("expected 5 rows under LIMIT 5, got %d", len(gotOn))
	}
	if len(gotOff) != 5 {
		t.Fatalf("the nested-loop reference returned %d rows under LIMIT 5, not 5", len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("the LIMIT prefix DIVERGED at row %d:\n  hash join   %s\n  nested loop %s\n"+
				"A bare LIMIT selects a prefix of the emitted sequence, so this means the hash "+
				"join is no longer emitting the nested loop's sequence — see hashJoinBuildOnLeft.",
				i, gotOn[i], gotOff[i])
		}
	}
}

func TestHashJoin_Guard_OrderByIsSafe(t *testing.T) {
	// ORDER BY above the join re-establishes a defined order, so the hash join is
	// safe and the (ordered) results must be identical between plans.
	g := makeRange(t, 100, 80, 10)
	q := "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax, b.y AS bv ORDER BY ax, bv"
	// Ordered output: compare WITHOUT re-sorting to also prove the order matches.
	on := NewEngine(g)
	off := NewEngineWithOptions(g, EngineOptions{DisableHashJoin: true})
	before := hashJoinBuildCount.Load()
	gotOn := drainOrdered(t, on, q)
	triggered := hashJoinBuildCount.Load() > before
	gotOff := drainOrdered(t, off, q)
	if !triggered {
		t.Fatalf("expected hash join to be substituted under ORDER BY")
	}
	if len(gotOn) != len(gotOff) {
		t.Fatalf("row-count mismatch: %d vs %d", len(gotOn), len(gotOff))
	}
	for i := range gotOn {
		if gotOn[i] != gotOff[i] {
			t.Fatalf("ordered row %d differs:\n  hashjoin   = %s\n  nestedloop = %s", i, gotOn[i], gotOff[i])
		}
	}
}

// TestHashJoin_CollectTakesTheHashJoin replaces
// TestHashJoin_Guard_CollectKeepsNestedLoop, which asserted the opposite. The old
// reasoning: collect() materialises rows in arrival order, so a hash join below it
// would change the list value. The first half is right and the second does not
// follow — the substitution preserves arrival order exactly, so the list is
// unchanged (rmp #2234).
//
// collect() is the strongest available witness for that claim: it exposes the
// ENTIRE emitted sequence as one comparable value, rather than a prefix or an
// order-blind aggregate.
//
// Checked three ways, the third independent of both plans: the join fires; the
// list matches the nested loop's element for element; and its length matches a
// hand-computed match count. makeRange(100, 100, 10) gives 100 A nodes with
// x = i%10 and 100 B nodes with y = i%10, so each of the 10 keys pairs 10 A rows
// with 10 B rows — 10 × 10 × 10 = 1000 matches. Without that third check a defect
// affecting both arms equally would pass unnoticed, which is how two earlier
// audits in this project lost a real defect.
func TestHashJoin_CollectTakesTheHashJoin(t *testing.T) {
	const (
		q             = "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN collect(a.x) AS xs"
		wantMatches   = 1000 // 10 keys × 10 A rows × 10 B rows
		collectAsRows = "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax"
	)
	g := makeRange(t, 100, 100, 10)
	on := NewEngine(g)
	off := NewEngineWithOptions(g, EngineOptions{DisableHashJoin: true})

	before := hashJoinBuildCount.Load()
	gotOn := drainOrdered(t, on, q)
	if hashJoinBuildCount.Load() == before {
		t.Fatal("the hash join did NOT fire above a collect(); rmp #2234 retired the guard " +
			"that used to prevent it")
	}

	before = hashJoinBuildCount.Load()
	gotOff := drainOrdered(t, off, q)
	if hashJoinBuildCount.Load() != before {
		t.Fatal("the hash join fired on the OFF arm; DisableHashJoin is not taking effect")
	}

	// One row, holding the whole collected list.
	if len(gotOn) != 1 || len(gotOff) != 1 {
		t.Fatalf("expected exactly one row from an aggregating query, got %d / %d",
			len(gotOn), len(gotOff))
	}
	if gotOn[0] != gotOff[0] {
		t.Fatalf("the COLLECTED LIST differs between plans, so the substitution changed the "+
			"arrival order collect() captures:\n  hash join   %s\n  nested loop %s",
			gotOn[0], gotOff[0])
	}
	// The absolute oracle: the list length, which neither plan gets to define.
	if n := len(drainOrdered(t, on, collectAsRows)); n != wantMatches {
		t.Fatalf("the join produced %d rows, but the fixture admits exactly %d matches "+
			"(10 keys × 10 A × 10 B) — the collected list above is the wrong length, and the "+
			"two plans agreeing on it means BOTH are wrong", n, wantMatches)
	}
}

func TestHashJoin_Guard_SizeFloor(t *testing.T) {
	// Tiny build side (below the floor) must keep the nested loop.
	g := makeRange(t, 4, 4, 2)
	on := NewEngine(g)
	before := hashJoinBuildCount.Load()
	got := drainSorted(t, on, "MATCH (a:A),(b:B) WHERE a.x = b.y RETURN a.x AS ax")
	if hashJoinBuildCount.Load() != before {
		t.Fatalf("hash join must NOT be substituted below the size floor, but it was")
	}
	// Correctness still holds: 4 A and 4 B, keys mod 2 → 2 per bucket each side
	// → 2*2*2 = 8 matches.
	if len(got) != 8 {
		t.Fatalf("expected 8 rows, got %d", len(got))
	}
}

func TestHashJoin_Guard_TrueCartesianNoKeyKeepsNestedLoop(t *testing.T) {
	// No equi-join predicate → no hash key → nested loop kept (a hash join cannot
	// help a true Cartesian product).
	g := makeRange(t, 50, 50, 10)
	on := NewEngine(g)
	before := hashJoinBuildCount.Load()
	res, err := on.Run(context.Background(), "MATCH (a:A),(b:B) WHERE a.x > b.y RETURN a.x AS ax LIMIT 1000000", nil)
	if err != nil {
		t.Fatal(err)
	}
	for res.Next() {
	}
	if err := res.Err(); err != nil {
		t.Fatal(err)
	}
	res.Close()
	if hashJoinBuildCount.Load() != before {
		t.Fatalf("hash join must NOT be substituted for a non-equi join predicate, but it was")
	}
}

// drainOrdered renders rows WITHOUT sorting, preserving emission order.
func drainOrdered(t *testing.T, e *Engine, q string) []string {
	t.Helper()
	res, err := e.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%q): %v", q, err)
	}
	cols := res.Columns()
	var out []string
	for res.Next() {
		rec := res.Record()
		s := ""
		for i, c := range cols {
			if i > 0 {
				s += "|"
			}
			s += fmt.Sprintf("%s=%v", c, rec[c])
		}
		out = append(out, s)
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%q): %v", q, err)
	}
	if err := res.Close(); err != nil {
		t.Fatalf("Close(%q): %v", q, err)
	}
	return out
}
