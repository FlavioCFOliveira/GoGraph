package lpg

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// TestIsolation_CrossSubstructure_EdgeImpliesLabels proves the barrier flips a
// transaction's writes across DIFFERENT substructures (adjacency + node labels)
// atomically. Each transaction toggles between two consistent states —
// {edge u→v present, u:Hot, v:Hot} and {no edge, no labels} — so the invariant
// "HasEdge(u,v) ⇔ HasNodeLabel(u,Hot) ⇔ HasNodeLabel(v,Hot)" must hold on every
// pinned read. A reader observing the edge without a label (or vice versa)
// would have seen a partial transaction across substructures. Run under -race.
func TestIsolation_CrossSubstructure_EdgeImpliesLabels(t *testing.T) {
	t.Parallel()

	g := New[string, int64](adjlist.Config{Directed: true})
	// Intern u, v up front so the toggling only adds/removes the edge + labels.
	if err := g.AddNode("u"); err != nil {
		t.Fatalf("AddNode u: %v", err)
	}
	if err := g.AddNode("v"); err != nil {
		t.Fatalf("AddNode v: %v", err)
	}

	const (
		toggles = 40000
		readers = 8
	)
	var (
		wg        sync.WaitGroup
		done      atomic.Bool
		violation atomic.Int64
		reads     atomic.Int64
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		present := false
		for i := 0; i < toggles; i++ {
			want := !present
			_ = g.ApplyAtomically(func() error {
				if want {
					_ = g.AddEdge("u", "v", 0)
					_ = g.SetNodeLabel("u", "Hot")
					_ = g.SetNodeLabel("v", "Hot")
				} else {
					g.AdjList().RemoveEdge("u", "v")
					g.RemoveNodeLabel("u", "Hot")
					g.RemoveNodeLabel("v", "Hot")
				}
				return nil
			})
			present = want
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				g.View(func() {
					e := g.AdjList().HasEdge("u", "v")
					lu := g.HasNodeLabel("u", "Hot")
					lv := g.HasNodeLabel("v", "Hot")
					reads.Add(1)
					if e != lu || e != lv {
						violation.Add(1)
					}
				})
			}
		}()
	}

	wg.Wait()
	if v := violation.Load(); v != 0 {
		t.Fatalf("observed %d cross-substructure violations (edge/label disagreement inside a pinned View)", v)
	}
	if reads.Load() == 0 {
		t.Fatal("readers never read; test did not exercise the invariant")
	}
}

// TestIsolation_ApplyAtomically_View_NoPartialReads stress-tests the F3
// transaction-visibility barrier (docs/isolation-design.md) directly on the
// lpg mechanism, with no WAL/I/O so it can run many iterations.
//
// A writer repeatedly sets node "a".v and node "b".v to the SAME value
// inside one ApplyAtomically call. Readers inside View read both and assert
// equality. The barrier guarantees a reader observes either none or all of a
// transaction's writes, so a.v == b.v must hold on every pinned read; a
// partial transaction (new a.v, old b.v) would trip the counter. Run under
// -race (the per-shard locks already prevent data races, so the gap proven
// closed here is the logical partial-transaction visibility).
func TestIsolation_ApplyAtomically_View_NoPartialReads(t *testing.T) {
	t.Parallel()

	g := New[string, int64](adjlist.Config{Directed: true})

	// Seed both nodes.
	if err := g.ApplyAtomically(func() error {
		if err := g.SetNodeProperty("a", "v", Int64Value(0)); err != nil {
			return err
		}
		return g.SetNodeProperty("b", "v", Int64Value(0))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const (
		iterations = 50000
		readers    = 8
	)
	var (
		wg        sync.WaitGroup
		done      atomic.Bool
		violation atomic.Int64
		reads     atomic.Int64
		writeErr  atomic.Int64
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer done.Store(true)
		for i := int64(1); i <= iterations; i++ {
			if err := g.ApplyAtomically(func() error {
				if err := g.SetNodeProperty("a", "v", Int64Value(i)); err != nil {
					return err
				}
				return g.SetNodeProperty("b", "v", Int64Value(i))
			}); err != nil {
				writeErr.Add(1)
				return
			}
		}
	}()

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !done.Load() {
				g.View(func() {
					va, oka := g.GetNodeProperty("a", "v")
					vb, okb := g.GetNodeProperty("b", "v")
					if !oka || !okb {
						return
					}
					ia, _ := va.Int64()
					ib, _ := vb.Int64()
					reads.Add(1)
					if ia != ib {
						violation.Add(1)
					}
				})
			}
		}()
	}

	wg.Wait()
	if writeErr.Load() != 0 {
		t.Fatalf("writer hit %d errors", writeErr.Load())
	}
	if v := violation.Load(); v != 0 {
		t.Fatalf("observed %d partial-transaction violations (a.v != b.v inside a pinned View)", v)
	}
	if reads.Load() == 0 {
		t.Fatal("readers never observed both properties; test did not exercise the invariant")
	}
}
