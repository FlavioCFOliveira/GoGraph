package cypher

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/cypher/exec"
	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// live_adjacency_test.go — rmp #2883: forward traversal in a write statement
// serves per-source runs from the transaction's own view instead of building a
// whole-graph CSR pair, without changing any answer.
//
// None of these tests calls t.Parallel: they bracket process-global counters,
// and a top-level sequential test never overlaps a parallel one.

// liveCounters is a bracket over the process-global structural counters.
type liveCounters struct {
	pairs, types, served, fallbacks uint64
}

func readLiveCounters() liveCounters {
	return liveCounters{
		pairs:     csrPairUncachedBuildCount.Load(),
		types:     slotTypeResolveCount.Load(),
		served:    exec.LiveOutRunServedCount(),
		fallbacks: exec.LiveFallbackCount(),
	}
}

func (c liveCounters) since(prev liveCounters) liveCounters {
	return liveCounters{
		pairs:     c.pairs - prev.pairs,
		types:     c.types - prev.types,
		served:    c.served - prev.served,
		fallbacks: c.fallbacks - prev.fallbacks,
	}
}

// newLiveTestEngine returns an engine over a directed multigraph, with live
// traversal disabled when control is true.
func newLiveTestEngine(t *testing.T, control bool) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	eng.disableLiveTraversalForTest = control
	return eng
}

// liveWrite runs a statement as an autocommit write and returns its rows.
func liveWrite(t *testing.T, eng *Engine, q string) []string {
	t.Helper()
	res, err := eng.RunInTx(context.Background(), q, nil)
	if err != nil {
		t.Fatalf("RunInTx(%s): %v", q, err)
	}
	return liveDrain(t, res, q)
}

// liveExec runs a statement inside an explicit transaction and returns its rows.
func liveExec(t *testing.T, tx *ExplicitTx, q string) []string {
	t.Helper()
	res, err := tx.Exec(q, nil)
	if err != nil {
		t.Fatalf("Exec(%s): %v", q, err)
	}
	return liveDrain(t, res, q)
}

func liveDrain(t *testing.T, res *Result, q string) []string {
	t.Helper()
	defer func() { _ = res.Close() }()
	var out []string
	for res.Next() {
		var sb strings.Builder
		for i := range res.Columns() {
			if i > 0 {
				sb.WriteByte('|')
			}
			if v := res.ValueAt(i); v != nil {
				sb.WriteString(v.String())
			} else {
				sb.WriteString("<nil>")
			}
		}
		out = append(out, sb.String())
	}
	if err := res.Err(); err != nil {
		t.Fatalf("Err(%s): %v", q, err)
	}
	return out
}

// seedHub creates one :Hub {id: 0} with a :LINK to each of n :Spoke nodes, plus
// an unrelated chain of :Other nodes so the graph is materially larger than the
// hub's neighbourhood.
func seedHub(t *testing.T, eng *Engine, n int) {
	t.Helper()
	liveWrite(t, eng, "CREATE INDEX FOR (n:Hub) ON (n.id)")
	liveWrite(t, eng, "CREATE INDEX FOR (n:Spoke) ON (n.id)")
	liveWrite(t, eng, fmt.Sprintf(
		"CREATE (h:Hub {id: 0}) WITH h UNWIND range(0, %d) AS i CREATE (h)-[:LINK]->(:Spoke {id: i})", n-1))
	liveWrite(t, eng, "UNWIND range(0, 199) AS i CREATE (:Other {id: i})-[:NEXT]->(:Other {id: i + 1000})")
}

// TestLiveExpand_WriteForwardBuildsNoPair is the structural gate: an anchored,
// typed, forward write statement builds neither a CSR pair nor a type column,
// and its runs demonstrably came from the live path.
func TestLiveExpand_WriteForwardBuildsNoPair(t *testing.T) {
	eng := newLiveTestEngine(t, false)
	seedHub(t, eng, 50)

	before := readLiveCounters()
	reuses := liveCaptureReuses.Load()
	liveWrite(t, eng, "MATCH (:Hub {id: 0})-[r:LINK]->(:Spoke {id: 7}) DELETE r")
	// The DELETE writes the hub the traversal just served, so its journal
	// capture must reuse that resolution rather than resolve the hub again.
	if liveCaptureReuses.Load() == reuses {
		t.Fatalf("the DELETE's capture of the node it had just served resolved it again")
	}
	rows := liveWrite(t, eng,
		"MATCH (h:Hub)-[:LINK]->(s:Spoke) WITH h, s LIMIT 1 CREATE (h)-[:LINK]->(s) RETURN s.id")
	d := readLiveCounters().since(before)
	if d.pairs != 0 || d.types != 0 {
		t.Fatalf("forward write statements built %d CSR pair(s) and %d type resolution(s); want 0 and 0", d.pairs, d.types)
	}
	if d.served == 0 {
		t.Fatalf("no run was served live: the gate above would pass vacuously")
	}
	if len(rows) != 1 {
		t.Fatalf("LIMIT 1 CREATE returned %d rows, want 1", len(rows))
	}
	got := liveWrite(t, eng, "MATCH (:Hub {id: 0})-[r:LINK]->(:Spoke {id: 7}) RETURN count(r)")
	if len(got) != 1 || got[0] != "0" {
		t.Fatalf("deleted relationship still matched: %v", got)
	}
}

// TestLiveExpand_ReverseStillBuildsPair pins the other half of the split: a
// traversal that reads incoming edges keeps the whole-graph build, because no
// versioned incoming-edge structure exists to answer it per source.
func TestLiveExpand_ReverseStillBuildsPair(t *testing.T) {
	eng := newLiveTestEngine(t, false)
	seedHub(t, eng, 20)
	before := readLiveCounters()
	rows := liveWrite(t, eng,
		"MATCH (s:Spoke {id: 3})<-[r:LINK]-(h:Hub) SET s.seen = true RETURN h.id")
	d := readLiveCounters().since(before)
	if d.pairs == 0 {
		t.Fatalf("an incoming write traversal built no CSR pair: it must keep the whole-graph build")
	}
	if !slices.Equal(rows, []string{"0"}) {
		t.Fatalf("rows = %v, want [0]", rows)
	}
}

// liveCountStmt counts a node's forward neighbours from inside a WRITE
// statement, so the traversal is planned on the write path. %d is the Spoke id
// whose neighbours are counted; -1 selects the hub.
func liveCountStmt(spoke int) string {
	anchor := "(:Hub {id: 0})"
	if spoke >= 0 {
		anchor = fmt.Sprintf("(:Spoke {id: %d})", spoke)
	}
	return "MATCH " + anchor + "-[:LINK]->(s) WITH count(s) AS c CREATE (:Probe {c: c}) RETURN c"
}

// TestLiveExpand_Isolation: a write transaction's live traversal does not see
// another transaction's uncommitted writes, still sees an edge another
// transaction deleted and committed after it began, and sees its own writes.
//
// The own-write step writes a DIFFERENT node's adjacency from the one the other
// transactions wrote: writing the hub after a later commit changed it is a
// write-write conflict, which MVCC correctly refuses.
func TestLiveExpand_Isolation(t *testing.T) {
	eng := newLiveTestEngine(t, false)
	seedHub(t, eng, 10)
	ctx := context.Background()

	a, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Rollback() }()

	before := readLiveCounters()
	if got := liveExec(t, a, liveCountStmt(-1)); !slices.Equal(got, []string{"10"}) {
		t.Fatalf("baseline count = %v, want [10]", got)
	}

	// Another transaction's UNCOMMITTED write: invisible to A.
	b, err := eng.BeginTx(ctx)
	if err != nil {
		t.Fatal(err)
	}
	liveExec(t, b, "MATCH (h:Hub {id: 0}) CREATE (h)-[:LINK]->(:Spoke {id: 200})")
	if got := liveExec(t, a, liveCountStmt(-1)); !slices.Equal(got, []string{"10"}) {
		t.Fatalf("another transaction's uncommitted write leaked: count = %v, want [10]", got)
	}
	if err := b.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Another transaction DELETES an edge A can see, and COMMITS: A still sees it.
	liveWrite(t, eng, "MATCH (:Hub {id: 0})-[r:LINK]->(:Spoke {id: 3}) DELETE r")
	if got := liveExec(t, a, liveCountStmt(-1)); !slices.Equal(got, []string{"10"}) {
		t.Fatalf("an edge deleted by a later commit vanished from A's snapshot: count = %v, want [10]", got)
	}

	// Own uncommitted write: visible to A's next statement.
	if got := liveExec(t, a, liveCountStmt(5)); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("spoke 5 baseline = %v, want [0]", got)
	}
	liveExec(t, a, "MATCH (s:Spoke {id: 5}) CREATE (s)-[:LINK]->(:Spoke {id: 100})")
	if got := liveExec(t, a, liveCountStmt(5)); !slices.Equal(got, []string{"1"}) {
		t.Fatalf("own uncommitted write not visible: count = %v, want [1]", got)
	}
	d := readLiveCounters().since(before)
	if d.served == 0 {
		t.Fatalf("no run was served live: the isolation assertions above would not test the live path")
	}
	if d.pairs != 0 {
		t.Fatalf("the forward counts built %d CSR pair(s); want 0", d.pairs)
	}

	// The own write stays invisible to everyone else until A commits.
	if got := liveWrite(t, eng, liveCountStmt(5)); !slices.Equal(got, []string{"0"}) {
		t.Fatalf("A's uncommitted write leaked to another statement: count = %v, want [0]", got)
	}
	if got := liveWrite(t, eng, liveCountStmt(-1)); !slices.Equal(got, []string{"9"}) {
		t.Fatalf("committed state count = %v, want [9]", got)
	}
}

// liveGraphDump renders the whole committed graph, relationships and all, in a
// canonical order.
func liveGraphDump(t *testing.T, eng *Engine) []string {
	t.Helper()
	nodes := liveWrite(t, eng, "MATCH (n) RETURN labels(n), n.k, n.hit ORDER BY n.k, n.hit")
	rels := liveWrite(t, eng,
		"MATCH (a)-[r]->(b) RETURN a.k, type(r), b.k, r.w ORDER BY a.k, type(r), b.k, r.w")
	return append(append([]string{"nodes:"}, nodes...), append([]string{"rels:"}, rels...)...)
}

// liveFixtureEdge is one relationship of [newLiveFixture].
type liveFixtureEdge struct {
	src, dst int
	typ      string
	w        int64
}

// newLiveFixture builds the differential fixture through the Go API with FIXED
// node keys, so two engines built by it assign the same NodeIDs and the same
// handles. Cypher CREATE cannot: its node keys come from a process-global
// counter, so two engines built from the same statements disagree on every id and
// therefore, legitimately, on the (destination, handle) order of every run.
//
// The fixture is a chain s0 -> s1 -> ... -> s5 typed :T, with parallel :U edges
// and an extra :T on one pair, and a self-loop, so ordering by (destination,
// handle) and multi-slot pairs are both observable.
func newLiveFixture(t *testing.T, control bool) *Engine {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for k := 0; k <= 5; k++ {
		key := fmt.Sprintf("s%d", k)
		if err := g.AddNode(key); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeLabel(key, "S"); err != nil {
			t.Fatal(err)
		}
		if err := g.SetNodeProperty(key, "k", lpg.Int64Value(int64(k))); err != nil {
			t.Fatal(err)
		}
	}
	edges := []liveFixtureEdge{
		{0, 1, "T", 0}, {1, 2, "T", 1}, {2, 3, "T", 2}, {3, 4, "T", 3}, {4, 5, "T", 4},
		{1, 3, "U", 1}, {1, 3, "U", 2}, {1, 3, "T", 9}, {4, 4, "T", 44},
		// Inserted in descending destination order, so s0's stored run is not in
		// (destination, handle) order and the served run must be sorted to agree.
		{0, 5, "T", 50}, {0, 4, "U", 40}, {0, 3, "T", 30}, {0, 2, "U", 20},
	}
	for _, e := range edges {
		src, dst := fmt.Sprintf("s%d", e.src), fmt.Sprintf("s%d", e.dst)
		h, err := g.AddEdgeH(src, dst, 0)
		if err != nil {
			t.Fatal(err)
		}
		g.SetEdgeLabelByHandle(src, dst, h, e.typ)
		if err := g.SetEdgePropertyByHandle(src, dst, h, "w", lpg.Int64Value(e.w)); err != nil {
			t.Fatal(err)
		}
	}
	eng := NewEngine(g)
	eng.disableLiveTraversalForTest = control
	return eng
}

// liveArms runs stmt on two freshly built, identical fixtures — one with live
// traversal, one on the whole-graph build — and returns the rows (in emission
// order), the committed graph afterwards, and how many runs the live arm served.
//
// Each arm gets its own engine rather than a rolled-back transaction on a shared
// one: rolling back some of these statements does not restore the graph at
// 4f733f9e (a pre-existing defect reported with rmp #2883), which would make the
// second arm run against a different graph from the first.
func liveArms(t *testing.T, stmt string) (rows, dumps [2][]string, served uint64) {
	t.Helper()
	var ids [2][]string
	for arm, control := range []bool{false, true} {
		eng := newLiveFixture(t, control)
		ids[arm] = liveWrite(t, eng, "MATCH (n) RETURN n.k, id(n) ORDER BY n.k")
		before := readLiveCounters()
		rows[arm] = liveWrite(t, eng, stmt)
		if !control {
			served = readLiveCounters().since(before).served
		}
		dumps[arm] = liveGraphDump(t, eng)
	}
	if !slices.Equal(ids[0], ids[1]) {
		t.Fatalf("fixture ids differ between arms (%v vs %v): the order comparison would be meaningless", ids[0], ids[1])
	}
	return rows, dumps, served
}

// TestLiveExpand_DifferentialAgainstWholeGraph runs statements whose traversal
// interleaves with the statement's own writes — the shapes a naive per-source
// read would answer differently — with live traversal and on the whole-graph
// build, and requires the same rows IN THE SAME ORDER and the same final graph.
func TestLiveExpand_DifferentialAgainstWholeGraph(t *testing.T) {
	type stmtCase struct {
		q string
		// unordered compares rows as a multiset. It is set ONLY where the
		// traversal reaches a node the statement itself created: such a node's
		// key comes from a process-global counter, so its id — and hence its
		// place in the (destination, handle) order — differs between two engines
		// even when the engines are otherwise identical.
		unordered bool
	}
	stmts := []stmtCase{
		// A CREATE above the traversal writes a source's out-edges before that
		// source is expanded: the traversal must not see them.
		{q: "MATCH (a:S)-[:T]->(b) CREATE (b)-[:T {w: 100}]->(:S {k: 100 + b.k}) RETURN a.k, b.k"},
		// A DELETE above the traversal removes edges of later sources.
		{q: "MATCH (a:S)-[r:T]->(b) DELETE r RETURN a.k, b.k"},
		// DETACH DELETE removes later sources and destinations.
		{q: "MATCH (a:S)-[:T]->(b) DETACH DELETE b RETURN a.k"},
		// Untyped, with multi-slot pairs and the self-loop.
		{q: "MATCH (a:S)-->(b) CREATE (a)-[:V]->(b) RETURN a.k, b.k"},
		{q: "MATCH (a:S)-[r:T|U]->(b) SET r.w = coalesce(r.w, 0) + 1 RETURN a.k, type(r), b.k, r.w"},
		// OPTIONAL MATCH re-Inits per row, and its CREATE lands between rows.
		{q: "MATCH (a:S) OPTIONAL MATCH (a)-[:T]->(b) CREATE (a)-[:T]->(:S {k: 200 + a.k}) RETURN a.k, b.k"},
		// Variable-length, forward, typed and untyped.
		{q: "MATCH p = (a:S {k: 0})-[:T*1..4]->(b) CREATE (b)-[:T]->(:S {k: 300 + b.k}) RETURN length(p), b.k"},
		{q: "MATCH (a:S {k: 1})-[*1..2]->(b) SET b.hit = true RETURN b.k"},
		// A traversal whose CHILD writes before it expands each source.
		{q: "MATCH (a:S) CREATE (a)-[:T {w: 7}]->(:S {k: 400 + a.k}) WITH a MATCH (a)-[r:T]->(b) RETURN a.k, b.k, r.w", unordered: true},
		// Expand-into over a cycle-closing forward hop.
		{q: "MATCH (a:S)-[:T]->(b)-[:T]->(c), (a)-[:U]->(c) CREATE (a)-[:W]->(c) RETURN a.k, c.k"},
		// A relationship deleted by one clause, then traversed by a later one.
		{q: "MATCH (a:S {k: 2})-[r:T]->() DELETE r WITH count(*) AS n MATCH (x:S)-[:T]->(y) RETURN n, x.k, y.k"},
	}
	for i, sc := range stmts {
		stmt := sc.q
		t.Run(fmt.Sprintf("stmt%02d", i), func(t *testing.T) {
			rows, dumps, served := liveArms(t, stmt)
			if sc.unordered {
				slices.Sort(rows[0])
				slices.Sort(rows[1])
			}
			if !slices.Equal(rows[0], rows[1]) {
				t.Errorf("rows differ\n live:    %v\n control: %v", rows[0], rows[1])
			}
			if !slices.Equal(dumps[0], dumps[1]) {
				t.Errorf("final graphs differ\n live:    %v\n control: %v", dumps[0], dumps[1])
			}
			if len(rows[0]) == 0 {
				t.Errorf("no rows: the comparison is vacuous")
			}
			if served == 0 {
				t.Errorf("no run was served live for %q: the comparison did not exercise the live path", stmt)
			}
		})
	}
}

// TestLiveExpand_OrderMatchesWholeGraph: an ORDER-free forward expansion emits
// its rows in the same sequence as the whole-graph build, parallel edges and all.
func TestLiveExpand_OrderMatchesWholeGraph(t *testing.T) {
	// Precondition: s0's STORED run is out of (destination, handle) order, so the
	// comparison below would fail if the live path served it unsorted.
	g := newLiveFixture(t, false).g
	id, _ := g.AdjList().Mapper().Lookup("s0")
	stored, _, _ := g.AdjList().LoadEntryH(id)
	if slices.IsSorted(stored) {
		t.Fatalf("fixture precondition: s0's stored run %v is already ordered", stored)
	}
	rows, _, served := liveArms(t, "MATCH (a:S)-[r]->(b) SET r.seen = true RETURN a.k, type(r), b.k, r.w")
	if !slices.Equal(rows[0], rows[1]) {
		t.Fatalf("emission order differs\n live:    %v\n control: %v", rows[0], rows[1])
	}
	if served == 0 || len(rows[0]) != 13 {
		t.Fatalf("served %d runs and %d rows; want >0 and 13", served, len(rows[0]))
	}
}

// TestLiveSource_AssembleMatchesWholeGraphBuild: the whole-graph adjacency the
// fallback assembles from per-node runs is the whole-graph build, arc for arc,
// handle for handle, type code for type code.
func TestLiveSource_AssembleMatchesWholeGraphBuild(t *testing.T) {
	eng := newLiveTestEngine(t, false)
	liveWrite(t, eng, "UNWIND range(0, 30) AS i CREATE (:S {k: i})")
	liveWrite(t, eng, "MATCH (a:S), (b:S) WHERE b.k = (a.k * 7 + 3) % 31 CREATE (a)-[:T]->(b), (a)-[:U]->(b)")
	liveWrite(t, eng, "MATCH (a:S {k: 4}) CREATE (a)-[:T]->(a)")
	liveWrite(t, eng, "MATCH (a:S {k: 9}) DETACH DELETE a")
	g := eng.g
	snap := g.BeginRead()
	defer g.EndRead(snap)
	view := g.ReadAt(snap)
	log := &liveTopoLog{}
	if !log.bind(view) {
		t.Fatal("bind refused the first view")
	}
	src := &liveOutSource{log: log, g: view, relTypes: []string{"T"}}
	at, _ := src.LiveBegin()
	log.seq.Add(1) // force the assembling branch rather than the present build
	got, codes, _ := src.assembleAt(at)
	fwd, _, _ := csrPairFromGraphAt(view)
	col := buildRelTypeColumn(view, fwd, nil)
	if !slices.Equal(got.EdgesSlice(), fwd.EdgesSlice()) || !slices.Equal(got.HandlesSlice(), fwd.HandlesSlice()) {
		t.Fatalf("assembled arcs/handles differ from the build")
	}
	n := len(fwd.VerticesSlice())
	if !slices.Equal(got.VerticesSlice()[:n], fwd.VerticesSlice()) {
		t.Fatalf("assembled offsets differ from the build")
	}
	accept := relTypeCodesFor(view, []string{"T"})
	a1 := exec.NewRelTypeColumn(codes, nil, nil, nil).Admit(accept)
	a2 := col.Admit(accept)
	for pos := range uint64(len(codes)) {
		if a1.Fwd(pos) != a2.Fwd(pos) {
			t.Fatalf("type admission differs at arc %d", pos)
		}
	}
	if len(codes) == 0 {
		t.Fatal("no arcs: the comparison is vacuous")
	}
}

// liveJournaledMethods are the exec.GraphMutator methods that change a node's
// outgoing adjacency, its relationship types, a pair's CREATE count or a node's
// liveness — everything a live run or its liveness filter is derived from — and
// must therefore report to the statement's liveTopoLog before writing.
var liveJournaledMethods = []string{
	"AddNode", "AddEdge", "AddEdgeH", "RemoveEdge", "RemoveEdgeByHandle", "RemoveNode",
	"SetEdgeLabel", "IncEdgeCreateCount", "DecEdgeCreateCount", "SetEdgeLabelAt",
	"RemoveEdgeInstance", "SetEdgeLabelByHandle", "RemoveEdgeInstanceByHandle", "RemoveAllEdgesFrom",
}

// liveUnjournaledMethods change nothing a live run is derived from: reads, node
// labels and properties, and relationship properties.
var liveUnjournaledMethods = []string{
	"IsTombstoned", "SetNodeLabel", "RemoveNodeLabel", "SetNodeProperty", "DelNodeProperty",
	"NodeProperties", "NodeLabels", "HasEdge", "SetEdgeProperty", "DelEdgeProperty",
	"EdgeProperties", "EdgeLabels", "EdgeCreateCount", "EdgeLabelsAt", "SetEdgePropertyAt",
	"EdgePropertiesAt", "EdgeLabelsByHandle", "SetEdgePropertyByHandle", "DelEdgePropertyByHandle",
	"EdgePropertiesByHandle", "FirstEdgeHandle", "OutNeighbours", "InNeighbours", "OutDegree",
	"ResolveNodeID", "ResolveNodeLabel", "WalkNodeIDs",
}

// TestLiveTopo_GraphMutatorMethodsAreClassified fails when a method is added to
// exec.GraphMutator without being classified above, so a new write path cannot
// bypass the journal without someone deciding that it may.
func TestLiveTopo_GraphMutatorMethodsAreClassified(t *testing.T) {
	iface := reflect.TypeFor[exec.GraphMutator]()
	known := map[string]bool{}
	for _, m := range liveJournaledMethods {
		known[m] = true
	}
	for _, m := range liveUnjournaledMethods {
		known[m] = true
	}
	for i := range iface.NumMethod() {
		if name := iface.Method(i).Name; !known[name] {
			t.Errorf("exec.GraphMutator.%s is not classified for the rmp #2883 journal", name)
		}
	}
	if len(known) != iface.NumMethod() {
		t.Errorf("classified %d methods, the interface has %d: a listed method no longer exists", len(known), iface.NumMethod())
	}
}

// TestLiveTopo_EveryAdjacencyWriteIsJournaled calls every journaled method on
// both concrete adapters with a live Init armed, and requires each to move the
// journal. It is what makes the classification above a fact about the code.
func TestLiveTopo_EveryAdjacencyWriteIsJournaled(t *testing.T) {
	calls := map[string]func(m exec.GraphMutator){
		"AddNode":            func(m exec.GraphMutator) { _, _ = m.AddNode("z") },
		"AddEdge":            func(m exec.GraphMutator) { _, _, _ = m.AddEdge("a", "b", 0) },
		"AddEdgeH":           func(m exec.GraphMutator) { _, _, _, _ = m.AddEdgeH("a", "b", 0) },
		"RemoveEdge":         func(m exec.GraphMutator) { m.RemoveEdge("a", "b") },
		"RemoveEdgeByHandle": func(m exec.GraphMutator) { m.RemoveEdgeByHandle("a", "b", 1) },
		"RemoveNode":         func(m exec.GraphMutator) { m.RemoveNode("c") },
		"SetEdgeLabel":       func(m exec.GraphMutator) { m.SetEdgeLabel("a", "b", "T") },
		"IncEdgeCreateCount": func(m exec.GraphMutator) { m.IncEdgeCreateCount("a", "b") },
		"DecEdgeCreateCount": func(m exec.GraphMutator) { m.DecEdgeCreateCount("a", "b") },
		"SetEdgeLabelAt":     func(m exec.GraphMutator) { m.SetEdgeLabelAt("a", "b", 1, "T") },
		"RemoveEdgeInstance": func(m exec.GraphMutator) { m.RemoveEdgeInstance("a", "b", 1) },
		"SetEdgeLabelByHandle": func(m exec.GraphMutator) {
			m.SetEdgeLabelByHandle("a", "b", 1, "T")
		},
		"RemoveEdgeInstanceByHandle": func(m exec.GraphMutator) { m.RemoveEdgeInstanceByHandle("a", "b", 1) },
		"RemoveAllEdgesFrom":         func(m exec.GraphMutator) { m.RemoveAllEdgesFrom("a") },
	}
	if len(calls) != len(liveJournaledMethods) {
		t.Fatalf("%d calls for %d journaled methods", len(calls), len(liveJournaledMethods))
	}
	for _, method := range liveJournaledMethods {
		call, ok := calls[method]
		if !ok {
			t.Fatalf("no call for journaled method %s", method)
		}
		for _, kind := range []string{"lpg", "wal"} {
			t.Run(method+"/"+kind, func(t *testing.T) {
				g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
				for _, n := range []string{"a", "b", "c"} {
					if err := g.AddNode(n); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := g.AddEdgeH("a", "b", 0); err != nil {
					t.Fatal(err)
				}
				snap := g.BeginRead()
				defer g.EndRead(snap)
				var m exec.GraphMutator
				var log *liveTopoLog
				switch kind {
				case "lpg":
					a := &lpgMutatorAdapter{g: g}
					m, log = a, &a.liveTopo
				default:
					a := &walMutatorAdapter{g: g}
					m, log = a, &a.liveTopo
				}
				if !log.bind(g.ReadAt(snap)) {
					t.Fatal("bind refused the first view")
				}
				log.begin()
				before := log.seq.Load()
				func() {
					// The wal adapter has no WAL transaction behind it here, so a
					// method may fail AFTER reporting to the journal. The journal
					// entry is what this test is about.
					defer func() { _ = recover() }()
					call(m)
				}()
				if log.seq.Load() == before {
					t.Fatalf("%s on the %s adapter wrote without journaling", method, kind)
				}
			})
		}
	}
}

// TestLiveTopo_CaptureRule exercises the journal directly: a node written after
// an Init is served from its capture, a node written before it is served live,
// and liveness follows the same rule.
func TestLiveTopo_CaptureRule(t *testing.T) {
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, n := range []string{"a", "b", "c"} {
		if err := g.AddNode(n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.AddEdgeH("a", "b", 0); err != nil {
		t.Fatal(err)
	}
	snap := g.BeginRead()
	defer g.EndRead(snap)
	a := &lpgMutatorAdapter{g: g}
	log := &a.liveTopo
	view := g.ReadAt(snap)
	if !log.bind(view) {
		t.Fatal("bind refused the first view")
	}
	src := &liveOutSource{log: log, g: view}
	idOf := func(k string) graph.NodeID {
		id, ok := g.AdjList().Mapper().Lookup(k)
		if !ok {
			t.Fatalf("no id for %s", k)
		}
		return id
	}
	at, _ := src.LiveBegin()
	// Nothing written: the fast path serves the present.
	d, _, _, _, hc := src.LiveOutRun(idOf("a"), at, nil, nil, nil)
	if len(d) != 1 || !hc {
		t.Fatalf("initial run = %v (handle column %v), want one handled arc", d, hc)
	}
	log.beforeAdjWrite("a")
	if len(log.caps[idOf("a")]) != 1 {
		t.Fatalf("the first write after an Init took no capture")
	}
	log.beforeAdjWrite("a")
	if len(log.caps[idOf("a")]) != 1 {
		t.Fatalf("a second write in the same Init epoch took a second capture")
	}
	log.beforeNodeRemove("b")
	if evs := log.lives[idOf("b")]; len(evs) != 1 || !evs[0].wasLive {
		t.Fatalf("liveness event = %v, want one recording live", evs)
	}
	// A later Init needs a newer capture for its own instant.
	at2, _ := src.LiveBegin()
	if at2 <= at {
		t.Fatalf("second Init instant %d not after first %d", at2, at)
	}
	log.beforeAdjWrite("a")
	if len(log.caps[idOf("a")]) != 2 {
		t.Fatalf("a write after a newer Init took no capture for it")
	}
}
