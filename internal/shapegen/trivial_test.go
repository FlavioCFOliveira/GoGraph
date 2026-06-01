package shapegen

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// updateGoldens flips the local golden-write mode used by every test
// in this file. It is a temporary, file-local helper that exists
// only until task T58.22 (#526) lands the shared golden helper in
// internal/goldens; at that point this flag and the formatAdjacency
// / assertGolden helpers below must be migrated to the shared
// implementation, and this comment removed.
//
// Usage:
//
//	go test -run TestTrivial ./internal/shapegen/... -shapegen-update
//
// will rewrite every golden under testdata/shapegen/trivial/ to
// match the current Build output. CI must never run with this flag.
var updateGoldens = flag.Bool(
	"shapegen-update",
	false,
	"rewrite shapegen trivial-family goldens with current Build output",
)

// goldenDir is the directory holding the trivial-family adjacency
// listings. The path is rooted at the package directory; go test's
// working directory convention puts the test binary there.
const goldenDir = "testdata/shapegen/trivial"

// formatAdjacency renders g's adjacency in the trivial-family
// golden format:
//
//   - one line per directed edge as "u -> v [w]" sorted lexicographically
//     by the (u, v, w) triple;
//   - one line per isolated node as "iso v" sorted by v;
//   - a trailing newline so the file ends with a newline by POSIX
//     convention.
//
// The format is deliberately keep-it-simple: stable, line-oriented,
// trivially diffable. T58.22 will likely fold it into a richer
// representation; until then this is the contract every golden in
// testdata/shapegen/trivial obeys.
func formatAdjacency(g *lpg.Graph[int, int64]) string {
	type edge struct {
		u, v int
		w    int64
	}
	adj := g.AdjList()
	mapper := adj.Mapper()

	// Collect node values in ascending order.
	maxID := uint64(adj.MaxNodeID())
	nodes := make([]int, 0, maxID)
	for id := uint64(0); id < maxID; id++ {
		v, ok := mapper.Resolve(graph.NodeID(id))
		if !ok {
			continue
		}
		nodes = append(nodes, v)
	}
	sort.Ints(nodes)

	// Collect every edge as (u, v, w), preserving multiplicity in
	// multigraphs (Neighbours iterates over every parallel entry).
	var edges []edge
	hasOutgoing := make(map[int]bool, len(nodes))
	for _, u := range nodes {
		for v, w := range adj.Neighbours(u) {
			edges = append(edges, edge{u: u, v: v, w: w})
			hasOutgoing[u] = true
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		switch {
		case a.u != b.u:
			return a.u < b.u
		case a.v != b.v:
			return a.v < b.v
		default:
			return a.w < b.w
		}
	})

	var sb strings.Builder
	for _, e := range edges {
		fmt.Fprintf(&sb, "%d -> %d [%d]\n", e.u, e.v, e.w)
	}
	// A node is "isolated" in the golden listing when it has no
	// outgoing edge AND no incoming edge from any other node. In a
	// directed graph the trivial family only records outgoing
	// adjacency, so we approximate "isolated" with "absent from the
	// set of source nodes AND absent from the set of destination
	// nodes". This matches the catalogue's intent for IsolatedOnly
	// and EmptyGraph.
	hasIncoming := make(map[int]bool, len(edges))
	for _, e := range edges {
		hasIncoming[e.v] = true
	}
	for _, n := range nodes {
		if hasOutgoing[n] || hasIncoming[n] {
			continue
		}
		fmt.Fprintf(&sb, "iso %d\n", n)
	}
	return sb.String()
}

// assertGolden compares got with the contents of the golden file at
// testdata/shapegen/trivial/<name>. When the -shapegen-update flag
// is set, the helper rewrites the golden instead of comparing and
// flags the test as having been used in write mode (so CI scripts
// that accidentally enable the flag can detect it via a fail-fast
// assertion).
func assertGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join(goldenDir, name)
	if *updateGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("assertGolden: MkdirAll(%q): %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("assertGolden: WriteFile(%q): %v", path, err)
		}
		t.Logf("rewrote golden %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // path is a test-local golden under testdata/, not user input
	if err != nil {
		t.Fatalf("assertGolden: ReadFile(%q): %v (run with -shapegen-update to bootstrap)", path, err)
	}
	if !bytes.Equal([]byte(got), want) {
		t.Fatalf("golden %s mismatch:\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
}

// defaultCfg is the canonical [adjlist.Config] used by every
// trivial-family test that does not need a specific configuration.
// Directed=true matches the catalogue's canonical orientation; the
// trivial-family generators override Directed and Multigraph when
// their topology requires it, so a single defaultCfg suffices.
var defaultCfg = adjlist.Config{Directed: true}

func TestTrivial_EmptyGraph(t *testing.T) {
	t.Parallel()

	s := EmptyGraph()
	if got, want := s.Name(), "trivial.empty"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	if got := s.Knobs(); len(got) != 0 {
		t.Fatalf("Knobs = %#v, want empty", got)
	}
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != 0 {
		t.Fatalf("Order = %d, want 0", got)
	}
	if got := g.AdjList().Size(); got != 0 {
		t.Fatalf("Size = %d, want 0", got)
	}
	if !g.AdjList().Directed() {
		t.Fatal("Directed = false, want true (catalogue invariant)")
	}
	assertGolden(t, "empty.txt", formatAdjacency(g))
}

func TestTrivial_SingleNode(t *testing.T) {
	t.Parallel()

	s := SingleNode()
	if got, want := s.Name(), "trivial.k1"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	if got := s.Knobs(); len(got) != 0 {
		t.Fatalf("Knobs = %#v, want empty", got)
	}
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != 1 {
		t.Fatalf("Order = %d, want 1", got)
	}
	if got := g.AdjList().Size(); got != 0 {
		t.Fatalf("Size = %d, want 0", got)
	}
	// HasEdge(0,0) must be false: SingleNode never inserts a loop.
	if g.AdjList().HasEdge(0, 0) {
		t.Fatal("HasEdge(0,0) = true, want false")
	}
	assertGolden(t, "k1.txt", formatAdjacency(g))
}

// TestTrivial_SingleEdge enumerates the constructor matrix of
// (directed, weighted, selfLoop) instead of relying on rapid to
// shrink booleans. Each row asserts the catalogue invariant for the
// chosen variant plus the documented Name.
func TestTrivial_SingleEdge(t *testing.T) {
	t.Parallel()

	cases := []struct {
		directed, weighted, selfLoop bool
		wantName                     string
		wantOrder                    uint64
		wantSize                     uint64
		wantHas01                    bool
		wantHas10                    bool
		wantHas00                    bool
	}{
		{directed: true, weighted: true, selfLoop: false, wantName: "trivial.k2",
			wantOrder: 2, wantSize: 1, wantHas01: true, wantHas10: false, wantHas00: false},
		{directed: true, weighted: false, selfLoop: false, wantName: "trivial.k2",
			wantOrder: 2, wantSize: 1, wantHas01: true, wantHas10: false, wantHas00: false},
		{directed: false, weighted: true, selfLoop: false, wantName: "trivial.k2",
			wantOrder: 2, wantSize: 1, wantHas01: true, wantHas10: true, wantHas00: false},
		{directed: false, weighted: false, selfLoop: false, wantName: "trivial.k2",
			wantOrder: 2, wantSize: 1, wantHas01: true, wantHas10: true, wantHas00: false},
		{directed: true, weighted: true, selfLoop: true, wantName: "trivial.k1.selfloop",
			wantOrder: 1, wantSize: 1, wantHas01: false, wantHas10: false, wantHas00: true},
		{directed: true, weighted: false, selfLoop: true, wantName: "trivial.k1.selfloop",
			wantOrder: 1, wantSize: 1, wantHas01: false, wantHas10: false, wantHas00: true},
		{directed: false, weighted: true, selfLoop: true, wantName: "trivial.k1.selfloop",
			wantOrder: 1, wantSize: 1, wantHas01: false, wantHas10: false, wantHas00: true},
		{directed: false, weighted: false, selfLoop: true, wantName: "trivial.k1.selfloop",
			wantOrder: 1, wantSize: 1, wantHas01: false, wantHas10: false, wantHas00: true},
	}

	for _, c := range cases {
		c := c
		label := fmt.Sprintf("d=%v_w=%v_l=%v", c.directed, c.weighted, c.selfLoop)
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			s := SingleEdge(c.directed, c.weighted, c.selfLoop)
			if got := s.Name(); got != c.wantName {
				t.Fatalf("Name = %q, want %q", got, c.wantName)
			}
			if got := s.Knobs(); len(got) != 0 {
				t.Fatalf("Knobs = %#v, want empty", got)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := g.AdjList().Order(); got != c.wantOrder {
				t.Fatalf("Order = %d, want %d", got, c.wantOrder)
			}
			if got := g.AdjList().Size(); got != c.wantSize {
				t.Fatalf("Size = %d, want %d", got, c.wantSize)
			}
			if got := g.AdjList().HasEdge(0, 1); got != c.wantHas01 {
				t.Fatalf("HasEdge(0,1) = %v, want %v", got, c.wantHas01)
			}
			if got := g.AdjList().HasEdge(1, 0); got != c.wantHas10 {
				t.Fatalf("HasEdge(1,0) = %v, want %v", got, c.wantHas10)
			}
			if got := g.AdjList().HasEdge(0, 0); got != c.wantHas00 {
				t.Fatalf("HasEdge(0,0) = %v, want %v", got, c.wantHas00)
			}
		})
	}

	// Pin four canonical variants to golden files so changes to the
	// adjacency renderer or to the underlying Build are caught:
	//   * directed weighted (no self loop)
	//   * undirected weighted (no self loop)
	//   * self loop weighted
	//   * unweighted variants are covered by the matrix above but
	//     not pinned to disk to keep the testdata corpus minimal.
	t.Run("golden_directed_weighted", func(t *testing.T) {
		t.Parallel()
		g, err := SingleEdge(true, true, false).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		assertGolden(t, "k2-directed-weighted.txt", formatAdjacency(g))
	})
	t.Run("golden_undirected_weighted", func(t *testing.T) {
		t.Parallel()
		g, err := SingleEdge(false, true, false).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		assertGolden(t, "k2-undirected-weighted.txt", formatAdjacency(g))
	})
	t.Run("golden_self_loop", func(t *testing.T) {
		t.Parallel()
		g, err := SingleEdge(true, false, true).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		assertGolden(t, "k2-self-loop.txt", formatAdjacency(g))
	})
}

func TestTrivial_ParallelDigon(t *testing.T) {
	t.Parallel()

	s := ParallelDigon(3)
	if got, want := s.Name(), "trivial.parallel-digon"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	knobs := s.Knobs()
	if len(knobs) != 1 || knobs[0].Name != "k" || knobs[0].Min != 1 || knobs[0].Max != 1000 || knobs[0].Default != 2 {
		t.Fatalf("Knobs = %#v, want exactly one k:[1,1000] default 2", knobs)
	}
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != 2 {
		t.Fatalf("Order = %d, want 2", got)
	}
	if got := g.AdjList().Size(); got != 3 {
		t.Fatalf("Size = %d, want 3", got)
	}
	if !g.AdjList().HasEdge(0, 1) {
		t.Fatal("HasEdge(0,1) = false, want true")
	}
	if g.AdjList().HasEdge(1, 0) {
		t.Fatal("HasEdge(1,0) = true, want false (directed by definition)")
	}
	if !g.AdjList().Multigraph() {
		t.Fatal("Multigraph = false, want true (catalogue invariant)")
	}

	// Count parallel edges out of node 0 to confirm Size accounting.
	var seen int
	for v := range g.AdjList().Neighbours(0) {
		if v == 1 {
			seen++
		}
	}
	if seen != 3 {
		t.Fatalf("parallel edges 0->1 = %d, want 3", seen)
	}

	assertGolden(t, "parallel-digon-k3.txt", formatAdjacency(g))
}

// TestTrivial_ParallelDigon_PanicsOnZero locks in the panic contract
// for k < 1; this also gives 100% coverage of the constructor guard.
func TestTrivial_ParallelDigon_PanicsOnZero(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("ParallelDigon(0) did not panic")
		}
	}()
	_ = ParallelDigon(0)
}

func TestTrivial_IsolatedOnly(t *testing.T) {
	t.Parallel()

	s := IsolatedOnly(5)
	if got, want := s.Name(), "trivial.isolated"; got != want {
		t.Fatalf("Name = %q, want %q", got, want)
	}
	knobs := s.Knobs()
	if len(knobs) != 1 || knobs[0].Name != "n" || knobs[0].Min != 0 || knobs[0].Max != 1000 || knobs[0].Default != 5 {
		t.Fatalf("Knobs = %#v, want exactly one n:[0,1000] default 5", knobs)
	}
	g, err := s.Build(defaultCfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := g.AdjList().Order(); got != 5 {
		t.Fatalf("Order = %d, want 5", got)
	}
	if got := g.AdjList().Size(); got != 0 {
		t.Fatalf("Size = %d, want 0", got)
	}
	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			if g.AdjList().HasEdge(i, j) {
				t.Fatalf("HasEdge(%d,%d) = true, want false (no edges allowed)", i, j)
			}
		}
	}
	assertGolden(t, "isolated-n5.txt", formatAdjacency(g))
}

// TestTrivial_IsolatedOnly_PanicsOnNegative covers the negative-n
// constructor guard.
func TestTrivial_IsolatedOnly_PanicsOnNegative(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("IsolatedOnly(-1) did not panic")
		}
	}()
	_ = IsolatedOnly(-1)
}

func TestTrivial_UniversalSelfLoops(t *testing.T) {
	t.Parallel()

	cases := []struct {
		weighted bool
		want     int64
	}{
		{weighted: false, want: unweightedSentinel},
		{weighted: true, want: weightedSentinel},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("weighted=%v", c.weighted), func(t *testing.T) {
			t.Parallel()
			s := UniversalSelfLoops(4, c.weighted)
			if got, want := s.Name(), "trivial.self-loop-universe"; got != want {
				t.Fatalf("Name = %q, want %q", got, want)
			}
			knobs := s.Knobs()
			if len(knobs) != 1 || knobs[0].Name != "n" || knobs[0].Min != 0 || knobs[0].Max != 1000 || knobs[0].Default != 4 {
				t.Fatalf("Knobs = %#v, want exactly one n:[0,1000] default 4", knobs)
			}
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if got := g.AdjList().Order(); got != 4 {
				t.Fatalf("Order = %d, want 4", got)
			}
			if got := g.AdjList().Size(); got != 4 {
				t.Fatalf("Size = %d, want 4", got)
			}
			for v := 0; v < 4; v++ {
				if !g.AdjList().HasEdge(v, v) {
					t.Fatalf("HasEdge(%d,%d) = false, want true", v, v)
				}
				// Confirm the weight matches the chosen variant.
				for nbr, w := range g.AdjList().Neighbours(v) {
					if nbr == v && w != c.want {
						t.Fatalf("self-loop weight at v=%d = %d, want %d", v, w, c.want)
					}
				}
			}
			// Cross-pair edges must be absent.
			for i := 0; i < 4; i++ {
				for j := 0; j < 4; j++ {
					if i == j {
						continue
					}
					if g.AdjList().HasEdge(i, j) {
						t.Fatalf("HasEdge(%d,%d) = true, want false", i, j)
					}
				}
			}
		})
	}

	t.Run("golden_n4_weighted", func(t *testing.T) {
		t.Parallel()
		g, err := UniversalSelfLoops(4, true).Build(defaultCfg)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		assertGolden(t, "self-loop-universe-n4-weighted.txt", formatAdjacency(g))
	})
}

// TestTrivial_UniversalSelfLoops_PanicsOnNegative covers the
// negative-n constructor guard.
func TestTrivial_UniversalSelfLoops_PanicsOnNegative(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("UniversalSelfLoops(-1, false) did not panic")
		}
	}()
	_ = UniversalSelfLoops(-1, false)
}

// TestTrivial_Properties_RapidSweep exercises the knob-aware
// generators (ParallelDigon, IsolatedOnly, UniversalSelfLoops) over
// their declared bounded sweeps and asserts the catalogue invariants
// for every draw. The constructor matrix of SingleEdge is enumerated
// independently by TestTrivial_SingleEdge above because rapid is a
// worse fit for shrinking eight discrete configurations than a
// straight table.
func TestTrivial_Properties_RapidSweep(t *testing.T) {
	t.Parallel()

	t.Run("parallel_digon", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			k := rapid.IntRange(1, 1000).Draw(r, "k")
			s := ParallelDigon(k)
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("ParallelDigon(%d).Build: %v", k, err)
			}
			if got := g.AdjList().Order(); got != 2 {
				t.Fatalf("k=%d: Order = %d, want 2", k, got)
			}
			if got := g.AdjList().Size(); got != uint64(k) {
				t.Fatalf("k=%d: Size = %d, want %d", k, got, k)
			}
			if !g.AdjList().HasEdge(0, 1) {
				t.Fatalf("k=%d: HasEdge(0,1) = false, want true", k)
			}
			if g.AdjList().HasEdge(1, 0) {
				t.Fatalf("k=%d: HasEdge(1,0) = true, want false", k)
			}
		})
	})

	t.Run("isolated_only", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(0, 1000).Draw(r, "n")
			s := IsolatedOnly(n)
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("IsolatedOnly(%d).Build: %v", n, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("n=%d: Order = %d, want %d", n, got, n)
			}
			if got := g.AdjList().Size(); got != 0 {
				t.Fatalf("n=%d: Size = %d, want 0", n, got)
			}
		})
	})

	t.Run("self_loop_universe", func(t *testing.T) {
		t.Parallel()
		rapid.Check(t, func(r *rapid.T) {
			n := rapid.IntRange(0, 1000).Draw(r, "n")
			weighted := rapid.Bool().Draw(r, "weighted")
			s := UniversalSelfLoops(n, weighted)
			g, err := s.Build(defaultCfg)
			if err != nil {
				t.Fatalf("UniversalSelfLoops(%d,%v).Build: %v", n, weighted, err)
			}
			if got := g.AdjList().Order(); got != uint64(n) {
				t.Fatalf("n=%d: Order = %d, want %d", n, got, n)
			}
			if got := g.AdjList().Size(); got != uint64(n) {
				t.Fatalf("n=%d: Size = %d, want %d", n, got, n)
			}
			for v := 0; v < n; v++ {
				if !g.AdjList().HasEdge(v, v) {
					t.Fatalf("n=%d: HasEdge(%d,%d) = false, want true", n, v, v)
				}
			}
		})
	})
}

// TestTrivial_PreservesMaxShardCapacity asserts that the topology
// overrides applied by every generator in this file do not clobber
// the caller-supplied cfg.MaxShardCapacity. A non-zero cap is
// observable through the underlying adjlist: AddEdge into a saturated
// shard returns adjlist.ErrShardFull. We probe this by giving each
// generator a cap that is large enough to admit its own construction
// but small enough that we can then add one more node and observe
// the error.
//
// The test exists primarily as documentation of the override policy.
// It also exercises EmptyGraph and SingleNode in cfg-preserving
// paths that the other tests do not touch.
func TestTrivial_PreservesMaxShardCapacity(t *testing.T) {
	t.Parallel()

	// A generous cap that every generator in this file fits under.
	cfg := adjlist.Config{Directed: true, MaxShardCapacity: 16}

	for _, tc := range []struct {
		name string
		s    Shape[int, int64]
	}{
		{"empty", EmptyGraph()},
		{"k1", SingleNode()},
		{"k2", SingleEdge(true, true, false)},
		{"k1.selfloop", SingleEdge(true, true, true)},
		{"parallel-digon", ParallelDigon(2)},
		{"isolated", IsolatedOnly(3)},
		{"self-loop-universe", UniversalSelfLoops(3, true)},
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
