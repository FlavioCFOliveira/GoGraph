package adjlist

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// scanInNeighbours is the implementation the reverse index replaced: a full
// walk of every interned node, loading each adjacency entry and looking for
// dst. It is kept here, in the test, as the ORACLE — the reverse index is
// correct exactly when it agrees with this on every node of every graph, and
// stating the reference explicitly is what lets the comparison fail loudly
// instead of the index quietly defining its own truth.
func scanInNeighbours[N comparable, W any](a *AdjList[N, W], dstID graph.NodeID) []graph.NodeID {
	var out []graph.NodeID
	a.Mapper().Walk(func(id graph.NodeID, _ N) bool {
		if id == dstID {
			return true
		}
		nbs, _ := a.LoadEntry(id)
		if slices.Contains(nbs, dstID) {
			out = append(out, id)
		}
		return true
	})
	return out
}

// assertReverseAgrees compares the reverse index with the oracle for EVERY
// interned node, and reports how many nodes actually had an in-edge so a run in
// which the comparison was vacuous cannot pass for a run in which it held.
func assertReverseAgrees[N comparable, W any](t *testing.T, a *AdjList[N, W], what string) (withIn int) {
	t.Helper()
	a.Mapper().Walk(func(id graph.NodeID, _ N) bool {
		want := scanInNeighbours(a, id)
		got := a.InNeighbourIDs(id)
		if len(want) > 0 {
			withIn++
		}
		if !slices.Equal(want, got) {
			t.Fatalf("%s: in-neighbours of %d: index gave %v, full scan gave %v", what, id, got, want)
		}
		return true
	})
	return withIn
}

// TestReverseIndexAgreesWithFullScan drives random add/remove sequences through
// every graph configuration and asserts, after each batch, that the reverse
// index reports exactly what a full scan reports — including multiplicity
// collapsing, self-loop exclusion, and the Mapper.Walk ordering.
func TestReverseIndexAgreesWithFullScan(t *testing.T) {
	t.Parallel()
	for _, cfg := range []Config{
		{Directed: true},
		{Directed: true, Multigraph: true},
		{Directed: false},
		{Directed: false, Multigraph: true},
	} {
		name := fmt.Sprintf("directed=%v/multi=%v", cfg.Directed, cfg.Multigraph)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			const nodes = 60
			rng := rand.New(rand.NewSource(int64(len(name)))) //nolint:gosec // G404: math/rand seeded from the test's own parameter — this test asserts a reproducible sequence, which a CSPRNG would destroy.
			a := New[string, float64](cfg)
			for i := 0; i < nodes; i++ {
				if err := a.AddNode(fmt.Sprintf("n%d", i)); err != nil {
					t.Fatalf("AddNode: %v", err)
				}
			}
			var sawIn int
			for round := 0; round < 40; round++ {
				for op := 0; op < 25; op++ {
					s := fmt.Sprintf("n%d", rng.Intn(nodes))
					d := fmt.Sprintf("n%d", rng.Intn(nodes))
					switch rng.Intn(3) {
					case 0, 1:
						if err := a.AddEdge(s, d, 1); err != nil {
							t.Fatalf("AddEdge: %v", err)
						}
					default:
						a.RemoveEdge(s, d)
					}
				}
				sawIn += assertReverseAgrees(t, a, fmt.Sprintf("round %d", round))
			}
			if sawIn == 0 {
				t.Fatal("no node ever had an in-edge: the comparison never had anything to catch")
			}
			// Draining every edge must drain the index with it; a leak here is the
			// failure mode that would leave DETACH DELETE with edges to remove that
			// it can no longer see.
			for i := 0; i < nodes; i++ {
				a.RemoveAllEdgesFrom(fmt.Sprintf("n%d", i))
			}
			assertReverseAgrees(t, a, "after draining every node")
			if got := a.RecordedInEdges(); got != 0 {
				t.Fatalf("after draining every edge, RecordedInEdges = %d, want 0", got)
			}
			if got := a.Size(); got != 0 {
				t.Fatalf("after draining every edge, Size = %d, want 0", got)
			}
		})
	}
}

// TestReverseIndexSelfLoopExcluded pins the one behaviour a reader is most
// likely to get wrong when reimplementing this: a self-loop does not make a
// node its own in-neighbour, because the scan this index replaced skipped the
// destination itself.
func TestReverseIndexSelfLoopExcluded(t *testing.T) {
	t.Parallel()
	a := New[string, float64](Config{Directed: true, Multigraph: true})
	if err := a.AddEdge("a", "a", 1); err != nil {
		t.Fatalf("AddEdge self-loop: %v", err)
	}
	if err := a.AddEdge("b", "a", 1); err != nil {
		t.Fatalf("AddEdge b->a: %v", err)
	}
	id, ok := a.Mapper().Lookup("a")
	if !ok {
		t.Fatal("a not interned")
	}
	got := a.InNeighbourIDs(id)
	bID, _ := a.Mapper().Lookup("b")
	if !slices.Equal(got, []graph.NodeID{bID}) {
		t.Fatalf("in-neighbours of a = %v, want exactly [%d] (the self-loop must not appear)", got, bID)
	}
}

// TestReverseIndexParallelEdgesCollapse asserts that k parallel edges report
// their source ONCE, and that the source disappears only when the LAST of them
// is removed — the multiplicity property that makes DETACH DELETE remove every
// slot without visiting the same source twice.
func TestReverseIndexParallelEdgesCollapse(t *testing.T) {
	t.Parallel()
	a := New[string, float64](Config{Directed: true, Multigraph: true})
	const parallel = 4
	for i := 0; i < parallel; i++ {
		if err := a.AddEdge("s", "d", float64(i)); err != nil {
			t.Fatalf("AddEdge %d: %v", i, err)
		}
	}
	dID, _ := a.Mapper().Lookup("d")
	sID, _ := a.Mapper().Lookup("s")
	if got := a.InNeighbourIDs(dID); !slices.Equal(got, []graph.NodeID{sID}) {
		t.Fatalf("with %d parallel edges, in-neighbours = %v, want [%d] once", parallel, got, sID)
	}
	for i := 0; i < parallel-1; i++ {
		a.RemoveEdge("s", "d")
		if got := a.InNeighbourIDs(dID); !slices.Equal(got, []graph.NodeID{sID}) {
			t.Fatalf("after removing %d of %d parallel edges, in-neighbours = %v, want [%d]",
				i+1, parallel, got, sID)
		}
	}
	a.RemoveEdge("s", "d")
	if got := a.InNeighbourIDs(dID); len(got) != 0 {
		t.Fatalf("after removing the last parallel edge, in-neighbours = %v, want none", got)
	}
	if got := a.RecordedInEdges(); got != 0 {
		t.Fatalf("RecordedInEdges = %d, want 0", got)
	}
}
