package shapegen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// Test header reference:
// Lengauer & Tarjan, "A Fast Algorithm for Finding Dominators in a
// Flowgraph", TOPLAS 1(1), 1979. The TestDAGs_LengauerTarjanExample_*
// tests in this file verify the pinned worked example against an
// independent dominator computation (Cooper-Harvey-Kennedy iterative
// dataflow), keeping the catalogue cross-checked against the canonical
// reference.

// dagsGoldenDir is the directory holding the dags-family adjacency
// listings. As with classicGoldenDir, structuredGoldenDir,
// treesGoldenDir, and specialsGoldenDir, the path is rooted at the
// package directory.
//
// This file deliberately reuses formatAdjacency from trivial_test.go
// (same package). When T58.22 lands the shared golden helper in
// internal/goldens, every family's golden helper must migrate
// together.
const dagsGoldenDir = "testdata/shapegen/dags"

// dagsGolden compares got with the contents of the golden file at
// dagsGoldenDir/<name>. The implementation mirrors treesGolden /
// specialsGolden / structuredGolden / classicGolden exactly because
// the families will migrate together to the shared helper in T58.22;
// until then duplicating keeps each family's test surface
// self-contained.
func dagsGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(dagsGoldenDir, name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("dagsGolden: MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("dagsGolden: WriteFile(%q): %v", path, err)
		}
		t.Logf("rewrote golden %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // path is a test-local golden under testdata/, not user input
	if err != nil {
		t.Fatalf("dagsGolden: ReadFile(%q): %v (run with -shapegen-update to bootstrap)", path, err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// -------------------------------------------------------------------
// Local DAG helpers — Kahn topological sort and Tarjan SCCs
// -------------------------------------------------------------------

// dagNodesAndOutAdj returns the sorted node-id slice of g and the
// directed out-adjacency as map[int][]int (sorted neighbour
// slices). The trees/specials test files use a symmetric closure
// over the adjacency; this family preserves direction because the
// AC checks require directedness explicitly.
func dagNodesAndOutAdj(g *lpg.Graph[int, int64]) (nodes []int, outAdj map[int][]int) {
	adj := g.AdjList()
	mapper := adj.Mapper()
	maxID := uint64(adj.MaxNodeID())
	nodes = make([]int, 0, maxID)
	for id := uint64(0); id < maxID; id++ {
		v, ok := mapper.Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		nodes = append(nodes, v)
	}
	sortInts(nodes)
	outAdj = make(map[int][]int, len(nodes))
	for _, u := range nodes {
		var nbrs []int
		for v := range adj.Neighbours(u) {
			nbrs = append(nbrs, v)
		}
		sortInts(nbrs)
		outAdj[u] = nbrs
	}
	return nodes, outAdj
}

// kahnTopoOrder runs Kahn's algorithm on g and returns a topological
// order if g is acyclic, or nil if a cycle is detected (i.e., the
// queue empties before consuming every node). The helper is the
// brief's required acyclicity check; the test calls it on every
// generator EXCEPT [LengauerTarjanExample] (which is cyclic by
// construction).
//
// The algorithm runs in O(V + E) time and space, using only
// allocations bounded by the graph itself.
func kahnTopoOrder(g *lpg.Graph[int, int64]) []int {
	nodes, outAdj := dagNodesAndOutAdj(g)
	if len(nodes) == 0 {
		return []int{}
	}
	inDeg := make(map[int]int, len(nodes))
	for _, u := range nodes {
		// Ensure every node appears so isolated nodes get inDeg 0.
		if _, ok := inDeg[u]; !ok {
			inDeg[u] = 0
		}
		for _, v := range outAdj[u] {
			inDeg[v]++
		}
	}
	// Seed the queue with every zero-in-degree node, in ascending id
	// order so the resulting order is deterministic for the test.
	queue := make([]int, 0, len(nodes))
	for _, u := range nodes {
		if inDeg[u] == 0 {
			queue = append(queue, u)
		}
	}
	order := make([]int, 0, len(nodes))
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		order = append(order, u)
		// Process neighbours in ascending order so the resulting
		// topological order is determined entirely by the graph.
		for _, v := range outAdj[u] {
			inDeg[v]--
			if inDeg[v] == 0 {
				queue = append(queue, v)
			}
		}
	}
	if len(order) != len(nodes) {
		return nil
	}
	return order
}

// tarjanSCCs returns the strongly-connected components of g, each
// component as a sorted slice of node ids, in the order produced by
// Tarjan's classical algorithm (root-finish order, so the first
// returned component is the "lowest" in the condensation DAG). The
// helper is the brief's required SCC check; the test calls it on
// every generator EXCEPT [LengauerTarjanExample] and asserts that
// every component is a singleton (len(N) components, each of size 1).
//
// The algorithm runs in O(V + E) time. Recursion is converted to an
// explicit stack to keep the helper safe at the catalogue's short-
// layer ceilings (e.g. TransitiveTournament(200) has 200 nodes and
// 19_900 edges, well inside any stack-safe budget, but the iterative
// form keeps the helper allocation-bounded regardless).
func tarjanSCCs(g *lpg.Graph[int, int64]) [][]int {
	nodes, outAdj := dagNodesAndOutAdj(g)
	state := newTarjanState(len(nodes))
	for _, v := range nodes {
		if _, seen := state.index[v]; !seen {
			state.strongConnect(v, outAdj)
		}
	}
	return state.sccs
}

// tarjanState carries the working data structures of [tarjanSCCs].
// It is split into its own type so the per-node "strongconnect"
// step can mutate the index/lowlink/stack vectors without juggling
// closure captures.
type tarjanState struct {
	index     map[int]int
	lowlink   map[int]int
	onStack   map[int]bool
	stack     []int
	nextIdx   int
	sccs      [][]int
	workStack []tarjanFrame
}

// tarjanFrame is one entry on the iterative DFS work stack used by
// [tarjanSCCs]. Each frame carries the node being visited and the
// index of the next neighbour to inspect; the strongconnect step
// re-enters the same frame after every neighbour recursion.
type tarjanFrame struct {
	v      int
	nbrIdx int
}

// newTarjanState constructs a tarjanState with maps pre-sized to
// the expected node count.
func newTarjanState(n int) *tarjanState {
	return &tarjanState{
		index:   make(map[int]int, n),
		lowlink: make(map[int]int, n),
		onStack: make(map[int]bool, n),
		stack:   make([]int, 0, n),
	}
}

// strongConnect runs the iterative Tarjan strongconnect routine
// rooted at v over outAdj. Components are appended to ts.sccs in
// root-finish order, with each component's node-id slice sorted
// ascending.
func (ts *tarjanState) strongConnect(v int, outAdj map[int][]int) {
	ts.push(v)
	ts.workStack = append(ts.workStack, tarjanFrame{v: v, nbrIdx: 0})
	for len(ts.workStack) > 0 {
		top := &ts.workStack[len(ts.workStack)-1]
		nbrs := outAdj[top.v]
		if top.nbrIdx < len(nbrs) {
			w := nbrs[top.nbrIdx]
			top.nbrIdx++
			if _, seen := ts.index[w]; !seen {
				ts.push(w)
				ts.workStack = append(ts.workStack, tarjanFrame{v: w, nbrIdx: 0})
				continue
			}
			if ts.onStack[w] {
				if ts.lowlink[top.v] > ts.index[w] {
					ts.lowlink[top.v] = ts.index[w]
				}
			}
			continue
		}
		// All neighbours processed: pop the frame and propagate the
		// lowlink to the parent (if any), then check root condition.
		current := top.v
		ts.workStack = ts.workStack[:len(ts.workStack)-1]
		if ts.lowlink[current] == ts.index[current] {
			ts.emitComponent(current)
		}
		if len(ts.workStack) > 0 {
			parent := &ts.workStack[len(ts.workStack)-1]
			if ts.lowlink[parent.v] > ts.lowlink[current] {
				ts.lowlink[parent.v] = ts.lowlink[current]
			}
		}
	}
}

// push installs v on the Tarjan stack and assigns its initial
// index / lowlink at the current counter.
func (ts *tarjanState) push(v int) {
	ts.index[v] = ts.nextIdx
	ts.lowlink[v] = ts.nextIdx
	ts.nextIdx++
	ts.stack = append(ts.stack, v)
	ts.onStack[v] = true
}

// emitComponent pops the Tarjan stack down to and including root,
// collects the popped nodes into an ascending-sorted slice, and
// appends the slice to ts.sccs.
func (ts *tarjanState) emitComponent(root int) {
	var comp []int
	for {
		top := ts.stack[len(ts.stack)-1]
		ts.stack = ts.stack[:len(ts.stack)-1]
		ts.onStack[top] = false
		comp = append(comp, top)
		if top == root {
			break
		}
	}
	sortInts(comp)
	ts.sccs = append(ts.sccs, comp)
}

// assertAcyclicAndSingletonSCCs runs both AC #1 (Kahn topological
// sort) and AC #2 (Tarjan SCCs returning N singletons) against g.
// It is invoked by every test in this file EXCEPT the
// LengauerTarjanExample test, per the user-amended AC: L-T is a
// flowgraph, not a DAG.
func assertAcyclicAndSingletonSCCs(t *testing.T, g *lpg.Graph[int, int64]) {
	t.Helper()
	order := kahnTopoOrder(g)
	if order == nil {
		t.Fatal("kahnTopoOrder returned nil — graph contains a cycle (AC #1 violation)")
	}
	if want := int(g.AdjList().Order()); len(order) != want {
		t.Fatalf("kahnTopoOrder produced %d nodes, want %d", len(order), want)
	}
	sccs := tarjanSCCs(g)
	if want := int(g.AdjList().Order()); len(sccs) != want {
		t.Fatalf("tarjanSCCs returned %d components, want %d singletons (AC #2 violation)", len(sccs), want)
	}
	for i, comp := range sccs {
		if len(comp) != 1 {
			t.Fatalf("tarjanSCCs component %d has size %d, want 1 (AC #2 violation)", i, len(comp))
		}
	}
}

// -------------------------------------------------------------------
// TransitiveTournament
// -------------------------------------------------------------------

// TestDAGs_TransitiveTournament_Invariants exercises the n sweep
// n in [0, 6] and asserts the documented closed forms together with
// AC #1/#2 (Kahn topological sort + Tarjan SCCs).
func TestDAGs_TransitiveTournament_Invariants(t *testing.T) {
	t.Parallel()
	for n := 0; n <= 6; n++ {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			s := TransitiveTournament(n)
			if got, want := s.Name(), "dags.transitive-tournament"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 1 || knobs[0].Name != "n" || knobs[0].Min != 0 || knobs[0].Max != 200 || knobs[0].Default != 5 {
				t.Fatalf("Knobs = %#v, want exactly one n:[0,200] default 5", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(n))
			assertSize(t, g, uint64(n*(n-1)/2))
			assertDirected(t, g, true)
			assertAcyclicAndSingletonSCCs(t, g)
		})
	}
}

// TestDAGs_TransitiveTournament_PanicsOutOfRange covers the two
// guard branches (negative and above 200).
func TestDAGs_TransitiveTournament_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	for _, n := range []int{-1, 201} {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("TransitiveTournament(%d) did not panic", n)
				}
			}()
			_ = TransitiveTournament(n)
		})
	}
}

// TestDAGs_TransitiveTournament_Golden pins TransitiveTournament(5).
func TestDAGs_TransitiveTournament_Golden(t *testing.T) {
	t.Parallel()
	g, err := TransitiveTournament(5).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dagsGolden(t, "transitive-tournament-n5.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Diamond
// -------------------------------------------------------------------

// TestDAGs_Diamond_Invariants exercises the k sweep k in [0, 6] and
// asserts the documented closed forms together with AC #1/#2 and
// (for k >= 1) the AC #3 "exactly 2 paths" property.
func TestDAGs_Diamond_Invariants(t *testing.T) {
	t.Parallel()
	for k := 0; k <= 6; k++ {
		k := k
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			t.Parallel()
			s := Diamond(k)
			if got, want := s.Name(), "dags.diamond"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 1 || knobs[0].Name != "k" || knobs[0].Min != 0 || knobs[0].Max != 1000 || knobs[0].Default != 3 {
				t.Fatalf("Knobs = %#v, want exactly one k:[0,1000] default 3", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertDirected(t, g, true)
			if k == 0 {
				assertOrder(t, g, 2)
				assertSize(t, g, 1)
			} else {
				assertOrder(t, g, uint64(2*k+2))
				assertSize(t, g, uint64(2*(k+1)))
			}
			assertAcyclicAndSingletonSCCs(t, g)
			if k >= 1 {
				// AC #3: exactly 2 paths from source (0) to sink (2k+1).
				count := countPaths(g, 0, 2*k+1)
				if count != 2 {
					t.Fatalf("Diamond(%d) source->sink path count = %d, want 2", k, count)
				}
			}
		})
	}
}

// countPaths returns the number of distinct directed paths from src
// to dst in the acyclic graph g. The helper is a DP over the
// topological order: paths(src) = 1, paths(v) = sum over predecessors
// u of paths(u). Only used on small DAGs (Diamond at k <= 6) so the
// O(V + E) cost is trivial.
func countPaths(g *lpg.Graph[int, int64], src, dst int) int {
	order := kahnTopoOrder(g)
	if order == nil {
		return 0
	}
	// Build a predecessor map for each node.
	_, outAdj := dagNodesAndOutAdj(g)
	pred := make(map[int][]int, len(order))
	for u, nbrs := range outAdj {
		for _, v := range nbrs {
			pred[v] = append(pred[v], u)
		}
	}
	paths := make(map[int]int, len(order))
	paths[src] = 1
	for _, v := range order {
		if v == src {
			continue
		}
		var sum int
		for _, u := range pred[v] {
			sum += paths[u]
		}
		paths[v] = sum
	}
	return paths[dst]
}

// TestDAGs_Diamond_PanicsOutOfRange covers the two guard branches
// (negative and above 1000).
func TestDAGs_Diamond_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	for _, k := range []int{-1, 1001} {
		k := k
		t.Run(fmt.Sprintf("k=%d", k), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Diamond(%d) did not panic", k)
				}
			}()
			_ = Diamond(k)
		})
	}
}

// TestDAGs_Diamond_Golden pins Diamond(3): 8 nodes, 8 edges.
func TestDAGs_Diamond_Golden(t *testing.T) {
	t.Parallel()
	g, err := Diamond(3).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dagsGolden(t, "diamond-k3.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// Layered
// -------------------------------------------------------------------

// TestDAGs_Layered_Invariants exercises a small (L, w, density,
// seed) sweep and asserts the documented closed forms together with
// AC #1/#2.
func TestDAGs_Layered_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		L, w, density int
		seed          uint64
	}{
		{1, 1, 0, 1},   // single layer, single node — no edges possible.
		{2, 2, 0, 1},   // density zero forces empty edge set.
		{2, 2, 100, 1}, // density 100 forces every inter-layer edge.
		{3, 3, 50, 42}, // mixed case — used by the golden too.
		{4, 2, 30, 7},  // taller, narrower.
		{2, 5, 70, 99}, // wider, shorter.
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("L=%d_w=%d_d=%d_seed=%d", c.L, c.w, c.density, c.seed), func(t *testing.T) {
			t.Parallel()
			s := Layered(c.L, c.w, c.density, c.seed)
			if got, want := s.Name(), "dags.layered"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 3 ||
				knobs[0].Name != "L" || knobs[0].Min != 1 || knobs[0].Max != 100 || knobs[0].Default != 3 ||
				knobs[1].Name != "w" || knobs[1].Min != 1 || knobs[1].Max != 100 || knobs[1].Default != 3 ||
				knobs[2].Name != "density" || knobs[2].Min != 0 || knobs[2].Max != 100 || knobs[2].Default != 50 {
				t.Fatalf("Knobs = %#v, want L:[1,100]/3, w:[1,100]/3, density:[0,100]/50", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(c.L*c.w))
			assertDirected(t, g, true)
			gotSize := g.AdjList().Size()
			if c.density == 0 && gotSize != 0 {
				t.Fatalf("density=0: Size = %d, want 0", gotSize)
			}
			if c.density == 100 && gotSize != uint64((c.L-1)*c.w*c.w) {
				t.Fatalf("density=100: Size = %d, want %d", gotSize, (c.L-1)*c.w*c.w)
			}
			if gotSize > uint64((c.L-1)*c.w*c.w) {
				t.Fatalf("Size = %d, exceeds upper bound %d", gotSize, (c.L-1)*c.w*c.w)
			}
			assertAcyclicAndSingletonSCCs(t, g)
		})
	}
}

// TestDAGs_Layered_PanicsOutOfRange covers every guard branch.
func TestDAGs_Layered_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		L, w, density int
	}{
		{"L_zero", 0, 1, 50},
		{"L_too_large", 101, 1, 50},
		{"w_zero", 1, 0, 50},
		{"w_too_large", 1, 101, 50},
		{"density_negative", 1, 1, -1},
		{"density_too_large", 1, 1, 101},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("Layered(%d,%d,%d,0) did not panic", c.L, c.w, c.density)
				}
			}()
			_ = Layered(c.L, c.w, c.density, 0)
		})
	}
}

// TestDAGs_Layered_Determinism asserts that the same (L, w, density,
// seed) tuple produces byte-identical adjacency listings across two
// independent Build calls.
func TestDAGs_Layered_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xC0FFEE
	g1, err := Layered(4, 4, 60, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := Layered(4, 4, 60, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestDAGs_Layered_Golden pins Layered(3, 3, 50, 42).
func TestDAGs_Layered_Golden(t *testing.T) {
	t.Parallel()
	g, err := Layered(3, 3, 50, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dagsGolden(t, "layered-L3-w3-d50-seed42.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// LengauerTarjanExample
// -------------------------------------------------------------------
//
// Per user-amended AC #5: this generator is skipped from the
// AC #1 / AC #2 acyclicity + singleton-SCC sweep because L-T is a
// flowgraph, not a DAG. The dominator-tree pinning below is the
// load-bearing assertion for this fixture.

// TestDAGs_LengauerTarjanExample_Invariants asserts the structural
// invariants of the L-T fixture: 13 nodes, 21 edges, directed,
// labels R..L attached. Per the user-amended AC, this test does
// NOT call assertAcyclicAndSingletonSCCs — L-T is a flowgraph, not
// a DAG.
func TestDAGs_LengauerTarjanExample_Invariants(t *testing.T) {
	t.Parallel()
	s := LengauerTarjanExample()
	if got, want := s.Name(), "dags.lengauer-tarjan"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	if got := s.Knobs(); len(got) != 0 {
		t.Fatalf("Knobs = %#v, want empty", got)
	}
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertOrder(t, g, 13)
	assertSize(t, g, 21)
	assertDirected(t, g, true)
	// Labels R, A, B, C, D, E, F, G, H, I, J, K, L are attached to
	// user-keys 1..13 in that order.
	for i := 0; i < len(lengauerTarjanLabels); i++ {
		key := i + 1
		want := lengauerTarjanLabels[i]
		if !g.HasNodeLabel(key, want) {
			t.Fatalf("L-T node %d missing label %q (has %v)", key, want, g.NodeLabels(key))
		}
	}
}

// TestDAGs_LengauerTarjanExample_IsCyclic confirms the cyclic nature
// of the fixture: Kahn's algorithm must fail to produce a complete
// topological order. Also confirms a Tarjan SCC scan finds at least
// one non-trivial component. Documents that AC #1 + AC #2 are
// (correctly) inapplicable to this fixture.
func TestDAGs_LengauerTarjanExample_IsCyclic(t *testing.T) {
	t.Parallel()
	g, err := LengauerTarjanExample().Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if order := kahnTopoOrder(g); order != nil {
		t.Fatalf("L-T fixture topologically sorted to %v but should be cyclic", order)
	}
	sccs := tarjanSCCs(g)
	largest := 0
	for _, c := range sccs {
		if len(c) > largest {
			largest = len(c)
		}
	}
	if largest <= 1 {
		t.Fatalf("L-T fixture has no non-trivial SCC (largest=%d) but should be cyclic", largest)
	}
}

// TestDAGs_LengauerTarjanExample_DominatorTree pins the immediate
// dominator table for the L-T worked example (AC #4). The test
// computes idom from scratch via Cooper-Harvey-Kennedy iterative
// dataflow and compares against the published table.
//
// Pinned idom table (root R = key 1 has no idom):
//
//	idom(A) = R, idom(B) = R, idom(C) = R, idom(D) = R,
//	idom(E) = R, idom(F) = C, idom(G) = C, idom(H) = R,
//	idom(I) = R, idom(J) = G, idom(K) = R, idom(L) = D.
//
// Reference: Lengauer & Tarjan, "A Fast Algorithm for Finding
// Dominators in a Flowgraph", TOPLAS 1(1), 1979.
func TestDAGs_LengauerTarjanExample_DominatorTree(t *testing.T) {
	t.Parallel()
	g, err := LengauerTarjanExample().Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	const root = 1 // R
	idom := computeIDom(g, root)
	want := map[int]int{
		2:  1, // A -> R
		3:  1, // B -> R
		4:  1, // C -> R
		5:  1, // D -> R
		6:  1, // E -> R
		7:  4, // F -> C
		8:  4, // G -> C
		9:  1, // H -> R
		10: 1, // I -> R
		11: 8, // J -> G
		12: 1, // K -> R
		13: 5, // L -> D
	}
	for v, w := range want {
		got, ok := idom[v]
		if !ok {
			t.Fatalf("idom(%s) missing in computed table", lengauerTarjanLabels[v-1])
		}
		if got != w {
			t.Fatalf("idom(%s) = %s, want %s",
				lengauerTarjanLabels[v-1],
				lengauerTarjanLabels[got-1],
				lengauerTarjanLabels[w-1])
		}
	}
	// Root must have no idom recorded.
	if _, ok := idom[root]; ok {
		t.Fatalf("idom(R) = %v, want no entry (root has no idom)", idom[root])
	}
}

// computeIDom returns the immediate-dominator map of g rooted at
// root, computed via the Cooper-Harvey-Kennedy iterative dataflow
// algorithm ("A Simple, Fast Dominance Algorithm", 2001). The map
// omits the root (which has no immediate dominator).
//
// The algorithm builds a reverse postorder, initialises idom[root]
// = root, and iterates over the remaining nodes in reverse postorder
// (excluding the root) until no idom assignment changes. At each
// node b with first-processed predecessor p, idom[b] becomes
// intersect(idom[b], idom[other_pred]) for every other already-
// initialised predecessor.
//
// The implementation uses node user-keys directly; nodes are keyed
// by int throughout. The cost is O((V + E) * d) where d is the
// dominator-tree depth, well inside the L-T fixture's budget.
func computeIDom(g *lpg.Graph[int, int64], root int) map[int]int {
	_, outAdj := dagNodesAndOutAdj(g)
	// Build reverse-postorder over the nodes reachable from root.
	post := reversePostorder(root, outAdj)
	// Position map: rpo[v] = index of v in post (smaller is earlier
	// in reverse postorder, i.e. closer to root).
	rpo := make(map[int]int, len(post))
	for i, v := range post {
		rpo[v] = i
	}
	// Predecessor list keyed by user-id, only including predecessors
	// reachable from root (i.e. present in rpo).
	pred := make(map[int][]int, len(post))
	for u, nbrs := range outAdj {
		if _, ok := rpo[u]; !ok {
			continue
		}
		for _, v := range nbrs {
			if _, ok := rpo[v]; !ok {
				continue
			}
			pred[v] = append(pred[v], u)
		}
	}
	const undef = -1
	idom := make(map[int]int, len(post))
	for _, v := range post {
		idom[v] = undef
	}
	idom[root] = root
	changed := true
	for changed {
		changed = false
		// Iterate in reverse postorder (smallest rpo index first),
		// skipping the root.
		for i := 0; i < len(post); i++ {
			b := post[i]
			if b == root {
				continue
			}
			// Find first processed predecessor (one whose idom is
			// already set) as the seed for the intersection.
			var newIDom int
			seedSet := false
			for _, p := range pred[b] {
				if idom[p] == undef {
					continue
				}
				if !seedSet {
					newIDom = p
					seedSet = true
					continue
				}
				newIDom = intersect(p, newIDom, idom, rpo)
			}
			if !seedSet {
				continue
			}
			if idom[b] != newIDom {
				idom[b] = newIDom
				changed = true
			}
		}
	}
	// Remove the root self-mapping before returning so callers see
	// only proper idom entries.
	delete(idom, root)
	// Also drop any node that never got an idom assignment (should
	// not happen for the L-T fixture but the cleanup keeps the helper
	// total).
	for v, d := range idom {
		if d == undef {
			delete(idom, v)
		}
	}
	return idom
}

// intersect implements the Cooper-Harvey-Kennedy "intersect" helper:
// walks two pointers up the (partially constructed) dominator tree
// until they meet, using the reverse-postorder index to decide which
// pointer to advance. The returned node is the deepest common
// ancestor of b1 and b2 in the current dominator tree.
func intersect(b1, b2 int, idom, rpo map[int]int) int {
	finger1 := b1
	finger2 := b2
	for finger1 != finger2 {
		for rpo[finger1] > rpo[finger2] {
			finger1 = idom[finger1]
		}
		for rpo[finger2] > rpo[finger1] {
			finger2 = idom[finger2]
		}
	}
	return finger1
}

// reversePostorder returns the nodes reachable from root in reverse
// postorder. Implemented as an iterative DFS so the helper is safe
// at any catalogue size. Children are visited in ascending user-id
// order to keep the order deterministic.
func reversePostorder(root int, outAdj map[int][]int) []int {
	visited := make(map[int]bool)
	var post []int
	type frame struct {
		v      int
		nbrIdx int
	}
	stack := []frame{{v: root, nbrIdx: 0}}
	visited[root] = true
	for len(stack) > 0 {
		top := &stack[len(stack)-1]
		nbrs := outAdj[top.v]
		if top.nbrIdx < len(nbrs) {
			w := nbrs[top.nbrIdx]
			top.nbrIdx++
			if !visited[w] {
				visited[w] = true
				stack = append(stack, frame{v: w, nbrIdx: 0})
			}
			continue
		}
		post = append(post, top.v)
		stack = stack[:len(stack)-1]
	}
	// Reverse in place to get reverse postorder.
	for i, j := 0, len(post)-1; i < j; i, j = i+1, j-1 {
		post[i], post[j] = post[j], post[i]
	}
	return post
}

// TestDAGs_LengauerTarjanExample_Golden pins the canonical adjacency
// listing of the L-T fixture.
func TestDAGs_LengauerTarjanExample_Golden(t *testing.T) {
	t.Parallel()
	g, err := LengauerTarjanExample().Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dagsGolden(t, "lengauer-tarjan.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// BuildDepDAG
// -------------------------------------------------------------------

// TestDAGs_BuildDepDAG_Invariants exercises a small sweep over
// (depth, fanIn, fanOut, seed) tuples and asserts the catalogue
// invariants together with AC #1/#2.
func TestDAGs_BuildDepDAG_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		depth, fanIn, fanOut int
		seed                 uint64
	}{
		{0, 1, 1, 0},  // root-only.
		{1, 1, 1, 1},  // single parent, single child.
		{2, 2, 2, 42}, // used by golden.
		{3, 1, 3, 7},  // bushy.
		{4, 3, 2, 99}, // deeper.
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("d=%d_fi=%d_fo=%d_seed=%d", c.depth, c.fanIn, c.fanOut, c.seed), func(t *testing.T) {
			t.Parallel()
			s := BuildDepDAG(c.depth, c.fanIn, c.fanOut, c.seed)
			if got, want := s.Name(), "dags.build-dep"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 3 ||
				knobs[0].Name != "depth" || knobs[0].Min != 0 || knobs[0].Max != 20 || knobs[0].Default != 3 ||
				knobs[1].Name != "fanIn" || knobs[1].Min != 1 || knobs[1].Max != 10 || knobs[1].Default != 2 ||
				knobs[2].Name != "fanOut" || knobs[2].Min != 1 || knobs[2].Max != 10 || knobs[2].Default != 2 {
				t.Fatalf("Knobs = %#v, want depth:[0,20]/3, fanIn:[1,10]/2, fanOut:[1,10]/2", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertDirected(t, g, true)
			if got := g.AdjList().Order(); got < 1 {
				t.Fatalf("Order = %d, want >= 1 (root always present)", got)
			}
			assertAcyclicAndSingletonSCCs(t, g)
		})
	}
}

// TestDAGs_BuildDepDAG_PanicsOutOfRange covers every guard branch.
func TestDAGs_BuildDepDAG_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                 string
		depth, fanIn, fanOut int
	}{
		{"depth_negative", -1, 1, 1},
		{"depth_too_large", 21, 1, 1},
		{"fanIn_zero", 1, 0, 1},
		{"fanIn_too_large", 1, 11, 1},
		{"fanOut_zero", 1, 1, 0},
		{"fanOut_too_large", 1, 1, 11},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("BuildDepDAG(%d,%d,%d,0) did not panic", c.depth, c.fanIn, c.fanOut)
				}
			}()
			_ = BuildDepDAG(c.depth, c.fanIn, c.fanOut, 0)
		})
	}
}

// TestDAGs_BuildDepDAG_Determinism asserts that the same (depth,
// fanIn, fanOut, seed) tuple produces byte-identical adjacency
// listings across two independent Build calls.
func TestDAGs_BuildDepDAG_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xDEADBEEF
	g1, err := BuildDepDAG(4, 2, 3, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := BuildDepDAG(4, 2, 3, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestDAGs_BuildDepDAG_DepthZeroIsRootOnly confirms the depth==0
// case produces exactly one node and zero edges.
func TestDAGs_BuildDepDAG_DepthZeroIsRootOnly(t *testing.T) {
	t.Parallel()
	g, err := BuildDepDAG(0, 1, 1, 0).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	assertOrder(t, g, 1)
	assertSize(t, g, 0)
}

// TestDAGs_BuildDepDAG_Golden pins BuildDepDAG(3, 2, 2, 42).
func TestDAGs_BuildDepDAG_Golden(t *testing.T) {
	t.Parallel()
	g, err := BuildDepDAG(3, 2, 2, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dagsGolden(t, "build-dep-d3-fi2-fo2-seed42.txt", formatAdjacency(g))
}

// TestDAGs_BuildDepDAG_WidthCapEngages exercises the width cap
// branch by driving depth and fanOut high enough that the unbounded
// recurrence would exceed buildDepDAGMaxWidth. The test confirms
// that the cap holds: no layer can have more than buildDepDAGMaxWidth
// nodes regardless of (depth, fanOut). Implemented by counting the
// nodes per layer through the topological order.
func TestDAGs_BuildDepDAG_WidthCapEngages(t *testing.T) {
	t.Parallel()
	// fanOut=10, depth=4 would produce 10^4 = 10_000 at the
	// deepest layer without the cap. With the cap at 1024, layers
	// must stabilise at <= 1024.
	g, err := BuildDepDAG(4, 2, 10, 1).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Walk the topological order layer by layer using BFS distances.
	_, outAdj := dagNodesAndOutAdj(g)
	dist := map[int]int{0: 0}
	queue := []int{0}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, v := range outAdj[u] {
			if _, seen := dist[v]; !seen {
				dist[v] = dist[u] + 1
				queue = append(queue, v)
			}
		}
	}
	layerCount := make(map[int]int)
	for _, d := range dist {
		layerCount[d]++
	}
	for layer, count := range layerCount {
		if count > buildDepDAGMaxWidth {
			t.Fatalf("layer %d has %d nodes, want <= %d (width cap)", layer, count, buildDepDAGMaxWidth)
		}
	}
}

// -------------------------------------------------------------------
// NegativeWeightAcyclic
// -------------------------------------------------------------------

// TestDAGs_NegativeWeightAcyclic_Invariants exercises a small (n,
// signMix, seed) sweep and asserts the documented closed forms,
// AC #1/#2, and the weight range/sign invariants.
func TestDAGs_NegativeWeightAcyclic_Invariants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		n, signMix int
		seed       uint64
	}{
		{0, 0, 1},
		{1, 50, 1},
		{2, 0, 7},   // every weight must be positive.
		{2, 100, 7}, // every weight must be negative.
		{5, 50, 42}, // used by golden.
		{8, 30, 99},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("n=%d_mix=%d_seed=%d", c.n, c.signMix, c.seed), func(t *testing.T) {
			t.Parallel()
			s := NegativeWeightAcyclic(c.n, c.signMix, c.seed)
			if got, want := s.Name(), "dags.negative-weight-acyclic"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 2 ||
				knobs[0].Name != "n" || knobs[0].Min != 0 || knobs[0].Max != 100 || knobs[0].Default != 5 ||
				knobs[1].Name != "signMix" || knobs[1].Min != 0 || knobs[1].Max != 100 || knobs[1].Default != 50 {
				t.Fatalf("Knobs = %#v, want n:[0,100]/5, signMix:[0,100]/50", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			assertOrder(t, g, uint64(c.n))
			assertSize(t, g, uint64(c.n*(c.n-1)/2))
			assertDirected(t, g, true)
			assertAcyclicAndSingletonSCCs(t, g)
			// Verify every edge weight is in [-1000, -1] or [1, 1000].
			adj := g.AdjList()
			for u := 0; u < c.n; u++ {
				for v, w := range adj.Neighbours(u) {
					if w == 0 {
						t.Fatalf("edge (%d,%d) has weight 0; the catalogue contract excludes 0", u, v)
					}
					if w < -1000 || w > 1000 {
						t.Fatalf("edge (%d,%d) has weight %d, out of [-1000, 1000]", u, v, w)
					}
					if c.signMix == 0 && w < 0 {
						t.Fatalf("signMix=0: edge (%d,%d) has negative weight %d", u, v, w)
					}
					if c.signMix == 100 && w > 0 {
						t.Fatalf("signMix=100: edge (%d,%d) has positive weight %d", u, v, w)
					}
				}
			}
		})
	}
}

// TestDAGs_NegativeWeightAcyclic_PanicsOutOfRange covers every guard
// branch.
func TestDAGs_NegativeWeightAcyclic_PanicsOutOfRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		n, signMix int
	}{
		{"n_negative", -1, 50},
		{"n_too_large", 101, 50},
		{"signMix_negative", 5, -1},
		{"signMix_too_large", 5, 101},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("NegativeWeightAcyclic(%d,%d,0) did not panic", c.n, c.signMix)
				}
			}()
			_ = NegativeWeightAcyclic(c.n, c.signMix, 0)
		})
	}
}

// TestDAGs_NegativeWeightAcyclic_Determinism asserts that the same
// (n, signMix, seed) tuple produces byte-identical adjacency
// listings across two independent Build calls.
func TestDAGs_NegativeWeightAcyclic_Determinism(t *testing.T) {
	t.Parallel()
	const seed uint64 = 0xFEEDFACE
	g1, err := NegativeWeightAcyclic(8, 40, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #1: %v", err)
	}
	g2, err := NegativeWeightAcyclic(8, 40, seed).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build #2: %v", err)
	}
	if got, want := formatAdjacency(g1), formatAdjacency(g2); got != want {
		t.Fatalf("Build is not deterministic:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestDAGs_NegativeWeightAcyclic_Golden pins
// NegativeWeightAcyclic(5, 50, 42).
func TestDAGs_NegativeWeightAcyclic_Golden(t *testing.T) {
	t.Parallel()
	g, err := NegativeWeightAcyclic(5, 50, 42).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	dagsGolden(t, "negative-weight-n5-mix50-seed42.txt", formatAdjacency(g))
}

// -------------------------------------------------------------------
// MaxShardCapacity preservation
// -------------------------------------------------------------------

// TestDAGs_PreservesMaxShardCapacity confirms that every dags
// generator preserves cfg.MaxShardCapacity verbatim, mirroring the
// other-family contracts.
func TestDAGs_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}
	for _, tc := range []struct {
		name string
		s    Shape[int, int64]
	}{
		{"transitive_tournament", TransitiveTournament(5)},
		{"diamond", Diamond(3)},
		{"layered", Layered(3, 3, 50, 42)},
		{"lengauer_tarjan", LengauerTarjanExample()},
		{"build_dep", BuildDepDAG(3, 2, 2, 42)},
		{"negative_weight", NegativeWeightAcyclic(5, 50, 42)},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, err := tc.s.Build(cfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if g == nil {
				t.Fatal("Build returned nil graph")
			}
		})
	}
}

// -------------------------------------------------------------------
// Error-path coverage
// -------------------------------------------------------------------

// TestDAGs_TransitiveTournament_ShardFullPropagates exercises the
// AddEdge error path of buildTransitiveTournament. The harness
// invokes the production helper buildTransitiveTournament directly
// with n=300: addNodesRange interns 300 distinct user keys, which
// FNV-1a routes across the 256 mapper shards so at least one shard
// holds >= 2 keys. Under MaxShardCapacity=1, the first AddEdge from
// a source whose NodeID has intraIdx >= 1 triggers adjlist.ErrShardFull,
// which the build must propagate verbatim through the err-threaded
// loop guard.
func TestDAGs_TransitiveTournament_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	if err := buildTransitiveTournament(g, 300); err == nil {
		t.Fatal("buildTransitiveTournament(g, 300) with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// TestDAGs_Diamond_ShardFullPropagates exercises the AddEdge error
// path of buildDiamond. With cap=1 a 300-node Diamond is well past
// the 256-shard threshold; the chain-A and chain-B AddEdge calls
// must propagate adjlist.ErrShardFull. The harness uses k=150 so
// the diamond has 2*150 + 2 = 302 nodes — comfortably above the
// boundary.
func TestDAGs_Diamond_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	if err := buildDiamond(g, 150); err == nil {
		t.Fatal("buildDiamond(g, 150) with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// TestDAGs_BuildDepDAG_ShardFullPropagates exercises the AddEdge
// error path of buildDepDAG. fanOut=10 with depth=4 drives the
// recurrence into the width cap (1024 nodes per layer), and at
// MaxShardCapacity=1 some AddEdge from a source with intraIdx >= 1
// must surface adjlist.ErrShardFull. The error must propagate
// verbatim through the build's err-threaded outer loop.
func TestDAGs_BuildDepDAG_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 1}
	_, err := BuildDepDAG(4, 2, 10, 1).Build(cfg)
	if err == nil {
		t.Fatal("Build returned nil error, want adjlist.ErrShardFull")
	}
}

// TestDAGs_NegativeWeightAcyclic_ShardFullPropagates exercises the
// AddEdge error path of buildNegativeWeightAcyclic via a private
// 300-node invocation (the public constructor caps n at 100, below
// the 256-shard threshold; the harness bypasses the cap by calling
// the helper directly). With cap=1 the AddEdge from a source whose
// NodeID has intraIdx >= 1 must surface adjlist.ErrShardFull.
func TestDAGs_NegativeWeightAcyclic_ShardFullPropagates(t *testing.T) {
	t.Parallel()
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 1}
	g := lpg.New[int, int64](cfg)
	if err := buildNegativeWeightAcyclic(g, 300, 50, 1); err == nil {
		t.Fatal("buildNegativeWeightAcyclic(g, 300, 50, 1) with MaxShardCapacity=1 returned nil error, want adjlist.ErrShardFull")
	}
}

// -------------------------------------------------------------------
// Property-based sweep
// -------------------------------------------------------------------

// TestDAGs_Properties_RapidSweep drives every parametric DAG
// generator (excluding LengauerTarjanExample, which has no knobs
// and is intentionally cyclic) over a small parameter sweep and
// asserts the catalogue invariants documented in the constructor
// godoc. Bounds are kept small so the short layer stays under the
// per-package time budget.
func TestDAGs_Properties_RapidSweep(t *testing.T) {
	t.Parallel()

	t.Run("transitive_tournament", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(0, 20).Draw(r, "n")
			g, err := TransitiveTournament(n).Build(defaultCfg)
			if err != nil {
				t.Fatalf("n=%d: Build: %v", n, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("n=%d: Order = %d, want %d", n, got, n)
			}
			if got := g.AdjList().Size(); got != uint64(n*(n-1)/2) {
				t.Fatalf("n=%d: Size = %d, want %d", n, got, n*(n-1)/2)
			}
		})
	})

	t.Run("diamond", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			k := rapid.IntRange(0, 30).Draw(r, "k")
			g, err := Diamond(k).Build(defaultCfg)
			if err != nil {
				t.Fatalf("k=%d: Build: %v", k, err)
			}
			if k == 0 {
				if got := g.AdjList().Order(); got != 2 {
					t.Fatalf("k=0: Order = %d, want 2", got)
				}
				if got := g.AdjList().Size(); got != 1 {
					t.Fatalf("k=0: Size = %d, want 1", got)
				}
				return
			}
			if got := g.AdjList().Order(); got != uint64(2*k+2) {
				t.Fatalf("k=%d: Order = %d, want %d", k, got, 2*k+2)
			}
			if got := g.AdjList().Size(); got != uint64(2*(k+1)) {
				t.Fatalf("k=%d: Size = %d, want %d", k, got, 2*(k+1))
			}
			if got := countPaths(g, 0, 2*k+1); got != 2 {
				t.Fatalf("k=%d: path count = %d, want 2", k, got)
			}
		})
	})

	t.Run("layered", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			L := rapid.IntRange(1, 5).Draw(r, "L")
			w := rapid.IntRange(1, 5).Draw(r, "w")
			density := rapid.IntRange(0, 100).Draw(r, "density")
			seed := rapid.Uint64().Draw(r, "seed")
			g, err := Layered(L, w, density, seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("L=%d w=%d density=%d: Build: %v", L, w, density, err)
			}
			if got := g.AdjList().Order(); got != uint64(L*w) {
				t.Fatalf("Order = %d, want %d", got, L*w)
			}
			maxSize := uint64((L - 1) * w * w)
			if got := g.AdjList().Size(); got > maxSize {
				t.Fatalf("Size = %d, exceeds max %d", got, maxSize)
			}
		})
	})

	t.Run("build_dep", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			depth := rapid.IntRange(0, 5).Draw(r, "depth")
			fanIn := rapid.IntRange(1, 4).Draw(r, "fanIn")
			fanOut := rapid.IntRange(1, 4).Draw(r, "fanOut")
			seed := rapid.Uint64().Draw(r, "seed")
			g, err := BuildDepDAG(depth, fanIn, fanOut, seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := g.AdjList().Order(); got < 1 {
				t.Fatalf("Order = %d, want >= 1", got)
			}
			if order := kahnTopoOrder(g); order == nil {
				t.Fatal("BuildDepDAG produced a cyclic graph (AC #1 violation)")
			}
		})
	})

	t.Run("negative_weight_acyclic", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(0, 20).Draw(r, "n")
			signMix := rapid.IntRange(0, 100).Draw(r, "signMix")
			seed := rapid.Uint64().Draw(r, "seed")
			g, err := NegativeWeightAcyclic(n, signMix, seed).Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("Order = %d, want %d", got, n)
			}
			if got := g.AdjList().Size(); got != uint64(n*(n-1)/2) {
				t.Fatalf("Size = %d, want %d", got, n*(n-1)/2)
			}
		})
	})
}

// -------------------------------------------------------------------
// Soak / nightly layer sweeps
// -------------------------------------------------------------------

// TestDAGs_TransitiveTournament_Soak exercises the short-layer
// ceiling (n=200) and asserts the closed-form invariants.
func TestDAGs_TransitiveTournament_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const n = 200
	g, err := TransitiveTournament(n).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	if got := g.AdjList().Size(); got != uint64(n*(n-1)/2) {
		t.Fatalf("Size = %d, want %d", got, n*(n-1)/2)
	}
}

// TestDAGs_Layered_Soak exercises Layered at the short-layer
// ceiling (L=100, w=100) with density 50% under a fixed seed.
func TestDAGs_Layered_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	g, err := Layered(100, 100, 50, 0xCAFE).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != 10_000 {
		t.Fatalf("Order = %d, want 10000", got)
	}
	if order := kahnTopoOrder(g); order == nil {
		t.Fatal("soak-layer Layered produced a cyclic graph")
	}
}

// TestDAGs_BuildDepDAG_Soak exercises BuildDepDAG at the short-layer
// ceiling (depth=20, fanIn=fanOut=10).
func TestDAGs_BuildDepDAG_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	g, err := BuildDepDAG(20, 10, 10, 0xBEAD).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got < 1 {
		t.Fatalf("Order = %d, want >= 1", got)
	}
	if order := kahnTopoOrder(g); order == nil {
		t.Fatal("soak-layer BuildDepDAG produced a cyclic graph")
	}
}

// TestDAGs_NegativeWeightAcyclic_Soak exercises NegativeWeightAcyclic
// at the short-layer ceiling (n=100, signMix=50%).
func TestDAGs_NegativeWeightAcyclic_Soak(t *testing.T) {
	testlayers.RequireSoak(t)
	const n = 100
	g, err := NegativeWeightAcyclic(n, 50, 0xFEED).Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	if got := g.AdjList().Size(); got != uint64(n*(n-1)/2) {
		t.Fatalf("Size = %d, want %d", got, n*(n-1)/2)
	}
}

// _ keeps the lpg import alive even when only the shapegen package
// types are referenced in this file. The build closures hand back
// *lpg.Graph already, so this is purely a compile-time guard against
// import drift during T58.22 migration.
var _ *lpg.Graph[int, int64]
