package sim

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// buildSearchOracle builds a GraphOracle holding exactly the given Person nodes
// and KNOWS edges, using the same templates the workload emits.
func buildSearchOracle(names []string, edges [][2]string) *GraphOracle {
	o := NewGraphOracle()
	for _, n := range names {
		o.ApplyCreate(tmplCreatePerson, map[string]any{"name": n, "age": int64(0)})
	}
	for _, e := range edges {
		o.ApplyCreate(tmplCreateKnows, map[string]any{"a": e[0], "b": e[1]})
	}
	return o
}

// buildSearchEngine builds a real cypher engine holding the same graph, so a
// CheckSearch against it exercises the genuine Cypher extraction path.
func buildSearchEngine(t *testing.T, names []string, edges [][2]string) *EngineAdapter {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true})
	eng := cypher.NewEngine(g)
	ctx := context.Background()
	for _, n := range names {
		res, err := eng.RunInTxAny(ctx, "CREATE (n:Person {name:$name})", map[string]any{"name": n})
		if err != nil {
			t.Fatalf("create node %q: %v", n, err)
		}
		drainRes(t, res)
	}
	for _, e := range edges {
		res, err := eng.RunInTxAny(ctx,
			"MATCH (a:Person {name:$a}),(b:Person {name:$b}) CREATE (a)-[:KNOWS]->(b)",
			map[string]any{"a": e[0], "b": e[1]})
		if err != nil {
			t.Fatalf("create edge %v: %v", e, err)
		}
		drainRes(t, res)
	}
	return NewEngineAdapter(eng)
}

// TestCheckSearch_CleanOnRealEngine drives the real Cypher extraction path: an
// engine and oracle holding the identical graph must yield zero violations.
func TestCheckSearch_CleanOnRealEngine(t *testing.T) {
	t.Parallel()
	names := []string{"Ada", "Alan", "Grace", "Edsger", "Donald"}
	edges := [][2]string{{"Ada", "Alan"}, {"Alan", "Grace"}, {"Grace", "Ada"}, {"Edsger", "Donald"}}
	o := buildSearchOracle(names, edges)
	eng := buildSearchEngine(t, names, edges)
	if v := CheckSearch(1, o, eng); len(v) != 0 {
		t.Fatalf("expected a clean check on a faithful engine, got: %v", v)
	}
}

// TestCheckSearch_DetectsDroppedNode asserts a node lost by the engine (a ghost
// loss) surfaces as a structural-parity violation.
func TestCheckSearch_DetectsDroppedNode(t *testing.T) {
	t.Parallel()
	names := []string{"Ada", "Alan", "Grace"}
	edges := [][2]string{{"Ada", "Alan"}, {"Alan", "Grace"}}
	o := buildSearchOracle(names, edges)
	// Engine lost "Grace" and its incident edge.
	fe := &fakeSearchEngine{names: []string{"Ada", "Alan"}, edges: [][2]string{{"Ada", "Alan"}}}
	v := CheckSearch(1, o, fe)
	assertHasViolation(t, v, ViolationGraphIntegrity, "search:nodes")
}

// TestCheckSearch_DetectsDroppedEdge asserts an edge lost by the engine surfaces
// as a structural-parity violation while the node-sets still match.
func TestCheckSearch_DetectsDroppedEdge(t *testing.T) {
	t.Parallel()
	names := []string{"Ada", "Alan", "Grace"}
	edges := [][2]string{{"Ada", "Alan"}, {"Alan", "Grace"}}
	o := buildSearchOracle(names, edges)
	fe := &fakeSearchEngine{names: names, edges: [][2]string{{"Ada", "Alan"}}} // missing Alan->Grace
	v := CheckSearch(1, o, fe)
	assertHasViolation(t, v, ViolationGraphIntegrity, "search:edges")
	for _, viol := range v {
		if viol.Op == "search:nodes" {
			t.Fatalf("node-sets match; did not expect a node violation: %v", v)
		}
	}
}

// TestCheckSearch_DetectsExtraEdge asserts a ghost edge the engine has but the
// oracle does not surfaces as a structural-parity violation.
func TestCheckSearch_DetectsExtraEdge(t *testing.T) {
	t.Parallel()
	names := []string{"Ada", "Alan", "Grace"}
	edges := [][2]string{{"Ada", "Alan"}, {"Alan", "Grace"}}
	o := buildSearchOracle(names, edges)
	fe := &fakeSearchEngine{names: names, edges: [][2]string{
		{"Ada", "Alan"}, {"Alan", "Grace"}, {"Ada", "Grace"}, // extra Ada->Grace
	}}
	v := CheckSearch(1, o, fe)
	assertHasViolation(t, v, ViolationGraphIntegrity, "search:edges")
}

// TestCheckSearch_DetectsUnknownEndpoint asserts an engine edge whose endpoint is
// not in the engine's node set is flagged.
func TestCheckSearch_DetectsUnknownEndpoint(t *testing.T) {
	t.Parallel()
	names := []string{"Ada", "Alan"}
	o := buildSearchOracle(names, nil)
	// Engine reports an edge to "Ghost", a name it did not return as a node.
	fe := &fakeSearchEngine{names: names, edges: [][2]string{{"Ada", "Ghost"}}}
	v := CheckSearch(1, o, fe)
	var found bool
	for _, viol := range v {
		if viol.Kind == ViolationGraphIntegrity && strings.Contains(viol.Message, "endpoint absent") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an unknown-endpoint violation, got: %v", v)
	}
}

// TestSearchAlgorithms_AgreeWithReferenceOnFixture cross-checks the search/
// algorithms against the naive references on a hand-built graph with two weak
// components plus an isolated node.
func TestSearchAlgorithms_AgreeWithReferenceOnFixture(t *testing.T) {
	t.Parallel()
	names := []string{"A", "B", "C", "D", "E", "F"}
	edges := [][2]string{{"A", "B"}, {"B", "C"}, {"D", "E"}} // F is isolated
	g := oracleNameGraph(buildSearchOracle(names, edges))
	if v := searchAlgorithmViolations(1, g); len(v) != 0 {
		t.Fatalf("search algorithms disagree with the reference on the fixture: %v", v)
	}
	// Spot-check the reference itself: A reaches A,B,C; F reaches only F.
	if got, want := g.naiveReachable(g.idx["A"]), sortedIdxs(g, "A", "B", "C"); !slices.Equal(got, want) {
		t.Fatalf("reachable(A) = %v, want %v", got, want)
	}
	if got, want := g.naiveReachable(g.idx["F"]), sortedIdxs(g, "F"); !slices.Equal(got, want) {
		t.Fatalf("reachable(F) = %v, want %v", got, want)
	}
	// The partition has three blocks: {A,B,C}, {D,E}, {F}.
	if sig := componentPartitionSig(g.naiveWCC()); strings.Count(sig, ";") != 2 {
		t.Fatalf("expected 3 weak components, signature was %q", sig)
	}
}

// TestNameGraph_EmptyToCSR covers the empty-graph path: an oracle with no nodes
// yields a well-formed empty CSR and a clean (no-op) algorithm check.
func TestNameGraph_EmptyToCSR(t *testing.T) {
	t.Parallel()
	g := oracleNameGraph(NewGraphOracle())
	c := g.toCSR()
	if c.Order() != 0 || c.Size() != 0 {
		t.Fatalf("empty CSR order=%d size=%d, want 0/0", c.Order(), c.Size())
	}
	if v := searchAlgorithmViolations(1, g); len(v) != 0 {
		t.Fatalf("empty graph should yield no violations, got: %v", v)
	}
}

// TestComponentPartitionSig_LabelInvariant verifies the signature depends only on
// the induced partition, not on the label values, and that a negative label
// (the search package's isolated marker) compares as a singleton.
func TestComponentPartitionSig_LabelInvariant(t *testing.T) {
	t.Parallel()
	if componentPartitionSig([]int{5, 5, 9, 9, 3}) != componentPartitionSig([]int{0, 0, 1, 1, 2}) {
		t.Fatal("equivalent partitions must have equal signatures")
	}
	if componentPartitionSig([]int{-1, -1, 7}) != componentPartitionSig([]int{0, 1, 2}) {
		t.Fatal("isolated (-1) nodes must compare as singletons")
	}
	if componentPartitionSig([]int{0, 0, 1}) == componentPartitionSig([]int{0, 1, 1}) {
		t.Fatal("different partitions must produce different signatures")
	}
}

// TestSearchScenario_CleanAcrossSeeds runs the catalogue search scenario for
// several seeds and asserts each is a clean, violation-free pass — the real
// end-to-end exercise of the battery over a seed-varied live graph.
func TestSearchScenario_CleanAcrossSeeds(t *testing.T) {
	t.Parallel()
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	sc, ok := reg.Lookup(ScenarioSearch)
	if !ok {
		t.Fatal("search scenario not registered")
	}
	ctx := context.Background()
	for _, seed := range []uint64{1, 42, 777, 6202568} {
		report, err := sc.Run(ctx, seed)
		if err != nil {
			t.Fatalf("seed %d: harness error: %v", seed, err)
		}
		if report != nil {
			t.Fatalf("seed %d: unexpected violations: %v", seed, report.Violations)
		}
	}
}

// TestSearchScenario_Deterministic runs the same seed twice and asserts both
// runs reach the identical (clean) outcome.
func TestSearchScenario_Deterministic(t *testing.T) {
	t.Parallel()
	reg, err := DefaultRegistry()
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	sc, _ := reg.Lookup(ScenarioSearch)
	ctx := context.Background()
	r1, err1 := sc.Run(ctx, 6202568)
	r2, err2 := sc.Run(ctx, 6202568)
	if err1 != nil || err2 != nil {
		t.Fatalf("harness errors: %v / %v", err1, err2)
	}
	if (r1 == nil) != (r2 == nil) {
		t.Fatalf("non-deterministic outcome: r1=%v r2=%v", r1, r2)
	}
}

// sortedIdxs returns the sorted dense ids of the named nodes in g.
func sortedIdxs(g *nameGraph, names ...string) []int {
	out := make([]int, 0, len(names))
	for _, n := range names {
		out = append(out, g.idx[n])
	}
	sort.Ints(out)
	return out
}

// assertHasViolation fails the test unless vs contains a violation of the given
// kind at the given op.
func assertHasViolation(t *testing.T, vs []Violation, kind ViolationKind, op string) {
	t.Helper()
	for _, v := range vs {
		if v.Kind == kind && v.Op == op {
			return
		}
	}
	t.Fatalf("expected a %s violation at op %q, got: %v", kind, op, vs)
}

// fakeSearchEngine is a fake Engine that serves the two search-extraction
// queries from configurable node and edge fixtures, so structural-parity
// divergences can be injected without a real torn engine. It distinguishes the
// node query from the edge query by the presence of "KNOWS" in the text.
type fakeSearchEngine struct {
	names []string
	edges [][2]string
}

func (e *fakeSearchEngine) Run(_ context.Context, query string, _ map[string]any) (Result, error) {
	if strings.Contains(query, knowsLabel) {
		return &fakeEdgeResult{edges: e.edges}, nil
	}
	return &fakeNodeResult{names: e.names}, nil
}

func (e *fakeSearchEngine) NodeCount() (int64, error) { return int64(len(e.names)), nil }
func (e *fakeSearchEngine) EdgeCount() (int64, error) { return int64(len(e.edges)), nil }

// fakeNodeResult yields one name per row in column 0.
type fakeNodeResult struct {
	names []string
	cur   int
}

func (r *fakeNodeResult) Next() bool               { r.cur++; return r.cur <= len(r.names) }
func (r *fakeNodeResult) ScalarInt() (int64, bool) { return 0, false }
func (r *fakeNodeResult) IntAt(int) (int64, bool)  { return 0, false }
func (r *fakeNodeResult) StringAt(i int) (string, bool) {
	if i == 0 {
		return r.names[r.cur-1], true
	}
	return "", false
}
func (r *fakeNodeResult) RowCount() int { return r.cur }
func (r *fakeNodeResult) Err() error    { return nil }
func (r *fakeNodeResult) Close() error  { return nil }

// fakeEdgeResult yields (src, dst) names in columns 0 and 1.
type fakeEdgeResult struct {
	edges [][2]string
	cur   int
}

func (r *fakeEdgeResult) Next() bool               { r.cur++; return r.cur <= len(r.edges) }
func (r *fakeEdgeResult) ScalarInt() (int64, bool) { return 0, false }
func (r *fakeEdgeResult) IntAt(int) (int64, bool)  { return 0, false }
func (r *fakeEdgeResult) StringAt(i int) (string, bool) {
	e := r.edges[r.cur-1]
	switch i {
	case 0:
		return e[0], true
	case 1:
		return e[1], true
	}
	return "", false
}
func (r *fakeEdgeResult) RowCount() int { return r.cur }
func (r *fakeEdgeResult) Err() error    { return nil }
func (r *fakeEdgeResult) Close() error  { return nil }
