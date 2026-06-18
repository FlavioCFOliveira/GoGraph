package adjlist

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// entryFor is a white-box helper returning the published *adjEntry for the
// node currently identified by src, or nil when src has no outgoing edges.
func entryFor[N comparable, W any](a *AdjList[N, W], src N) *adjEntry[W] {
	id, ok := a.mapper.Lookup(src)
	if !ok {
		return nil
	}
	return loadEntry[W](&a.shards[id&shardMask], uint64(id)>>shardBits)
}

// TestCompact_TrimsSlack confirms Compact right-sizes every column to exact
// length while leaving the live [0:len] contents (neighbours, weights,
// handles, labels) and their alignment unchanged.
func TestCompact_TrimsSlack(t *testing.T) {
	t.Parallel()
	a := New[int, int](Config{Directed: true, Multigraph: true})

	// 6 parallel edges from 0 forces geometric growth (cap 4 -> 8), leaving
	// slack (len 6, cap 8). Each slot carries a distinct handle and label so
	// the alignment of every column is verifiable after the trim.
	const degree = 6
	for i := 0; i < degree; i++ {
		if err := a.AddEdgeLabeledH(0, i+1, i*10, uint64(i+1), uint32(i+100)); err != nil {
			t.Fatalf("AddEdgeLabeledH: %v", err)
		}
	}

	pre := entryFor(a, 0)
	if pre == nil {
		t.Fatal("no entry for node 0")
	}
	if cap(pre.neighbours) == len(pre.neighbours) {
		t.Fatalf("test precondition: expected slack, got cap == len == %d", len(pre.neighbours))
	}

	a.Compact(context.Background())

	post := entryFor(a, 0)
	if post == nil {
		t.Fatal("Compact dropped the entry for node 0")
	}
	// Every column right-sized to exact length.
	if got := cap(post.neighbours); got != degree {
		t.Errorf("neighbours cap = %d, want %d (len)", got, degree)
	}
	if got := cap(post.weights); got != degree {
		t.Errorf("weights cap = %d, want %d (len)", got, degree)
	}
	if got := cap(post.handles); got != degree {
		t.Errorf("handles cap = %d, want %d (len)", got, degree)
	}
	if got := cap(post.labels); got != degree {
		t.Errorf("labels cap = %d, want %d (len)", got, degree)
	}
	// Live contents and alignment preserved. The stored neighbour is the
	// interned NodeID of the external value, not the value itself, so resolve
	// each NodeID back to its external int to assert alignment.
	if len(post.neighbours) != degree {
		t.Fatalf("neighbours len = %d, want %d", len(post.neighbours), degree)
	}
	for i := 0; i < degree; i++ {
		ext, ok := a.mapper.Resolve(post.neighbours[i])
		if !ok {
			t.Errorf("neighbours[%d] = %d resolves to no external value", i, post.neighbours[i])
			continue
		}
		if ext != i+1 {
			t.Errorf("neighbours[%d] resolves to %d, want %d", i, ext, i+1)
		}
		if post.weights[i] != i*10 {
			t.Errorf("weights[%d] = %d, want %d", i, post.weights[i], i*10)
		}
		if post.handles[i] != uint64(i+1) {
			t.Errorf("handles[%d] = %d, want %d", i, post.handles[i], i+1)
		}
		if post.labels[i] != uint32(i+100) {
			t.Errorf("labels[%d] = %d, want %d", i, post.labels[i], i+100)
		}
	}
}

// TestCompact_PreservesNilColumns confirms a label-free / handle-free graph
// keeps its optional columns nil after Compact (no zero-length slice gained),
// so downstream nil checks behave identically.
func TestCompact_PreservesNilColumns(t *testing.T) {
	t.Parallel()
	a := New[int, int](Config{Directed: true, Multigraph: true})
	for i := 0; i < 6; i++ {
		mustAddEdge(t, a, 0, i+1, i)
	}

	a.Compact(context.Background())

	post := entryFor(a, 0)
	if post == nil {
		t.Fatal("Compact dropped the entry for node 0")
	}
	if post.handles != nil {
		t.Errorf("handles = %v, want nil (handle-free graph)", post.handles)
	}
	if post.labels != nil {
		t.Errorf("labels = %v, want nil (label-free graph)", post.labels)
	}
	if cap(post.neighbours) != len(post.neighbours) {
		t.Errorf("neighbours not trimmed: cap %d, len %d", cap(post.neighbours), len(post.neighbours))
	}
}

// TestCompact_SkipsTightEntries confirms an entry that already has no slack is
// left untouched (same pointer), avoiding useless re-publication churn.
func TestCompact_SkipsTightEntries(t *testing.T) {
	t.Parallel()
	a := New[int, int](Config{Directed: true, Multigraph: true})
	// 4 edges land exactly at the initial capacity (growCap(0) == 4), so the
	// entry has cap == len == 4 and no slack.
	for i := 0; i < 4; i++ {
		mustAddEdge(t, a, 0, i+1, i)
	}
	before := entryFor(a, 0)
	if cap(before.neighbours) != len(before.neighbours) {
		t.Fatalf("test precondition: expected tight entry, got cap %d len %d",
			cap(before.neighbours), len(before.neighbours))
	}

	a.Compact(context.Background())

	after := entryFor(a, 0)
	if before != after {
		t.Errorf("Compact republished a tight (no-slack) entry; want the same pointer")
	}
}

// TestCompact_ReclaimsCapacity confirms the aggregate backing capacity drops
// after Compact across many slack-bearing nodes (the memory win), with the
// edge set unchanged.
func TestCompact_ReclaimsCapacity(t *testing.T) {
	t.Parallel()
	a := New[int, int](Config{Directed: true, Multigraph: true})
	const nodes, degree = 200, 5 // degree 5 -> cap 8, ~37% slack per node
	for src := 0; src < nodes; src++ {
		for d := 0; d < degree; d++ {
			mustAddEdge(t, a, src, nodes+src*degree+d, d)
		}
	}

	sumCap := func() (total int) {
		for src := 0; src < nodes; src++ {
			if e := entryFor(a, src); e != nil {
				total += cap(e.neighbours)
			}
		}
		return total
	}
	before := sumCap()
	beforeSize := a.Size()

	a.Compact(context.Background())

	after := sumCap()
	if after >= before {
		t.Errorf("aggregate cap not reduced: before %d, after %d", before, after)
	}
	if want := nodes * degree; after != want {
		t.Errorf("aggregate cap after Compact = %d, want %d (exact len)", after, want)
	}
	if a.Size() != beforeSize {
		t.Errorf("Compact changed Size: %d -> %d", beforeSize, a.Size())
	}
}

// TestCompact_ConcurrentReadersSeeConsistentSnapshot runs Compact while many
// goroutines iterate the adjacency. Under -race this pins the lock-free read
// path against Compact's atomic republication: a reader must always observe a
// complete neighbour set (either the old or the trimmed entry), never a torn
// or partial one.
func TestCompact_ConcurrentReadersSeeConsistentSnapshot(t *testing.T) {
	t.Parallel()
	a := New[int, int](Config{Directed: true, Multigraph: true})
	const nodes, degree = 64, 6
	for src := 0; src < nodes; src++ {
		for d := 0; d < degree; d++ {
			mustAddEdge(t, a, src, 1000+src*degree+d, d)
		}
	}

	var (
		wg   sync.WaitGroup
		stop atomic.Bool
	)
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				for src := 0; src < nodes; src++ {
					n := 0
					for range a.Neighbours(src) {
						n++
					}
					if n != degree {
						t.Errorf("reader saw %d neighbours for %d, want %d", n, src, degree)
						return
					}
				}
			}
		}()
	}
	// Compact repeatedly while readers iterate. After the first pass entries
	// are tight, so later passes skip them, but the concurrency is exercised.
	for i := 0; i < 50; i++ {
		a.Compact(context.Background())
	}
	stop.Store(true)
	wg.Wait()
}

// TestCompact_HonoursCancellation confirms a cancelled context stops Compact
// without corrupting state (whatever shards were processed stay consistent).
func TestCompact_HonoursCancellation(t *testing.T) {
	t.Parallel()
	a := New[int, int](Config{Directed: true, Multigraph: true})
	for i := 0; i < 6; i++ {
		mustAddEdge(t, a, 0, i+1, i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.Compact(ctx) // returns promptly; must not panic or corrupt state.

	// The graph remains fully readable and unchanged in edge set.
	n := 0
	for range a.Neighbours(0) {
		n++
	}
	if n != 6 {
		t.Errorf("after cancelled Compact, node 0 has %d neighbours, want 6", n)
	}
}
