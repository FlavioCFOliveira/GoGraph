package cypher_test

// byhandle_probe_skip_test.go — rmp #2387. buildEdgeProps used to probe the
// by-handle edge-property store on EVERY relationship row before the gate that
// decides whether the answer is wanted. On a graph built through the Go API the
// probe can only return nothing, because that path writes the PER-PAIR store
// only; in examples/26 it ran 17 009 744 times and its result was used zero
// times, at 1.15% of the run's CPU.
//
// The skip is licensed by lpg.Graph's monotonic anyHandleProp latch. These
// tests pin the property that matters: the routing decision buildEdgeProps
// makes must stay congruent across ALL THREE consumers of it — the value map,
// keys(r), and the r.k IS [NOT] NULL presence test — on BOTH sides of the
// latch. Each test also asserts the latch's state, so a test cannot pass by
// silently failing to exercise the path it claims to (the probe skip must
// actually engage in the first case, and must NOT engage in the second).
//
// Layer: short. goleak-clean (engines/graphs are local).

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/cypher/expr"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// scalarRows runs query and returns column col of every row, as a sorted slice
// of Go-rendered values, so the three consumers can be compared verbatim.
func scalarRows(t *testing.T, eng *cypher.Engine, query, col string) []string {
	t.Helper()
	res, err := eng.RunAny(context.Background(), query, nil)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer res.Close()
	var got []string
	for res.Next() {
		got = append(got, renderExpr(res.Record()[col]))
	}
	if err := res.Err(); err != nil {
		t.Fatalf("iteration %q: %v", query, err)
	}
	sort.Strings(got)
	return got
}

// renderExpr gives a stable textual form for the value kinds these queries
// return, with map keys sorted so properties(r) compares deterministically.
func renderExpr(v any) string {
	switch tv := v.(type) {
	case expr.StringValue:
		return "s:" + string(tv)
	case expr.IntegerValue:
		return "i:" + strconv.FormatInt(int64(tv), 10)
	case expr.ListValue:
		parts := make([]string, 0, len(tv))
		for _, e := range tv {
			parts = append(parts, renderExpr(e))
		}
		sort.Strings(parts)
		return "[" + strings.Join(parts, ",") + "]"
	case expr.MapValue:
		keys := make([]string, 0, len(tv))
		for k := range tv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+renderExpr(tv[k]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case expr.Value:
		if expr.IsNull(tv) {
			return "null"
		}
		return "?"
	default:
		return "?"
	}
}

// TestByHandleProbeSkip_GoAPIGraph_AllThreeConsumersAgree is the case the
// optimisation exists for, and it is examples/26's exact build shape:
// AddEdgeLabeledWithProperty stamps a relationship type and a PER-PAIR
// property, and never touches the by-handle store. The latch must therefore be
// false — so the probe is skipped — and all three consumers must still report
// the per-pair property.
func TestByHandleProbeSkip_GoAPIGraph_AllThreeConsumersAgree(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
		if err := g.SetNodeLabel(n, "P"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", n, err)
		}
		if err := g.SetNodeProperty(n, "id", lpg.StringValue(n)); err != nil {
			t.Fatalf("SetNodeProperty(%q): %v", n, err)
		}
	}
	if err := g.AddEdgeLabeledWithProperty("a", "b", 1, "FRIEND", "since", lpg.Int64Value(2020)); err != nil {
		t.Fatalf("AddEdgeLabeledWithProperty: %v", err)
	}

	// The precondition, asserted rather than assumed: this build leaves the
	// by-handle property store untouched, which is what licenses the skip.
	if g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("Go-API build latched the by-handle store: the probe skip under test would NOT engage, so this test would prove nothing")
	}

	eng := cypher.NewEngine(g)

	// Consumer 1 — the value map, reached both as a named scalar and whole.
	if got := scalarRows(t, eng, `MATCH (:P)-[r:FRIEND]->(:P) RETURN r.since AS v`, "v"); len(got) != 1 || got[0] != "i:2020" {
		t.Fatalf("r.since = %v, want [i:2020]", got)
	}
	if got := scalarRows(t, eng, `MATCH (:P)-[r:FRIEND]->(:P) RETURN properties(r) AS v`, "v"); len(got) != 1 || got[0] != "{since=i:2020}" {
		t.Fatalf("properties(r) = %v, want [{since=i:2020}]", got)
	}
	// Consumer 2 — keys(r).
	if got := scalarRows(t, eng, `MATCH (:P)-[r:FRIEND]->(:P) RETURN keys(r) AS v`, "v"); len(got) != 1 || got[0] != "[s:since]" {
		t.Fatalf("keys(r) = %v, want [[s:since]]", got)
	}
	// Consumer 3 — the presence test, in both polarities.
	if got := scalarRows(t, eng, `MATCH (:P)-[r:FRIEND]->(:P) WHERE r.since IS NOT NULL RETURN r.since AS v`, "v"); len(got) != 1 {
		t.Fatalf("IS NOT NULL matched %v rows, want 1", got)
	}
	if got := scalarRows(t, eng, `MATCH (:P)-[r:FRIEND]->(:P) WHERE r.absent IS NULL RETURN r.since AS v`, "v"); len(got) != 1 {
		t.Fatalf("IS NULL on an absent key matched %v rows, want 1", got)
	}

	// Still false after the reads: nothing in the read path may latch.
	if g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("a READ latched the by-handle store")
	}
}

// TestByHandleProbeSkip_ByHandleGraph_PerInstanceStillHolds is the other side
// of the latch, and the regression that matters: on a graph that DOES record
// by-handle properties the probe must still run, and each parallel instance
// must report its OWN property (rmp #1684) across all three consumers.
func TestByHandleProbeSkip_ByHandleGraph_PerInstanceStillHolds(t *testing.T) {
	eng, g := inMemMultigraphEngine(t)

	seed := []string{
		`CREATE (a:N {key: 'x'})`,
		`CREATE (b:N {key: 'y'})`,
		`MATCH (a:N {key:'x'}), (b:N {key:'y'}) CREATE (a)-[:USES {w: 10}]->(b)`,
		`MATCH (a:N {key:'x'}), (b:N {key:'y'}) CREATE (a)-[:CALLS {w: 20}]->(b)`,
	}
	for _, q := range seed {
		mustRunWrite(t, eng, q)
	}

	// Cypher CREATE records by-handle properties, so the latch MUST be set and
	// the probe MUST run — otherwise the per-instance answers below could not
	// be produced at all.
	if !g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("Cypher CREATE with a relationship property did not latch: the by-handle probe would be skipped and per-instance properties would be lost")
	}

	// Consumer 1 — each instance's own value, not a merged one.
	if got := scalarRows(t, eng, `MATCH (:N)-[r]->(:N) RETURN r.w AS v`, "v"); len(got) != 2 || got[0] != "i:10" || got[1] != "i:20" {
		t.Fatalf("r.w per instance = %v, want [i:10 i:20]", got)
	}
	if got := scalarRows(t, eng, `MATCH (:N)-[r]->(:N) RETURN properties(r) AS v`, "v"); len(got) != 2 || got[0] != "{w=i:10}" || got[1] != "{w=i:20}" {
		t.Fatalf("properties(r) per instance = %v, want [{w=i:10} {w=i:20}]", got)
	}
	// Consumer 2 — keys(r) on each instance.
	if got := scalarRows(t, eng, `MATCH (:N)-[r]->(:N) RETURN keys(r) AS v`, "v"); len(got) != 2 || got[0] != "[s:w]" || got[1] != "[s:w]" {
		t.Fatalf("keys(r) per instance = %v, want two [s:w]", got)
	}
	// Consumer 3 — the presence test on each instance.
	if got := scalarRows(t, eng, `MATCH (:N)-[r]->(:N) WHERE r.w IS NOT NULL RETURN r.w AS v`, "v"); len(got) != 2 {
		t.Fatalf("IS NOT NULL matched %v, want both instances", got)
	}
	if got := scalarRows(t, eng, `MATCH (:N)-[r]->(:N) WHERE r.nope IS NOT NULL RETURN r.w AS v`, "v"); len(got) != 0 {
		t.Fatalf("IS NOT NULL on an absent key matched %v, want none", got)
	}
}

// TestByHandleProbeSkip_LatchedGraphKeepsPerPairEdgesCorrect is the mixed case
// the latch's global scope creates: once ANY by-handle property exists the
// probe runs for every relationship, including ones that have no by-handle
// entry. Those must keep falling back to the per-pair store — the fallback
// ladder buildEdgeProps documents — so a latched graph and an unlatched one
// answer identically for a Go-API-built edge.
func TestByHandleProbeSkip_LatchedGraphKeepsPerPairEdgesCorrect(t *testing.T) {
	eng, g := inMemMultigraphEngine(t)

	// A Cypher-created relationship latches the graph.
	for _, q := range []string{
		`CREATE (a:N {key: 'x'})`,
		`CREATE (b:N {key: 'y'})`,
		`MATCH (a:N {key:'x'}), (b:N {key:'y'}) CREATE (a)-[:USES {w: 10}]->(b)`,
	} {
		mustRunWrite(t, eng, q)
	}
	if !g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("precondition: the graph should be latched by the Cypher CREATE above")
	}

	// Now add a Go-API edge between a fresh pair: handle stamped, per-pair
	// property only, no by-handle entry — but the graph is latched, so the
	// probe WILL run and must find nothing and fall back.
	for _, n := range []string{"gp", "gq"} {
		if err := g.AddNode(n); err != nil {
			t.Fatalf("AddNode(%q): %v", n, err)
		}
		if err := g.SetNodeLabel(n, "G"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", n, err)
		}
	}
	if err := g.AddEdgeLabeledWithProperty("gp", "gq", 1, "FRIEND", "since", lpg.Int64Value(1999)); err != nil {
		t.Fatalf("AddEdgeLabeledWithProperty: %v", err)
	}

	if got := scalarRows(t, eng, `MATCH (:G)-[r:FRIEND]->(:G) RETURN r.since AS v`, "v"); len(got) != 1 || got[0] != "i:1999" {
		t.Fatalf("per-pair r.since on a LATCHED graph = %v, want [i:1999]", got)
	}
	if got := scalarRows(t, eng, `MATCH (:G)-[r:FRIEND]->(:G) RETURN keys(r) AS v`, "v"); len(got) != 1 || got[0] != "[s:since]" {
		t.Fatalf("per-pair keys(r) on a LATCHED graph = %v, want [[s:since]]", got)
	}
	if got := scalarRows(t, eng, `MATCH (:G)-[r:FRIEND]->(:G) WHERE r.since IS NOT NULL RETURN r.since AS v`, "v"); len(got) != 1 {
		t.Fatalf("per-pair presence on a LATCHED graph matched %v, want 1", got)
	}
}
