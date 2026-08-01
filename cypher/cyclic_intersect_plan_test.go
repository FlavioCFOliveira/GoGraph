package cypher

// cyclic_intersect_plan_test.go — engine-level gates for the fused cyclic expand
// recogniser (rmp #2157).
//
// Layer: short.
//
// WHY A DIFFERENTIAL ALONE IS NOT ENOUGH HERE, and why every test below pairs it
// with the engagement counter: SPIKE #2155 verified that the openCypher TCK
// contains NO directed cycle over three or more distinct node variables, so
// TCK 3897/3897 stays green whether this operator is correct, wrong, or never runs.
// A flag-on/flag-off differential is blind in exactly the same way — if the
// recogniser silently declines, both arms run today's plan and agree perfectly. So
// each case asserts BOTH that the results match AND that the operator engaged (or
// deliberately did not).

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	cmetrics "github.com/FlavioCFOliveira/GoGraph/internal/metrics"
)

// engageProbe counts fused-cyclic-expand engagements.
type engageProbe struct{ n atomic.Uint64 }

func (p *engageProbe) IncCounter(name string, delta uint64) {
	if name == exec.MetricExpandIntersectEngaged {
		p.n.Add(delta)
	}
}
func (p *engageProbe) ObserveLatency(string, time.Duration) {}

func (p *engageProbe) SetGauge(string, float64) {}

// withEngageProbe installs a probe for the duration of fn and returns the count.
func withEngageProbe(t *testing.T, fn func()) uint64 {
	t.Helper()
	p := &engageProbe{}
	cmetrics.SetBackend(p)
	defer cmetrics.SetBackend(nil)
	fn()
	return p.n.Load()
}

// cyclicGraph builds a labelled multigraph with the given edges, all of type K.
func cyclicGraph(t *testing.T, nodes int, edges [][2]int) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	keys := make([]string, nodes)
	for i := 0; i < nodes; i++ {
		keys[i] = "n" + itoaCyc(i)
		if err := g.AddNode(keys[i]); err != nil {
			t.Fatalf("AddNode: %v", err)
		}
		if err := g.SetNodeLabel(keys[i], "P"); err != nil {
			t.Fatalf("SetNodeLabel: %v", err)
		}
	}
	for _, e := range edges {
		if err := g.AddEdge(keys[e[0]], keys[e[1]], 1.0); err != nil {
			t.Fatalf("AddEdge: %v", err)
		}
		g.SetEdgeLabel(keys[e[0]], keys[e[1]], "K")
	}
	return g
}

func itoaCyc(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// runRows drains a query and returns each row rendered positionally, so two runs
// can be compared as an ordered SEQUENCE rather than a multiset.
func runRows(t *testing.T, eng *Engine, q string) []string {
	t.Helper()
	res, err := eng.Run(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("Run(%s): %v", q, err)
	}
	defer func() { _ = res.Close() }()
	var out []string
	for res.Next() {
		var sb strings.Builder
		for i := range res.Columns() {
			if i > 0 {
				sb.WriteByte('|')
			}
			v := res.ValueAt(i)
			if v == nil {
				sb.WriteString("<nil>")
				continue
			}
			sb.WriteString(v.String())
		}
		out = append(out, sb.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%s): %v", q, err)
	}
	return out
}

// The shapes that must FUSE. Both are direct-stack: the relationship-type filter
// lives inside the operator, so no Selection is interposed between the hops.
var fusingQueries = []string{
	`MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`,
	`MATCH (a)-[r1:K]->(b)-[r2:K]->(c)-[r3:K]->(a) RETURN a.x AS ax, b.x AS bx, c.x AS cx`,
	`MATCH (a)-->(b)-->(c)-->(a) RETURN count(*) AS n`,
	// A 2-CYCLE fuses too, and this was NOT anticipated when the recogniser was
	// written — the test originally asserted it must decline and the code proved the
	// expectation wrong. It is correct: for `(a)-[r1]->(b)-[r2]->(a)` the middle
	// hop's source and IntoVar are both `a`, so the operator intersects
	// N_out(a) ∩ N_in(a) to find every b closing the 2-cycle, emits the identical six
	// columns (a, r1, b, b, r2, a), and its internal r3 != r2 check is exactly the
	// cyphermorphism rule that r1 and r2 must be distinct edges. This is the audit's
	// own Sec 2.3 shape, so it is worth having.
	`MATCH (a)-[:K]->(b)-[:K]->(a) RETURN count(*) AS n`,
	`MATCH (a)-[r1:K]->(b)-[r2:K]->(a) RETURN a.x AS ax, b.x AS bx`,
}

// The shapes that must NOT fuse, each for a stated reason.
var nonFusingQueries = []struct {
	q      string
	reason string
}{
	{`MATCH (a:P)-[:K]->(b:P)-[:K]->(c:P)-[:K]->(a) RETURN count(*) AS n`,
		"label predicates interpose a Selection between the hops, so Child is not an *ir.Expand"},
	{`MATCH (a)-[:K]->(b)-[:K]->(c) RETURN count(*) AS n`,
		"acyclic: no hop has IntoVar set, so the predicate cannot fire"},
	{`MATCH (a)-[:K]->(b)<-[:K]-(c)-[:K]->(a) RETURN count(*) AS n`,
		"a reversed leg is not DirectionOutgoing"},
	{`MATCH (a)-[:K*1..2]->(b)-[:K]->(a) RETURN count(*) AS n`,
		"a variable-length leg is not a fixed-arity hop"},
}

// TestCyclicIntersect_FusesAndAgrees is the core gate: for every fusing shape the
// results must be an IDENTICAL SEQUENCE with the flag on and off, AND the operator
// must have engaged with it on and not with it off.
func TestCyclicIntersect_FusesAndAgrees(t *testing.T) {
	g := cyclicGraph(t, 6, [][2]int{
		{0, 1}, {1, 2}, {2, 0}, // a triangle
		{1, 3}, {3, 0}, // a second triangle sharing 0→1
		{2, 2}, // a self-loop
		{0, 1}, // a parallel edge on 0→1
		{1, 0}, // makes 0↔1 a genuine 2-cycle
		{4, 5}, // a dangling pair, no cycle
	})
	for i := 0; i < 6; i++ {
		if err := g.SetNodeProperty("n"+itoaCyc(i), "x", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}

	for _, q := range fusingQueries {
		t.Run(q, func(t *testing.T) {
			var off, on []string
			offEngaged := withEngageProbe(t, func() {
				off = runRows(t, NewEngine(g), q)
			})
			onEngaged := withEngageProbe(t, func() {
				on = runRows(t, NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true}), q)
			})

			if offEngaged != 0 {
				t.Fatalf("operator engaged %d times with the flag OFF; want 0", offEngaged)
			}
			if onEngaged == 0 {
				t.Fatalf("operator did NOT engage with the flag ON — the differential below " +
					"would be vacuously green because both arms ran today's plan")
			}
			if len(on) != len(off) {
				t.Fatalf("row count: on=%d off=%d\n  on:  %v\n  off: %v", len(on), len(off), on, off)
			}
			for i := range off {
				if on[i] != off[i] {
					t.Fatalf("row %d differs:\n  on  %q\n  off %q", i, on[i], off[i])
				}
			}
			if len(off) == 0 {
				t.Fatal("the query returned no rows, so the differential proves nothing")
			}
		})
	}
}

// TestCyclicIntersect_DeclinedShapesStayIdentical checks each veto: the operator
// must not engage, and the results must be unchanged by the flag.
func TestCyclicIntersect_DeclinedShapesStayIdentical(t *testing.T) {
	g := cyclicGraph(t, 6, [][2]int{
		{0, 1}, {1, 2}, {2, 0}, {1, 3}, {3, 0}, {0, 1},
	})
	for i := 0; i < 6; i++ {
		if err := g.SetNodeProperty("n"+itoaCyc(i), "x", lpg.Int64Value(int64(i))); err != nil {
			t.Fatalf("SetNodeProperty: %v", err)
		}
	}
	for _, tc := range nonFusingQueries {
		t.Run(tc.reason, func(t *testing.T) {
			var off, on []string
			_ = withEngageProbe(t, func() { off = runRows(t, NewEngine(g), tc.q) })
			engaged := withEngageProbe(t, func() {
				on = runRows(t, NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true}), tc.q)
			})
			if engaged != 0 {
				t.Fatalf("operator engaged %d times on a shape it must decline (%s)", engaged, tc.reason)
			}
			if len(on) != len(off) {
				t.Fatalf("row count changed with the flag on: on=%d off=%d", len(on), len(off))
			}
			for i := range off {
				if on[i] != off[i] {
					t.Fatalf("row %d changed with the flag on:\n  on  %q\n  off %q", i, on[i], off[i])
				}
			}
		})
	}
}

// TestCyclicIntersect_ExplainShowsTheOperator proves the plan shape changed, which
// is the plan-time half of the white-box evidence.
func TestCyclicIntersect_ExplainShowsTheOperator(t *testing.T) {
	g := cyclicGraph(t, 4, [][2]int{{0, 1}, {1, 2}, {2, 0}})
	const q = `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`

	offPlan, err := NewEngine(g).Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain (off): %v", err)
	}
	if strings.Contains(offPlan, "ExpandIntersect") {
		t.Fatalf("the flag is off but the plan already names ExpandIntersect:\n%s", offPlan)
	}
	onPlan, err := NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true}).Explain(q, nil)
	if err != nil {
		t.Fatalf("Explain (on): %v", err)
	}
	if !strings.Contains(onPlan, "ExpandIntersect") {
		t.Fatalf("the flag is on but the plan does not name ExpandIntersect:\n%s", onPlan)
	}
	// The fusion must REMOVE one Expand, not add an operator beside them.
	if got, want := strings.Count(onPlan, "Expand"), strings.Count(offPlan, "Expand"); got >= want {
		t.Fatalf("fused plan has %d Expand mentions, unfused has %d — the fusion should "+
			"replace two operators with one:\n--- on ---\n%s\n--- off ---\n%s",
			got, want, onPlan, offPlan)
	}
}

// TestCyclicIntersect_ParallelEdgeMultiplicityEndToEnd pins openCypher multiplicity
// through the whole engine, against a hand-computed oracle rather than only against
// the flag-off arm.
func TestCyclicIntersect_ParallelEdgeMultiplicityEndToEnd(t *testing.T) {
	// One triangle with 3 parallel edges on 1→2 and 2 on 2→0: each of the three
	// rotations is enumerated once per (edge choice) combination, so count(*) is
	// 3 rotations × 3 × 2 × 1 = 18.
	g := cyclicGraph(t, 4, [][2]int{
		{0, 1},
		{1, 2}, {1, 2}, {1, 2},
		{2, 0}, {2, 0},
	})
	const q = `MATCH (a)-[:K]->(b)-[:K]->(c)-[:K]->(a) RETURN count(*) AS n`
	const wantCount = 18

	for _, arm := range []struct {
		name string
		eng  *Engine
	}{
		{"flag off", NewEngine(g)},
		{"flag on", NewEngineWithOptions(g, EngineOptions{EnableCyclicIntersect: true})},
	} {
		t.Run(arm.name, func(t *testing.T) {
			res, err := arm.eng.Run(context.Background(), q, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			defer func() { _ = res.Close() }()
			var got int64
			for res.Next() {
				if iv, ok := res.ValueAt(0).(expr.IntegerValue); ok {
					got = int64(iv)
				}
			}
			if err := res.Err(); err != nil {
				t.Fatalf("Err: %v", err)
			}
			if got != wantCount {
				t.Fatalf("count(*) = %d; hand-computed oracle says %d", got, wantCount)
			}
		})
	}
}
