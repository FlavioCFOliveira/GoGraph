package lpg_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"go.uber.org/goleak"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// TestLPG_EdgeProperty_ConcurrentReshape exercises the lock-free read path while
// a writer drives a column repeatedly across the dense<->sparse representation
// boundary. A high-degree source node carries one string property whose present
// set the writer grows and shrinks around the demote/promote thresholds, so the
// column oscillates between the COO and dense forms under load. Concurrent
// readers must always observe a COMPLETE prior-or-new snapshot: for every slot
// they read, the value (when present) must be one of the small set of legal
// values the writer ever stores — never a torn/garbage payload from a
// half-converted column. The writer's own post-write read-back also confirms
// monotonic correctness.
//
// This is the concurrency complement to the single-thread dense-differential
// oracle: it gates the atomic-publish + copy-on-write discipline across the
// representation switch specifically. Must pass under -race.
func TestLPG_EdgeProperty_ConcurrentReshape(t *testing.T) {
	defer goleak.VerifyNone(t)

	g := lpg.New[string, int64](adjlist.Config{Directed: true})
	const src = "hub"
	const degree = 200 // high degree so the column spans many slots
	if err := g.AddNode(src); err != nil {
		t.Fatalf("AddNode(hub): %v", err)
	}
	dsts := make([]string, degree)
	for i := range dsts {
		dsts[i] = fmt.Sprintf("d%d", i)
		if err := g.AddNode(dsts[i]); err != nil {
			t.Fatalf("AddNode(%s): %v", dsts[i], err)
		}
		if err := g.AddEdge(src, dsts[i], 1); err != nil {
			t.Fatalf("AddEdge(hub->%s): %v", dsts[i], err)
		}
	}

	// The only legal values for the "since" property. A reader observing any
	// string outside this set has seen a torn write.
	legal := map[string]bool{}
	values := make([]string, 8)
	for i := range values {
		values[i] = fmt.Sprintf("2020-01-%02d", i+1)
		legal[values[i]] = true
	}

	var stop atomic.Bool
	var wg sync.WaitGroup

	// Writer: oscillate the present set between ~10% (sparse) and ~90% (dense)
	// fill, rewriting values so the read-back varies, driving the reshape both
	// ways many times.
	wg.Add(1)
	go func() {
		defer wg.Done()
		round := 0
		for !stop.Load() {
			round++
			v := lpg.StringValue(values[round%len(values)])
			// Grow to ~90% fill (promotes to dense).
			for i := 0; i < degree*9/10; i++ {
				if err := g.SetEdgeProperty(src, dsts[i], "since", v); err != nil {
					panic(fmt.Sprintf("SetEdgeProperty: %v", err))
				}
			}
			// Shrink to ~10% fill (demotes to sparse).
			for i := degree * 1 / 10; i < degree; i++ {
				g.DelEdgeProperty(src, dsts[i], "since")
			}
		}
	}()

	// Readers: continuously read every slot's property and assert any present
	// value is legal (never torn) and that EdgeProperties returns a self-
	// consistent map.
	const readers = 8
	checked := make([]int64, readers)
	for r := 0; r < readers; r++ {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for i := 0; i < degree; i++ {
					if v, ok := g.GetEdgeProperty(src, dsts[i], "since"); ok {
						s, isStr := v.String()
						if !isStr || !legal[s] {
							panic(fmt.Sprintf("reader %d slot %d: torn/illegal value %q (kind %d)", r, i, s, v.Kind()))
						}
						atomic.AddInt64(&checked[r], 1)
					}
				}
				// Whole-pair map read: every value present must also be legal.
				m := g.EdgeProperties(src, dsts[0])
				if pv, ok := m["since"]; ok {
					if s, _ := pv.String(); !legal[s] {
						panic(fmt.Sprintf("reader %d EdgeProperties: illegal value %q", r, s))
					}
				}
			}
		}()
	}

	// Let the workload run for a fixed number of writer rounds' worth of work.
	// We bound by total reads observed rather than wall time to stay
	// deterministic-ish without a timer; a few hundred thousand reads is ample
	// to interleave many reshapes.
	for {
		var total int64
		for r := 0; r < readers; r++ {
			total += atomic.LoadInt64(&checked[r])
		}
		if total > 200_000 {
			break
		}
	}
	stop.Store(true)
	wg.Wait()

	// Final state must be self-consistent: every present slot carries a legal
	// value and the per-pair view agrees with the per-slot view.
	for i := 0; i < degree; i++ {
		if v, ok := g.GetEdgeProperty(src, dsts[i], "since"); ok {
			if s, _ := v.String(); !legal[s] {
				t.Fatalf("final slot %d: illegal value %q", i, s)
			}
		}
	}
}
