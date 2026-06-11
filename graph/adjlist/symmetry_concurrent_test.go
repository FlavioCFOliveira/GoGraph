package adjlist_test

import (
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// handleMultisetFor returns the sorted slice of handles stored in the nb/h
// columns of a LoadEntryH result where the neighbour equals target.
// When h is nil every matched slot contributes a zero handle.
func handleMultisetFor(nb []graph.NodeID, h []uint64, target graph.NodeID) []uint64 {
	var out []uint64
	for i, n := range nb {
		if n == target {
			var hv uint64
			if h != nil {
				hv = h[i]
			}
			out = append(out, hv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// uint64SlicesEqual reports whether a and b are element-wise equal.
func uint64SlicesEqual(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// setDiff returns the first element present in before but absent from after
// (treating slices as sorted multisets). Returns 0 when they are equal.
func setDiff(before, after []uint64) uint64 {
	i, j := 0, 0
	for i < len(before) && j < len(after) {
		if before[i] == after[j] {
			i++
			j++
		} else {
			return before[i]
		}
	}
	if i < len(before) {
		return before[i]
	}
	return 0
}

// TestUndirectedMultigraph_ConcurrentAddEdgeH_SymmetryGate is the regression
// gate for the mirror-slot inversion race described in task #1360.
//
// Without the two-lock canonical-order fix: two concurrent AddEdgeH(u,v,…)
// calls can interleave so u→v accumulates [h1,h2] while v→u accumulates
// [h2,h1].  RemoveEdge then removes different logical instances from each
// direction, breaking symmetry permanently.
//
// The test is designed to fail (or be flakey) before the fix and to pass
// reliably under -race after it.
func TestUndirectedMultigraph_ConcurrentAddEdgeH_SymmetryGate(t *testing.T) {
	t.Parallel()

	const (
		goroutines  = 32
		repetitions = 30
	)

	for rep := 0; rep < repetitions; rep++ {
		rep := rep
		t.Run(fmt.Sprintf("rep%02d", rep), func(t *testing.T) {
			t.Parallel()

			a := adjlist.New[string, int](adjlist.Config{
				Directed:   false,
				Multigraph: true,
			})

			// Pre-intern both endpoints so their NodeIDs are assigned before
			// the concurrent phase; Intern is already race-safe in Mapper.
			if err := a.AddNode("u"); err != nil {
				t.Fatalf("AddNode u: %v", err)
			}
			if err := a.AddNode("v"); err != nil {
				t.Fatalf("AddNode v: %v", err)
			}

			uID, uOK := a.Mapper().Lookup("u")
			vID, vOK := a.Mapper().Lookup("v")
			if !uOK || !vOK {
				t.Fatal("pre-interned nodes not found")
			}

			// Launch goroutines concurrently adding parallel edges u→v.
			var wg sync.WaitGroup
			wg.Add(goroutines)
			for g := 0; g < goroutines; g++ {
				g := g
				go func() {
					defer wg.Done()
					handle := uint64(g + 1) // distinct, non-zero per goroutine
					if err := a.AddEdgeH("u", "v", g, handle); err != nil {
						t.Errorf("goroutine %d AddEdgeH: %v", g, err)
					}
				}()
			}
			wg.Wait()

			// Collect the handle multisets for both directions.
			nbU, _, hU := a.LoadEntryH(uID)
			nbV, _, hV := a.LoadEntryH(vID)

			fwd := handleMultisetFor(nbU, hU, vID)
			mir := handleMultisetFor(nbV, hV, uID)

			if len(fwd) != goroutines {
				t.Errorf("rep %d: forward slot count = %d, want %d", rep, len(fwd), goroutines)
			}
			if len(mir) != goroutines {
				t.Errorf("rep %d: mirror slot count = %d, want %d", rep, len(mir), goroutines)
			}
			if !uint64SlicesEqual(fwd, mir) {
				t.Errorf("rep %d: mirror inversion — forward handles %v != mirror handles %v", rep, fwd, mir)
			}

			// Remove one edge and assert both directions lose the SAME handle.
			a.RemoveEdge("u", "v")

			nbU2, _, hU2 := a.LoadEntryH(uID)
			nbV2, _, hV2 := a.LoadEntryH(vID)

			fwd2 := handleMultisetFor(nbU2, hU2, vID)
			mir2 := handleMultisetFor(nbV2, hV2, uID)

			if len(fwd2) != goroutines-1 {
				t.Errorf("rep %d after RemoveEdge: forward count = %d, want %d", rep, len(fwd2), goroutines-1)
			}
			if len(mir2) != goroutines-1 {
				t.Errorf("rep %d after RemoveEdge: mirror count = %d, want %d", rep, len(mir2), goroutines-1)
			}
			if !uint64SlicesEqual(fwd2, mir2) {
				t.Errorf("rep %d after RemoveEdge: forward %v != mirror %v (asymmetric removal)", rep, fwd2, mir2)
			}

			removedFwd := setDiff(fwd, fwd2)
			removedMir := setDiff(mir, mir2)
			if removedFwd != removedMir {
				t.Errorf("rep %d: forward removed handle %d, mirror removed handle %d — different logical edges removed", rep, removedFwd, removedMir)
			}
		})
	}
}

// TestUndirectedMultigraph_ConcurrentAddEdgeH_CrossShardSymmetry explicitly
// exercises the case where u and v land in DIFFERENT shards — the path that
// was previously unprotected and required the two-lock canonical-order fix.
func TestUndirectedMultigraph_ConcurrentAddEdgeH_CrossShardSymmetry(t *testing.T) {
	t.Parallel()

	const (
		goroutines  = 64
		shardMask   = 0xFF
		repetitions = 5
	)

	// Locate keys whose Mapper-assigned NodeIDs land in different shards.
	// We probe with a temporary AdjList so the probe does not pollute the
	// graph under test.
	probe := adjlist.New[int, struct{}](adjlist.Config{Directed: true})
	var key0, key1 int
	found0, found1 := false, false
	for i := 0; (!found0 || !found1) && i < 10_000_000; i++ {
		id := probe.Mapper().Intern(i)
		s := byte(uint64(id) & shardMask)
		if s == 0 && !found0 {
			key0 = i
			found0 = true
		} else if s == 1 && !found1 {
			key1 = i
			found1 = true
		}
	}
	if !found0 || !found1 {
		t.Fatal("could not find keys in shards 0 and 1 within 10M candidates")
	}

	for rep := 0; rep < repetitions; rep++ {
		rep := rep
		t.Run(fmt.Sprintf("rep%02d", rep), func(t *testing.T) {
			t.Parallel()

			a := adjlist.New[int, int](adjlist.Config{
				Directed:   false,
				Multigraph: true,
			})
			if err := a.AddNode(key0); err != nil {
				t.Fatalf("AddNode key0: %v", err)
			}
			if err := a.AddNode(key1); err != nil {
				t.Fatalf("AddNode key1: %v", err)
			}

			uID, _ := a.Mapper().Lookup(key0)
			vID, _ := a.Mapper().Lookup(key1)

			var wg sync.WaitGroup
			wg.Add(goroutines)
			for g := 0; g < goroutines; g++ {
				g := g
				go func() {
					defer wg.Done()
					if err := a.AddEdgeH(key0, key1, g, uint64(g+1)); err != nil {
						t.Errorf("goroutine %d AddEdgeH: %v", g, err)
					}
				}()
			}
			wg.Wait()

			nbU, _, hU := a.LoadEntryH(uID)
			nbV, _, hV := a.LoadEntryH(vID)

			fwd := handleMultisetFor(nbU, hU, vID)
			mir := handleMultisetFor(nbV, hV, uID)

			if len(fwd) != goroutines {
				t.Errorf("cross-shard rep %d: forward count = %d, want %d", rep, len(fwd), goroutines)
			}
			if len(mir) != goroutines {
				t.Errorf("cross-shard rep %d: mirror count = %d, want %d", rep, len(mir), goroutines)
			}
			if !uint64SlicesEqual(fwd, mir) {
				t.Errorf("cross-shard rep %d: forward %v != mirror %v", rep, fwd, mir)
			}

			// RemoveEdge and verify the same handle is removed from both sides.
			a.RemoveEdge(key0, key1)

			nbU2, _, hU2 := a.LoadEntryH(uID)
			nbV2, _, hV2 := a.LoadEntryH(vID)

			fwd2 := handleMultisetFor(nbU2, hU2, vID)
			mir2 := handleMultisetFor(nbV2, hV2, uID)

			if !uint64SlicesEqual(fwd2, mir2) {
				t.Errorf("cross-shard rep %d after RemoveEdge: forward %v != mirror %v", rep, fwd2, mir2)
			}

			removedFwd := setDiff(fwd, fwd2)
			removedMir := setDiff(mir, mir2)
			if removedFwd != removedMir {
				t.Errorf("cross-shard rep %d: removed fwd handle %d != removed mir handle %d", rep, removedFwd, removedMir)
			}
		})
	}
}
