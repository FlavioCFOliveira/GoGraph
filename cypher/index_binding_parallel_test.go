package cypher

// index_binding_parallel_test.go — #1723 gate: the CREATE INDEX hash backfill
// partitions its lock-free phase-2 across a bounded worker pool above
// backfillParallelMinNodes. These tests pin that the parallel path produces an
// index identical to the serial one (every live node indexed exactly once, no
// extras) and that it honours context cancellation. Run under -race, they also
// certify the concurrent graph reads + concurrent hash.Index inserts are
// data-race-free.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
)

// seedLabeledNamed builds a graph of n nodes, each tagged label and carrying a
// distinct string "name" property, and returns it together with the set of
// names seeded. Keys are unique ("k0".."k{n-1}").
func seedLabeledNamed(tb testing.TB, n int, label string) (*lpg.Graph[string, float64], map[string]struct{}) {
	tb.Helper()
	g := lpg.New[string, float64](adjlist.Config{})
	names := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k%d", i)
		name := fmt.Sprintf("name-%d", i)
		if err := g.SetNodeLabel(key, label); err != nil {
			tb.Fatalf("SetNodeLabel: %v", err)
		}
		if err := g.SetNodeProperty(key, "name", lpg.StringValue(name)); err != nil {
			tb.Fatalf("SetNodeProperty: %v", err)
		}
		names[name] = struct{}{}
	}
	return g, names
}

// TestBackfillNodeHashIndex_ParallelContentsIdentical drives the parallel
// phase-2 (n > backfillParallelMinNodes) and asserts the produced index is
// exactly the seeded ground truth: every name present with cardinality 1, the
// total indexed pair count equal to n, and an unseeded name absent.
func TestBackfillNodeHashIndex_ParallelContentsIdentical(t *testing.T) {
	t.Parallel()
	const n = backfillParallelMinNodes*2 + 123 // strictly above the parallel floor
	g, names := seedLabeledNamed(t, n, "Person")
	e := NewEngine(g)

	idx, err := newBoundNodeHashIndex(e.g, "Person", "name")
	if err != nil {
		t.Fatalf("newBoundNodeHashIndex: %v", err)
	}
	if berr := e.backfillNodeHashIndex(context.Background(), idx, "Person", "name"); berr != nil {
		t.Fatalf("backfill: %v", berr)
	}

	var total uint64
	for name := range names {
		c := idx.Lookup(name).GetCardinality()
		if c != 1 {
			t.Fatalf("Lookup(%q) cardinality = %d, want 1", name, c)
		}
		total += c
	}
	if total != uint64(n) {
		t.Fatalf("total indexed pairs = %d, want %d", total, n)
	}
	if c := idx.Lookup("name-does-not-exist").GetCardinality(); c != 0 {
		t.Fatalf("unseeded name cardinality = %d, want 0", c)
	}
}

// TestBackfillNodeHashIndex_SerialVsParallelIdentical drives the SAME
// above-threshold graph through both the parallel backfill (the default) and the
// forced-serial backfill (EngineOptions.DisableParallelBackfill ⇒
// parallelBackfillEnabled=false) and asserts the two indexes are content-identical
// for every seeded name. This is the engine half of the parallel-differential DST
// dimension (#1747): a serial/parallel mismatch is sharper than -race alone (a
// race-clean run can still drop a node under a hot-shard partition).
func TestBackfillNodeHashIndex_SerialVsParallelIdentical(t *testing.T) {
	t.Parallel()
	const n = backfillParallelMinNodes*3 + 77 // well above the parallel floor

	build := func(parallel bool) map[string]uint64 {
		g, names := seedLabeledNamed(t, n, "Person")
		e := NewEngine(g)
		e.parallelBackfillEnabled = parallel // white-box toggle of the gate
		idx, err := newBoundNodeHashIndex(e.g, "Person", "name")
		if err != nil {
			t.Fatalf("newBoundNodeHashIndex: %v", err)
		}
		if berr := e.backfillNodeHashIndex(context.Background(), idx, "Person", "name"); berr != nil {
			t.Fatalf("backfill (parallel=%v): %v", parallel, berr)
		}
		card := make(map[string]uint64, len(names))
		for name := range names {
			card[name] = idx.Lookup(name).GetCardinality()
		}
		return card
	}

	par := build(true)
	ser := build(false)
	if len(par) != len(ser) {
		t.Fatalf("name-set size differs: parallel=%d serial=%d", len(par), len(ser))
	}
	for name, pc := range par {
		if sc := ser[name]; sc != pc {
			t.Fatalf("cardinality mismatch for %q: parallel=%d serial=%d", name, pc, sc)
		}
		if pc != 1 {
			t.Fatalf("Lookup(%q) cardinality = %d, want 1", name, pc)
		}
	}
}

// TestBackfillNodeHashIndex_ContextCancelled verifies the parallel backfill
// honours an already-cancelled context: it stops and returns the context error
// (the partial index is discarded by the caller, never registered).
func TestBackfillNodeHashIndex_ContextCancelled(t *testing.T) {
	t.Parallel()
	const n = backfillParallelMinNodes * 2
	g, _ := seedLabeledNamed(t, n, "Person")
	e := NewEngine(g)

	idx, err := newBoundNodeHashIndex(e.g, "Person", "name")
	if err != nil {
		t.Fatalf("newBoundNodeHashIndex: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before the backfill starts

	if berr := e.backfillNodeHashIndex(ctx, idx, "Person", "name"); berr == nil {
		t.Fatal("backfill with cancelled context returned nil, want context.Canceled")
	} else if !errors.Is(berr, context.Canceled) {
		t.Fatalf("backfill error = %v, want context.Canceled", berr)
	}
}

// TestBackfillNodeHashIndex_SerialSmallGraph exercises the sub-threshold serial
// path (n < backfillParallelMinNodes) for the same identical-contents contract,
// so both branches of the size gate are covered.
func TestBackfillNodeHashIndex_SerialSmallGraph(t *testing.T) {
	t.Parallel()
	const n = 64 // well below the parallel floor
	g, names := seedLabeledNamed(t, n, "Person")
	e := NewEngine(g)

	idx, err := newBoundNodeHashIndex(e.g, "Person", "name")
	if err != nil {
		t.Fatalf("newBoundNodeHashIndex: %v", err)
	}
	if berr := e.backfillNodeHashIndex(context.Background(), idx, "Person", "name"); berr != nil {
		t.Fatalf("backfill: %v", berr)
	}
	var total uint64
	for name := range names {
		total += idx.Lookup(name).GetCardinality()
	}
	if total != uint64(n) {
		t.Fatalf("total indexed pairs = %d, want %d", total, n)
	}
}
