package cypher

// reltype_column_differential_test.go — the differential that certifies the
// slot-aligned relationship-type column against the position-keyed filter map it
// replaced (rmp #2251).
//
// # What this proves, and why it is the right proof
//
// The map and the column SHARE their resolution: [forEachResolvedSlotType] is the
// pre-#2251 buildEdgeTypeFilter's three-tier resolver, lifted out with nothing
// removed but the final accept-set filtering (see its doc, and
// buildEdgeTypeFilterOracle in reltype_column_oracle_test.go). So the only axis on
// which the two can disagree is what they DO with that resolution — which is
// exactly the axis these tests isolate:
//
//   - FORWARD: for every arc position of every fixture, membership of the oracle
//     map must equal the column's admission verdict.
//   - REVERSE: for every reverse slot the column claims to know, its verdict must
//     equal the verdict the pre-#2251 Expand computed by recovering the slot's
//     forward position and probing the map there.
//
// The fixtures are chosen to be the shapes the four defects the task names were
// found on, so a resolution regression reopens one of them HERE, in a
// position-level comparison, rather than as a wrong row count somewhere downstream:
//
//   - rmp #2258 — a pair holding one typed and one untyped parallel arc, and a
//     self-loop with many slots of which one is typed. The pair's DERIVED union
//     matched `[r:K]` once per slot where once in total is correct.
//   - rmp #2293 — arc positions shifting under an insertion, so a structure keyed
//     by position is MISALIGNED rather than merely stale.
//   - TCK Match2 [6] / Match7 [29] — per-instance typing across parallel arcs and
//     the multi-type arc, which the by-handle record and the pair overflow carry
//     and the raw adjacency label column does not.
//
// Every fixture is also driven through the three type-set shapes the runtime can
// present: a single type, a multi-type disjunction `[r:A|B]`, and a type the graph
// has never interned.

import (
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// nodeID is graph.NodeID under a local name, so this file does not need the csr
// import purely for a conversion.
func nodeID(v uint64) graph.NodeID { return graph.NodeID(v) }

// relTypeFixture is one graph shape plus the relationship-type sets to drive it
// with.
type relTypeFixture struct {
	build func(t *testing.T) *lpg.Graph[string, float64]
	name  string
	why   string
	sets  [][]string
}

// cypherGraph builds a multigraph by running Cypher statements, so every arc's
// type is recorded BY HANDLE — the path every real Cypher workload takes.
func cypherGraph(stmts ...string) func(t *testing.T) *lpg.Graph[string, float64] {
	return func(t *testing.T) *lpg.Graph[string, float64] {
		t.Helper()
		g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
		eng := NewEngine(g)
		for _, s := range stmts {
			degreeRun(t, eng, s)
		}
		return g
	}
}

// relTypeFixtures is the shape battery. Keep the "why" current: it is what tells
// the next reader which defect a failing row has reopened.
func relTypeFixtures() []relTypeFixture {
	single := [][]string{{"K"}, {"K", "M"}, {"NEVER_INTERNED"}}

	return []relTypeFixture{
		{
			name: "cypher_mixed_typed_and_untyped_parallel",
			why:  "rmp #2258: the pair's DERIVED union matched [r:K] once per parallel slot",
			build: cypherGraph(
				`CREATE (:P {sid:'a'}), (:P {sid:'b'})`,
				`MATCH (a:P {sid:'a'}), (b:P {sid:'b'}) CREATE (a)-[:K]->(b)`,
				`MATCH (a:P {sid:'a'}), (b:P {sid:'b'}) CREATE (a)-[:M]->(b)`,
				`MATCH (a:P {sid:'a'}), (b:P {sid:'b'}) CREATE (a)-[:K]->(b)`,
			),
			sets: single,
		},
		{
			name: "cypher_self_loop_many_slots_one_typed",
			why:  "rmp #2258: a 12-slot self-loop with one :K matched [r:K] twelve times",
			build: func() func(t *testing.T) *lpg.Graph[string, float64] {
				stmts := []string{`CREATE (:P {sid:'s'})`,
					`MATCH (a:P {sid:'s'}) CREATE (a)-[:K]->(a)`}
				for i := 0; i < 11; i++ {
					stmts = append(stmts, `MATCH (a:P {sid:'s'}) CREATE (a)-[:M]->(a)`)
				}
				return cypherGraph(stmts...)
			}(),
			sets: single,
		},
		{
			name: "cypher_position_shift_under_insertion",
			why:  "rmp #2293: an earlier source gaining an arc shifts every later arc's position",
			build: cypherGraph(
				`CREATE (:P {sid:'x'}), (:P {sid:'a'}), (:P {sid:'b'}), (:P {sid:'c'})`,
				`MATCH (a:P {sid:'a'}), (b:P {sid:'b'}) CREATE (a)-[:K]->(b)`,
				`MATCH (x:P {sid:'x'}), (c:P {sid:'c'}) CREATE (x)-[:M]->(c)`,
			),
			sets: single,
		},
		{
			name:  "goapi_column_typed_slots",
			why:   "the Go API records types in the adjacency COLUMN, not by handle",
			build: goAPIGraph,
			sets:  single,
		},
		{
			name:  "mixed_cypher_and_goapi_on_one_pair",
			why:   "TCK Match2 [6]/Match7 [29]: a per-PAIR ordinal handed a Go-API slot a CREATE's type",
			build: mixedOriginGraph,
			sets:  single,
		},
		{
			name:  "multi_type_single_arc",
			why:   "a handle's label bag is a SET, so one arc can carry several types",
			build: multiTypeArcGraph,
			sets:  [][]string{{"K"}, {"M"}, {"K", "M"}, {"K", "NEVER_INTERNED"}, {"NEVER_INTERNED"}},
		},
	}
}

// goAPIGraph builds through the raw Go API, whose arcs carry their type in the
// adjacency label column rather than against a per-edge handle record.
func goAPIGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	keys := []string{"n0", "n1", "n2", "n3", "n4"}
	for _, k := range keys {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", k, err)
		}
	}
	// A ring plus two chords, so in- and out-degree both vary and some nodes have
	// several incoming arcs (which is what leaves the reverse side something to
	// disagree about).
	arcs := [][3]string{
		{"n0", "n1", "K"}, {"n1", "n2", "M"}, {"n2", "n3", "K"},
		{"n3", "n4", "M"}, {"n4", "n0", "K"}, {"n0", "n3", "M"}, {"n1", "n3", "K"},
	}
	for _, a := range arcs {
		if err := g.AddEdge(a[0], a[1], 1); err != nil {
			t.Fatalf("AddEdge(%v): %v", a, err)
		}
		g.SetEdgeLabel(a[0], a[1], a[2])
	}
	return g
}

// mixedOriginGraph puts a Cypher-created arc and a Go-API arc on the SAME pair.
// The two record their type by different mechanisms, which is the shape the
// positional inference used to answer wrongly.
func mixedOriginGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	eng := NewEngine(g)
	degreeRun(t, eng, `CREATE (:P {sid:'a'}), (:P {sid:'b'})`)
	degreeRun(t, eng, `MATCH (a:P {sid:'a'}), (b:P {sid:'b'}) CREATE (a)-[:K]->(b)`)
	// Now a Go-API arc on the same pair, one typed :M and one left untyped.
	a, b := keyOf(t, g, "a"), keyOf(t, g, "b")
	if err := g.AddEdge(a, b, 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel(a, b, "M")
	if err := g.AddEdge(a, b, 1); err != nil {
		t.Fatalf("AddEdge (untyped): %v", err)
	}
	return g
}

// multiTypeArcGraph gives ONE arc two relationship types, which only the Go API
// can do: the by-handle label store holds a SET, and the pair overflow list holds
// second-and-later types.
func multiTypeArcGraph(t *testing.T) *lpg.Graph[string, float64] {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
	for _, k := range []string{"u", "v", "w"} {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
		if err := g.SetNodeLabel(k, "P"); err != nil {
			t.Fatalf("SetNodeLabel(%q): %v", k, err)
		}
	}
	if err := g.AddEdge("u", "v", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel("u", "v", "K")
	g.SetEdgeLabel("u", "v", "M") // second type on the same pair → overflow
	if err := g.AddEdge("v", "w", 1); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	g.SetEdgeLabel("v", "w", "M")
	return g
}

// keyOf returns the external key of the node whose sid property is sid.
func keyOf(t *testing.T, g *lpg.Graph[string, float64], sid string) string {
	t.Helper()
	view := g.ReadAt(nil)
	adj := view.AdjList()
	mapper := adj.Mapper()
	for id := uint64(0); id <= uint64(adj.MaxNodeID()); id++ {
		k, ok := mapper.Resolve(nodeID(id))
		if !ok {
			continue
		}
		if v, ok := view.GetNodeProperty(k, "sid"); ok {
			if sv, ok := v.String(); ok && sv == sid {
				return k
			}
		}
	}
	t.Fatalf("no node with sid=%q", sid)
	return ""
}

// TestRelTypeColumn_MatchesFilterMapOracle is the differential. For every fixture
// and every type set, the column's verdict must equal the retired map's, position
// for position, in BOTH directions.
func TestRelTypeColumn_MatchesFilterMapOracle(t *testing.T) {
	for _, f := range relTypeFixtures() {
		t.Run(f.name, func(t *testing.T) {
			g := f.build(t)
			view := g.ReadAt(nil)
			fwd, rev := csrPairFromGraph(view)
			if len(fwd.EdgesSlice()) == 0 {
				t.Fatalf("fixture %q built no arcs, so this comparison is vacuous", f.name)
			}
			for _, set := range f.sets {
				t.Run(fmt.Sprint(set), func(t *testing.T) {
					diffs, colAdmit := columnAdmitOracleDiff(view, fwd, rev, set)
					if len(diffs) != 0 {
						t.Errorf("FORWARD admission diverges from the retired filter map at %d "+
							"position(s) %v (column said %v).\n  fixture: %s\n  why it matters: %s",
							len(diffs), diffs, colAdmit, f.name, f.why)
					}
					if rd := revColumnOracleDiff(view, fwd, rev, set); len(rd) != 0 {
						t.Errorf("REVERSE admission diverges from the pre-#2251 Expand at %d slot(s):\n"+
							"    %v\n  fixture: %s\n  why it matters: %s", len(rd), rd, f.name, f.why)
					}
				})
			}
		})
	}
}

// TestRelTypeColumn_CoversEveryArc guards against the differential above passing
// vacuously. A column that admitted NOTHING would agree with an oracle that also
// admitted nothing, so at least one fixture/type-set pair must actually admit some
// arcs and reject others — otherwise the comparison has no discriminating power.
func TestRelTypeColumn_CoversEveryArc(t *testing.T) {
	sawAdmit, sawReject := false, false
	for _, f := range relTypeFixtures() {
		g := f.build(t)
		view := g.ReadAt(nil)
		fwd, rev := csrPairFromGraph(view)
		col := buildRelTypeColumn(view, fwd, rev)
		if got, want := col.RelTypeColumnFwdLenForTest(), len(fwd.EdgesSlice()); got != want {
			t.Errorf("%s: column describes %d forward arcs, CSR has %d", f.name, got, want)
		}
		admit := col.Admit(relTypeCodesFor(view, []string{"K"}))
		for pos := uint64(0); pos < uint64(len(fwd.EdgesSlice())); pos++ {
			if admit.Fwd(pos) {
				sawAdmit = true
			} else {
				sawReject = true
			}
		}
	}
	if !sawAdmit || !sawReject {
		t.Fatalf("the battery never both admitted and rejected an arc (admit=%v reject=%v); "+
			"the differential above cannot discriminate and proves nothing", sawAdmit, sawReject)
	}
}
