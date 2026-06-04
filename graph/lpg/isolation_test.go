package lpg

import (
	"runtime"
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

// TestIsolation_DirectReadObservesPartialTransaction characterises the
// documented OPT-IN nature of the visibility barrier (#1283): the
// no-partial-transaction guarantee holds ONLY for reads routed through
// [Graph.View]. A direct public read (here g.AdjList().HasEdge and
// g.HasNodeLabel called WITHOUT View) takes only its own shard locks, not
// visMu, so it can observe a multi-op transaction half-applied — the edge of
// an edge-plus-labels write before the endpoint labels exist.
//
// This is a CONTRACT/characterization test, not a bug fix: it locks the
// currently-documented behaviour. It proves two halves of the same coin under
// a deterministic handshake (no flaky timing):
//
//   - a reader reading DIRECTLY mid-transaction observes violation > 0
//     (the opt-in hole is real and documented), while
//   - the same reader, wrapped in [Graph.View], observes ZERO violations
//     (View closes the window).
//
// The writer pins the partial state open across a barrier so the direct read
// is guaranteed to land inside the transaction; the reader never requests
// visMu (only shard locks), so the handshake cannot deadlock against the
// writer that holds visMu via [Graph.ApplyAtomically]. Run under -race: the
// per-shard locks make every access data-race-free; the gap proven OPEN here
// is the logical partial-transaction visibility, not a memory race.
func TestIsolation_DirectReadObservesPartialTransaction(t *testing.T) {
	t.Parallel()

	g := New[string, int64](adjlist.Config{Directed: true})
	if err := g.AddNode("u"); err != nil {
		t.Fatalf("AddNode u: %v", err)
	}
	if err := g.AddNode("v"); err != nil {
		t.Fatalf("AddNode v: %v", err)
	}

	// readEdgeImpliesLabels evaluates the cross-substructure invariant
	// "HasEdge(u,v) ⇔ HasNodeLabel(u,Hot) ⇔ HasNodeLabel(v,Hot)" with three
	// direct public reads and reports whether it is currently violated.
	readEdgeImpliesLabels := func() bool {
		e := g.AdjList().HasEdge("u", "v")
		lu := g.HasNodeLabel("u", "Hot")
		lv := g.HasNodeLabel("v", "Hot")
		return e != lu || e != lv
	}

	// Half 1 — direct read, NO View. A writer opens a transaction, adds the
	// edge, then blocks BEFORE setting the labels until the reader has read.
	// The reader, reading directly, must observe {edge present, labels absent}
	// — a half-applied transaction — proving the opt-in hole.
	var directViolation atomic.Int64
	{
		readNow := make(chan struct{})  // writer -> reader: edge added, labels not yet
		readDone := make(chan struct{}) // reader -> writer: read taken, finish the txn
		writeDone := make(chan struct{})

		go func() {
			defer close(writeDone)
			_ = g.ApplyAtomically(func() error {
				_ = g.AddEdge("u", "v", 0)
				close(readNow) // partial state is now established
				<-readDone     // hold the transaction open across the direct read
				_ = g.SetNodeLabel("u", "Hot")
				_ = g.SetNodeLabel("v", "Hot")
				return nil
			})
		}()

		<-readNow
		if readEdgeImpliesLabels() {
			directViolation.Add(1)
		}
		close(readDone)
		<-writeDone
	}

	if directViolation.Load() == 0 {
		t.Fatalf("direct (no-View) read did not observe the documented partial-transaction hole; " +
			"expected violation > 0")
	}

	// Reset to the clean, fully-applied state for half 2.
	if err := g.ApplyAtomically(func() error {
		g.AdjList().RemoveEdge("u", "v")
		g.RemoveNodeLabel("u", "Hot")
		g.RemoveNodeLabel("v", "Hot")
		return nil
	}); err != nil {
		t.Fatalf("reset: %v", err)
	}

	// Half 2 — the SAME read wrapped in View. The View read blocks until the
	// whole transaction is visible (the writer holds visMu for the entire
	// apply), so it can only ever observe the fully-applied state: zero
	// violations. We synchronise the writer's start with the reader's attempt
	// so the View call genuinely contends with a multi-op apply.
	var viewViolation atomic.Int64
	var viewReads atomic.Int64
	{
		var wg sync.WaitGroup
		startWrite := make(chan struct{})

		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startWrite
			_ = g.ApplyAtomically(func() error {
				_ = g.AddEdge("u", "v", 0)
				runtime.Gosched() // widen the partial window the View must mask
				_ = g.SetNodeLabel("u", "Hot")
				_ = g.SetNodeLabel("v", "Hot")
				return nil
			})
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			close(startWrite)
			// Read repeatedly through View while the writer applies. Every
			// pinned read must see the invariant hold.
			for i := 0; i < 2000; i++ {
				g.View(func() {
					viewReads.Add(1)
					if readEdgeImpliesLabels() {
						viewViolation.Add(1)
					}
				})
			}
		}()

		wg.Wait()
	}

	if v := viewViolation.Load(); v != 0 {
		t.Fatalf("View-wrapped read observed %d partial-transaction violations; View must close the window", v)
	}
	if viewReads.Load() == 0 {
		t.Fatal("View readers never read; half 2 did not exercise the invariant")
	}

	t.Logf("characterized opt-in barrier: direct reads observed %d violation(s); "+
		"View reads observed 0 across %d reads",
		directViolation.Load(), viewReads.Load())
}
