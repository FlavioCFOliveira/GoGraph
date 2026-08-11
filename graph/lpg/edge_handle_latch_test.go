package lpg

import (
	"sync"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
)

// The tests below pin the anyHandleProp latch that
// [Graph.AnyEdgeHandlePropertyEverWritten] exposes (rmp #2387). The latch lets
// a reader skip the by-handle property probe entirely, so a MISSED creation
// site would make a stored property invisible — a correctness bug traded for a
// CPU saving. There is therefore one test per creation site, per the
// count-the-call-sites rule, and each asserts BOTH that the latch is set and
// that the property still reads back.

func newMultigraph(t *testing.T) *Graph[string, float64] {
	t.Helper()
	return New[string, float64](adjlist.Config{Directed: true, Multigraph: true})
}

// TestHandlePropLatch_FalseOnFreshGraph is the precondition the whole
// optimisation rests on: a graph that has never recorded a by-handle property
// reports false, and the read it licenses skipping does return empty.
func TestHandlePropLatch_FalseOnFreshGraph(t *testing.T) {
	t.Parallel()
	g := newMultigraph(t)

	if g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("fresh graph reports a by-handle property was written")
	}

	// A graph built the Go-API way — AddEdgeH stamps a handle, SetEdgeProperty
	// writes the PER-PAIR store — must leave the latch false. This is exactly
	// the shape example 26 builds and the reason the probe was dead there.
	h, err := g.AddEdgeH("a", "b", 1)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	if err := g.SetEdgeProperty("a", "b", "since", Int64Value(2020)); err != nil {
		t.Fatalf("SetEdgeProperty: %v", err)
	}
	if g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("per-pair SetEdgeProperty latched the BY-HANDLE store")
	}
	if got := g.EdgePropertiesByHandle("a", "b", h); len(got) != 0 {
		t.Fatalf("EdgePropertiesByHandle = %v, want empty (latch said empty)", got)
	}
	// The per-pair property is of course still there; the latch says nothing
	// about it.
	if got, ok := g.GetEdgeProperty("a", "b", "since"); !ok || got.Kind() != PropInt64 {
		t.Fatalf("per-pair property lost: got %v ok=%v", got, ok)
	}
}

// TestHandlePropLatch_CreationSite_StringKeyed covers creation site 1 of 2:
// setEdgePropertyByHandleInfo in edge_handle.go, reached by the natural-key
// public API.
func TestHandlePropLatch_CreationSite_StringKeyed(t *testing.T) {
	t.Parallel()
	g := newMultigraph(t)

	h, err := g.AddEdgeH("a", "b", 1)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	if err := g.SetEdgePropertyByHandle("a", "b", h, "w", Int64Value(7)); err != nil {
		t.Fatalf("SetEdgePropertyByHandle: %v", err)
	}

	if !g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("SetEdgePropertyByHandle did not set the latch: a reader would skip a REAL property")
	}
	props := g.EdgePropertiesByHandle("a", "b", h)
	if v, ok := props["w"]; !ok {
		t.Fatalf("by-handle property invisible after write: %v", props)
	} else if i, ok := v.Int64(); !ok || i != 7 {
		t.Fatalf("by-handle property w = %v (int64 %d, ok %v), want 7", v, i, ok)
	}
}

// TestHandlePropLatch_CreationSite_IDKeyed covers creation site 2 of 2:
// setEdgePropertyByHandleIDInfo in edge_handle_durable.go, which is the
// snapshot / WAL recovery writer. A graph rebuilt from durable state must
// re-latch, or every recovered by-handle property would read back empty.
func TestHandlePropLatch_CreationSite_IDKeyed(t *testing.T) {
	t.Parallel()
	g := newMultigraph(t)

	h, err := g.AddEdgeH("a", "b", 1)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	srcID, ok := g.adj.Mapper().Lookup("a")
	if !ok {
		t.Fatal(`Mapper has no id for "a"`)
	}
	dstID, ok := g.adj.Mapper().Lookup("b")
	if !ok {
		t.Fatal(`Mapper has no id for "b"`)
	}

	g.SetEdgePropertyByHandleID(srcID, dstID, h, "w", Int64Value(9))

	if !g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("SetEdgePropertyByHandleID did not set the latch: recovery would restore invisible properties")
	}
	props := g.EdgePropertiesByHandleID(srcID, dstID, h)
	if v, ok := props["w"]; !ok {
		t.Fatalf("by-handle property invisible after ID-keyed write: %v", props)
	} else if i, ok := v.Int64(); !ok || i != 9 {
		t.Fatalf("by-handle property w = %v (int64 %d, ok %v), want 9", v, i, ok)
	}
	// The natural-key reader must agree with the NodeID-keyed writer.
	if got := g.EdgePropertiesByHandle("a", "b", h); len(got) != 1 {
		t.Fatalf("natural-key read disagrees with ID-keyed write: %v", got)
	}
}

// TestHandlePropLatch_MonotonicAcrossDelete pins the one-way property. Once
// set, the latch stays set even though the store is empty again, because
// under-reporting loses a read while over-reporting only costs a probe.
func TestHandlePropLatch_MonotonicAcrossDelete(t *testing.T) {
	t.Parallel()
	g := newMultigraph(t)

	h, err := g.AddEdgeH("a", "b", 1)
	if err != nil {
		t.Fatalf("AddEdgeH: %v", err)
	}
	if err := g.SetEdgePropertyByHandle("a", "b", h, "w", Int64Value(1)); err != nil {
		t.Fatalf("SetEdgePropertyByHandle: %v", err)
	}
	g.DelEdgePropertyByHandle("a", "b", h, "w")

	if got := g.EdgePropertiesByHandle("a", "b", h); len(got) != 0 {
		t.Fatalf("property survived the delete: %v", got)
	}
	if !g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("latch cleared by a delete: it must be monotonic")
	}
}

// TestHandlePropLatch_LatchedBeforeTheWriteCanBeVisible is the DECISIVE
// ordering test, and it is deterministic in both directions.
//
// The racing sibling below cannot be: the window between "the property becomes
// visible" and "the latch is stored" in a build that got the order wrong is a
// few nanoseconds wide, and a reader doing map lookups will essentially never
// land inside it. Moving the store after the unlock — the exact defect the
// latch exists to forbid — was verified to pass the racing test 10 times out of
// 10. A test that a real defect walks straight through is not an oracle.
//
// So instead of racing the writer, this test HOLDS THE SHARD LOCK the writer
// must take. While the lock is held the writer is parked before it can publish
// anything, so the latch's value at that moment answers the question exactly:
//
//   - stored before the write is visible  ⇒ latch is already true while parked
//   - stored after                        ⇒ latch stays false until we unlock
//
// The poll below therefore fails by TIMEOUT on a mis-ordered build and passes in
// microseconds on a correct one, with a budget generous enough that host load
// cannot turn it into a flake.
// It is table-driven over BOTH creation sites, because the latch has two
// writers — the natural-key public API and the snapshot/WAL replay writer — and
// a fix or a regression on one says nothing about the other.
func TestHandlePropLatch_LatchedBeforeTheWriteCanBeVisible(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		write func(g *Graph[string, float64], srcID, dstID graph.NodeID, h uint64)
	}{
		{
			"string-keyed", // setEdgePropertyByHandleInfo, edge_handle.go
			func(g *Graph[string, float64], _, _ graph.NodeID, h uint64) {
				_ = g.SetEdgePropertyByHandle("a", "b", h, "w", Int64Value(1))
			},
		},
		{
			"id-keyed", // setEdgePropertyByHandleIDInfo, edge_handle_durable.go
			func(g *Graph[string, float64], srcID, dstID graph.NodeID, h uint64) {
				g.SetEdgePropertyByHandleID(srcID, dstID, h, "w", Int64Value(1))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := newMultigraph(t)

			h, err := g.AddEdgeH("a", "b", 1)
			if err != nil {
				t.Fatalf("AddEdgeH: %v", err)
			}
			if g.AnyEdgeHandlePropertyEverWritten() {
				t.Fatal("latch true before any by-handle property was written")
			}

			srcID, ok := g.adj.Mapper().Lookup("a")
			if !ok {
				t.Fatal("source not interned")
			}
			dstID, ok := g.adj.Mapper().Lookup("b")
			if !ok {
				t.Fatal("destination not interned")
			}
			sh := g.edgeHandlePropShardFor(edgeKey{src: srcID, dst: dstID})

			// Park the writer: it cannot reach any publishing step while we
			// hold this.
			sh.mu.Lock()

			done := make(chan struct{})
			go func() {
				defer close(done)
				tc.write(g, srcID, dstID, h)
			}()

			deadline := time.Now().Add(5 * time.Second)
			latched := false
			for time.Now().Before(deadline) {
				if g.AnyEdgeHandlePropertyEverWritten() {
					latched = true
					break
				}
				time.Sleep(time.Millisecond)
			}
			// Release before asserting, so a failure cannot deadlock the writer.
			sh.mu.Unlock()
			<-done

			if !latched {
				t.Fatal("the latch was still false while the writer was parked on the shard lock: " +
					"it is being stored AFTER the write becomes visible, so a reader can observe " +
					"a by-handle property with the latch reading false and skip a probe that would " +
					"have returned data")
			}
			if got := g.EdgePropertiesByHandle("a", "b", h); len(got) == 0 {
				t.Fatal("property missing after the writer completed")
			}
		})
	}
}

// TestHandlePropLatch_SetBeforeVisible is the ordering assertion the latch's
// soundness argument rests on: a reader that observes the property must also
// observe the latch.
//
// It is a SMOKE TEST, not the oracle — see
// TestHandlePropLatch_LatchedBeforeTheWriteCanBeVisible for the decisive one.
// What it is good for is proving that ordinary concurrent traffic produces no
// violation, which is a different question from whether a mis-ordered build
// would be caught.
//
// THE ORDER OF THE TWO READS IS THE WHOLE TEST. The reader samples the
// PROPERTY first and the LATCH second, because the property is the earlier
// event in the ordering under test: the writer stores the latch and only then
// makes the property visible, so a reader that saw the property at T1 must see
// the latch at any T2 > T1. A false latch there is a genuine violation.
//
// Sampling them the other way round — latch first, property second — does NOT
// test the ordering and cannot be made to pass: between the two reads a writer
// may latch and write, so the reader legitimately holds a stale `false` while
// seeing the value, and the test reports a violation that never happened. That
// is what this test did when it was introduced (67977beb, 2026-08-10), which is
// why it failed deterministically from its first commit. A reader observing
// "no properties" while a concurrent write is in flight is simply a read
// ordered before that write, which is what the latch's documented contract
// ("false is exact") means for a concurrent caller.
func TestHandlePropLatch_SetBeforeVisible(t *testing.T) {
	t.Parallel()
	g := newMultigraph(t)

	const pairs = 64
	handles := make([]uint64, pairs)
	for i := range handles {
		h, err := g.AddEdgeH("src", handleLatchKey(i), 1)
		if err != nil {
			t.Fatalf("AddEdgeH %d: %v", i, err)
		}
		handles[i] = h
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	violations := make(chan string, pairs)

	// Writers: latch, then write.
	for i := range handles {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if err := g.SetEdgePropertyByHandle("src", handleLatchKey(i), handles[i], "w", Int64Value(int64(i))); err != nil {
				violations <- "write failed"
			}
		}(i)
	}
	// Readers: read the property, THEN the latch. Seeing a value whose latch is
	// still false when sampled afterwards is the violation. See the doc comment
	// for why this order, and only this order, expresses the property.
	for i := range handles {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			for attempt := 0; attempt < 64; attempt++ {
				got := g.EdgePropertiesByHandle("src", handleLatchKey(i), handles[i])
				latched := g.AnyEdgeHandlePropertyEverWritten()
				if len(got) > 0 && !latched {
					violations <- "read a by-handle property while the latch was false"
					return
				}
			}
		}(i)
	}

	close(start)
	wg.Wait()
	close(violations)
	for v := range violations {
		t.Fatalf("ordering violated: %s", v)
	}

	if !g.AnyEdgeHandlePropertyEverWritten() {
		t.Fatal("latch false after 64 by-handle writes")
	}
}

// handleLatchKey gives each of the 64 concurrent writers its OWN destination
// node, so every write targets a distinct (src, dst) pair and therefore a
// distinct handle. Two writers sharing a pair would still exercise the
// ordering, but a distinct pair per writer spreads them across the property
// shards, which is where the latch has to hold.
func handleLatchKey(i int) string {
	return string(rune('a'+i%26)) + string(rune('a'+i/26))
}
