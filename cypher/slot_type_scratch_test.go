package cypher

import (
	"reflect"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// slot_type_scratch_test.go — rmp #2889: the positional inference's per-source
// counters live in the sweep's reusable scratch, are built only when a source
// reaches that inference, and are emptied before the next source. These tests pin
// that the reuse changes no answer.

// resolvedTypes is every visited slot of a resolution, keyed by position, with
// the aliased types copied out.
func resolvedTypes(visit func(func(pos uint64, types []string))) map[uint64][]string {
	out := make(map[uint64][]string)
	visit(func(pos uint64, types []string) { out[pos] = slices.Clone(types) })
	return out
}

// sourceRun returns src's run of fwd and the handle column sliced to it exactly as
// [forEachResolvedSlotType] slices it.
func sourceRun(fwd *csr.CSR[float64], src uint64) (edges []graph.NodeID, hs []uint64, start uint64) {
	verts, all, handles := fwd.VerticesSlice(), fwd.EdgesSlice(), fwd.HandlesSlice()
	start, end := verts[src], verts[src+1]
	switch {
	case uint64(len(handles)) >= end:
		hs = handles[start:end]
	case uint64(len(handles)) > start:
		hs = handles[start:]
	}
	return all[start:end], hs, start
}

// freshScratchPerSource resolves fwd exactly as [forEachResolvedSlotType] does,
// except that every source gets a NEW scratch — the state the per-source maps had
// when they were allocated per source.
func freshScratchPerSource(g *lpg.ReadView[string, float64], fwd *csr.CSR[float64]) map[uint64][]string {
	return resolvedTypes(func(visit func(uint64, []string)) {
		mapper := g.AdjList().Mapper()
		for src := uint64(0); src < uint64(fwd.MaxNodeID()); src++ {
			edges, hs, start := sourceRun(fwd, src)
			resolveSourceSlotTypes(g, mapper, newSlotTypeScratch(), graph.NodeID(src), edges, hs, start, visit)
		}
	})
}

// staleParallelGraph builds, through the Go API, a multigraph whose sources each
// carry four parallel arcs to one destination typed PER CREATE INSTANCE (the
// legacy pre-handle store), takes a forward CSR of it, then removes one of those
// arcs from the live adjacency. The CSR slot left over has no by-handle type and
// no column-typed slot left to match, so it reaches the POSITIONAL inference,
// which types each by its CREATE-instance ordinal — the value dstSeen supplies —
// on several sources, which is what exercises the reset between them.
func staleParallelGraph(t *testing.T) (*lpg.ReadView[string, float64], *csr.CSR[float64]) {
	t.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	sources := []string{"a", "x", "y"}
	for _, k := range append([]string{"b", "c"}, sources...) {
		if err := g.AddNode(k); err != nil {
			t.Fatalf("AddNode(%q): %v", k, err)
		}
	}
	for _, s := range sources {
		for i, typ := range []string{"K", "M", "K", "M"} {
			if err := g.AddEdge(s, "b", 1); err != nil {
				t.Fatalf("AddEdge(%s, b): %v", s, err)
			}
			g.IncEdgeCreateCount(s, "b")
			if err := g.SetEdgeLabelAt(s, "b", int64(i+1), typ); err != nil {
				t.Fatal(err)
			}
		}
		if err := g.AddEdge(s, "c", 1); err != nil {
			t.Fatalf("AddEdge(%s, c): %v", s, err)
		}
		if err := g.SetEdgeLabel(s, "c", "K"); err != nil {
			t.Fatal(err)
		}
	}
	fwd, _ := csrPairFromGraph(g.ReadAt(nil))
	for _, s := range sources {
		must(t).E(g.RemoveEdge(s, "b"))
	}
	return g.ReadAt(nil), fwd
}

// TestResolveSlotTypes_SweepMatchesFreshScratchPerSource is the differential: a
// sweep that reuses one scratch across every source resolves exactly what a sweep
// with a fresh scratch per source resolves, on every relationship-type fixture and
// on a stale CSR that drives the positional inference on several sources.
func TestResolveSlotTypes_SweepMatchesFreshScratchPerSource(t *testing.T) {
	type fixture struct {
		name string
		make func(t *testing.T) (*lpg.ReadView[string, float64], *csr.CSR[float64])
	}
	var fixtures []fixture
	for _, f := range relTypeFixtures() {
		fixtures = append(fixtures, fixture{f.name, func(t *testing.T) (*lpg.ReadView[string, float64], *csr.CSR[float64]) {
			view := f.build(t).ReadAt(nil)
			fwd, _ := csrPairFromGraph(view)
			return view, fwd
		}})
	}
	fixtures = append(fixtures, fixture{"stale_csr_positional_on_several_sources", staleParallelGraph})
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			view, fwd := f.make(t)
			if len(fwd.EdgesSlice()) == 0 {
				t.Fatal("fixture built no arcs, so this comparison is vacuous")
			}
			got := resolvedTypes(func(visit func(uint64, []string)) { forEachResolvedSlotType(view, fwd, visit) })
			if want := freshScratchPerSource(view, fwd); !reflect.DeepEqual(got, want) {
				t.Fatalf("reused scratch resolved\n  %v\nfresh scratch per source resolved\n  %v", got, want)
			}
		})
	}
}

// TestResolveSlotTypes_PositionalCountersAreReset asserts that the stale fixture
// really reaches the positional inference on more than one source — so the
// differential above is not vacuous — and that the shared counters are empty
// after every source.
func TestResolveSlotTypes_PositionalCountersAreReset(t *testing.T) {
	view, fwd := staleParallelGraph(t)
	mapper := view.AdjList().Mapper()
	sc := newSlotTypeScratch()
	reached := 0
	for src := uint64(0); src < uint64(fwd.MaxNodeID()); src++ {
		edges, hs, start := sourceRun(fwd, src)
		if len(edges) == 0 {
			continue
		}
		resolveSourceSlotTypes(view, mapper, sc, graph.NodeID(src), edges, hs, start, func(uint64, []string) {})
		if len(sc.dstParallelTotal) != 0 || len(sc.dstSeen) != 0 {
			t.Fatalf("source %d left %d/%d positional counters behind", src, len(sc.dstParallelTotal), len(sc.dstSeen))
		}
		if probeReachesPositional(view, mapper, graph.NodeID(src), edges, hs) {
			reached++
		}
	}
	// The inference must also have DECIDED something with the counters, or the
	// differential could not see them. RemoveEdge drops one of the four parallel
	// arcs, so each source's fourth CSR slot to b is the one left without a
	// column-typed slot, and the positional rule types it by CREATE instance
	// dstSeen[b] = 4, which is :M. A counter that started at the inference rather
	// than at the start of the run would read instance 1, which is :K.
	b, _ := mapper.Lookup("b")
	var toB [][]string
	forEachResolvedSlotType(view, fwd, func(pos uint64, types []string) {
		if fwd.EdgesSlice()[pos] == b {
			toB = append(toB, slices.Clone(types))
		}
	})
	if want := [][]string{{"M"}, {"M"}, {"M"}}; !reflect.DeepEqual(toB, want) {
		t.Fatalf("slots to b resolved %v, want %v", toB, want)
	}
	if reached < 2 {
		t.Fatalf("the positional inference was reached on %d sources, want at least 2", reached)
	}
}

// probeReachesPositional reports whether resolving src's run on a fresh scratch
// builds the positional counters.
func probeReachesPositional(
	view *lpg.ReadView[string, float64], mapper *graph.Mapper[string], src graph.NodeID, edges []graph.NodeID, hs []uint64,
) bool {
	sc := newSlotTypeScratch()
	resolveSourceSlotTypes(view, mapper, sc, src, edges, hs, 0, func(uint64, []string) {})
	return sc.dstParallelTotal != nil
}

// TestStartPositionalCounts_MatchesEagerCount pins the lazy start: at every
// position a run may first reach the inference, the counters equal what counting
// every slot from the start of the run gives.
func TestStartPositionalCounts_MatchesEagerCount(t *testing.T) {
	m := graph.NewMapper[string]()
	ids := make([]graph.NodeID, 0, 3)
	for _, k := range []string{"b", "c", "d"} {
		ids = append(ids, m.Intern(k))
	}
	b, c, d := ids[0], ids[1], ids[2]
	run := []graph.NodeID{b, b, c, b, d, c, b}
	for pos := range run {
		sc := newSlotTypeScratch()
		sc.startPositionalCounts(m, run, uint64(pos))
		total, seen := map[graph.NodeID]int64{}, map[graph.NodeID]int64{}
		for i, x := range run {
			total[x]++
			if i <= pos {
				seen[x]++
			}
		}
		if !reflect.DeepEqual(sc.dstParallelTotal, total) || !reflect.DeepEqual(sc.dstSeen, seen) {
			t.Fatalf("pos %d: got total %v seen %v, want %v %v", pos, sc.dstParallelTotal, sc.dstSeen, total, seen)
		}
		sc.endPositionalCounts(run)
		if len(sc.dstParallelTotal) != 0 || len(sc.dstSeen) != 0 {
			t.Fatalf("pos %d: end left %v %v", pos, sc.dstParallelTotal, sc.dstSeen)
		}
	}
}
