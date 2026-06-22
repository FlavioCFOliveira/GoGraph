package adjlist

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

// TestAdjList_Weightless_Accessor pins the Weightless accessor against both
// Config branches.
func TestAdjList_Weightless_Accessor(t *testing.T) {
	t.Parallel()
	if New[string, int64](Config{Directed: true, Weightless: true}).Weightless() != true {
		t.Fatal("Weightless() = false for Weightless:true config")
	}
	if New[string, int64](Config{Directed: true}).Weightless() != false {
		t.Fatal("Weightless() = true for default config")
	}
}

// TestAdjList_Weightless_NoWeightColumn is the core invariant test: a
// weightless graph never allocates the weights column on ANY mutation path
// (fresh entry, in-place fast append, grow/slow append), so LoadEntry and
// LoadEntryH always return a nil weights slice even though a non-zero weight
// was passed to AddEdge. The published entry's weights field must be strictly
// nil (not a zero-length non-nil slice) — that is what the snapshot writer's
// `weights != nil` gate keys off to persist hasWeights=0.
func TestAdjList_Weightless_NoWeightColumn(t *testing.T) {
	t.Parallel()
	a := New[string, int64](Config{Directed: true})
	a2 := New[string, int64](Config{Directed: true, Weightless: true})

	// Append enough edges from one source to force the grow/slow path several
	// times (geometric capacity 4 -> 8 -> 16 ...), so the fresh, fast, and slow
	// append paths are all exercised.
	const degree = 50
	for i := 0; i < degree; i++ {
		dst := "n" + itoa(i)
		// Pass a deliberately non-zero, non-uniform weight to prove it is
		// ignored by the weightless graph and stored by the weighted one.
		w := int64(1000 + i)
		mustAddEdge(t, a, "hub", dst, w)
		mustAddEdge(t, a2, "hub", dst, w)
	}

	idHub, _ := a.Mapper().Lookup("hub")
	idHub2, _ := a2.Mapper().Lookup("hub")

	// Weighted graph: weights present, length-aligned, carrying the values.
	nb, ws := a.LoadEntry(idHub)
	if len(nb) != degree {
		t.Fatalf("weighted: neighbours len=%d, want %d", len(nb), degree)
	}
	if ws == nil || len(ws) != degree {
		t.Fatalf("weighted: weights len=%d (nil=%t), want %d non-nil", len(ws), ws == nil, degree)
	}

	// Weightless graph: same neighbours, but weights strictly nil.
	nb2, ws2 := a2.LoadEntry(idHub2)
	if len(nb2) != degree {
		t.Fatalf("weightless: neighbours len=%d, want %d", len(nb2), degree)
	}
	if ws2 != nil {
		t.Fatalf("weightless: LoadEntry weights = %v, want nil (len %d)", ws2, len(ws2))
	}

	// LoadEntryH must likewise return nil weights for the weightless graph.
	nb2H, ws2H, _ := a2.LoadEntryH(idHub2)
	if len(nb2H) != degree {
		t.Fatalf("weightless: LoadEntryH neighbours len=%d, want %d", len(nb2H), degree)
	}
	if ws2H != nil {
		t.Fatalf("weightless: LoadEntryH weights = %v, want nil", ws2H)
	}

	// Direct entry inspection: prove the published immutable entry's weights
	// field is itself nil, not merely the LoadEntry return.
	s := &a2.shards[idHub2&shardMask]
	e := loadEntry[int64](s, uint64(idHub2)>>shardBits)
	if e == nil {
		t.Fatal("weightless: hub entry unexpectedly nil")
	}
	if e.weights != nil {
		t.Fatalf("weightless: adjEntry.weights = %v, want strictly nil", e.weights)
	}
}

// TestAdjList_Weightless_NeighboursYieldsZero confirms the Neighbours iterator
// yields the zero value of W for every neighbour of a weightless graph (the
// all-zero "unweighted" representation), while topology (neighbour set) is
// fully intact.
func TestAdjList_Weightless_NeighboursYieldsZero(t *testing.T) {
	t.Parallel()
	a := New[string, int64](Config{Directed: true, Weightless: true})
	mustAddEdge(t, a, "a", "b", 99)
	mustAddEdge(t, a, "a", "c", 7)

	got := map[string]int64{}
	for v, w := range a.Neighbours("a") {
		got[v] = w
	}
	if len(got) != 2 {
		t.Fatalf("Neighbours(a) yielded %d entries, want 2", len(got))
	}
	for _, dst := range []string{"b", "c"} {
		w, ok := got[dst]
		if !ok {
			t.Fatalf("Neighbours(a) missing %q", dst)
		}
		if w != 0 {
			t.Fatalf("Neighbours(a) weight for %q = %d, want 0 (weightless)", dst, w)
		}
	}
	// Topology probes are weight-independent and must still be correct.
	if !a.HasEdge("a", "b") || !a.HasEdge("a", "c") {
		t.Fatal("weightless: HasEdge lost a live edge")
	}
	if a.Size() != 2 {
		t.Fatalf("weightless: Size = %d, want 2", a.Size())
	}
}

// TestAdjList_Weightless_RemoveCompactKeepNil exercises the compactEntry path
// (RemoveEdge on a multi-edge source) and the trimEntry path (Compact) for a
// weightless graph, proving the weights column stays nil through both — neither
// a removal nor a Compact may resurrect a zero-filled weights slice.
func TestAdjList_Weightless_RemoveCompactKeepNil(t *testing.T) {
	t.Parallel()
	a := New[string, int64](Config{Directed: true, Weightless: true})
	for i := 0; i < 10; i++ {
		mustAddEdge(t, a, "hub", "n"+itoa(i), int64(i+1))
	}
	// Remove an interior edge: exercises compactEntry (excise slot idx).
	a.RemoveEdge("hub", "n5")
	idHub, _ := a.Mapper().Lookup("hub")
	if _, ws := a.LoadEntry(idHub); ws != nil {
		t.Fatalf("weightless after RemoveEdge: weights = %v, want nil", ws)
	}
	if e := loadEntry[int64](&a.shards[idHub&shardMask], uint64(idHub)>>shardBits); e != nil && e.weights != nil {
		t.Fatalf("weightless after RemoveEdge: entry.weights = %v, want nil", e.weights)
	}

	// Compact: exercises trimEntry. The geometric-growth slack in neighbours is
	// reclaimed, but the nil weights column must remain nil.
	a.Compact(context.Background())
	if _, ws := a.LoadEntry(idHub); ws != nil {
		t.Fatalf("weightless after Compact: weights = %v, want nil", ws)
	}
	if e := loadEntry[int64](&a.shards[idHub&shardMask], uint64(idHub)>>shardBits); e != nil && e.weights != nil {
		t.Fatalf("weightless after Compact: entry.weights = %v, want nil", e.weights)
	}
	// Topology after removal+compaction: 9 edges, n5 gone.
	if a.Size() != 9 {
		t.Fatalf("weightless: Size after remove = %d, want 9", a.Size())
	}
	if a.HasEdge("hub", "n5") {
		t.Fatal("weightless: RemoveEdge did not remove n5")
	}
}

// TestAdjList_Weightless_UndirectedMirror confirms the undirected mirror append
// (cross-shard two-lock path included) keeps both directions weightless.
func TestAdjList_Weightless_UndirectedMirror(t *testing.T) {
	t.Parallel()
	a := New[int, int64](Config{Weightless: true}) // undirected
	// Use ids that land in different shards (low 8 bits differ) to drive the
	// cross-shard mirror path in addEdge.
	mustAddEdge(t, a, 0, 1, 42)
	id0, _ := a.Mapper().Lookup(0)
	id1, _ := a.Mapper().Lookup(1)
	if _, ws := a.LoadEntry(id0); ws != nil {
		t.Fatalf("weightless undirected fwd: weights = %v, want nil", ws)
	}
	if _, ws := a.LoadEntry(id1); ws != nil {
		t.Fatalf("weightless undirected mirror: weights = %v, want nil", ws)
	}
	if !a.HasEdge(0, 1) || !a.HasEdge(1, 0) {
		t.Fatal("weightless undirected: mirror edge missing")
	}
}

// TestAdjList_Weightless_ConcurrentReadsConsistentPrefix mirrors the lock-free
// read contract test (torn_read_test.go) for a weightless graph: a reader
// observing a concurrently-grown adjacency must always see a consistent prefix.
// This proves the nil-weights column is published by the same single atomic
// adjEntry pointer swap and never tears, even though the weights column path
// now branches on Weightless. Run under -race this also catches any unsynced
// access introduced by the new branches.
func TestAdjList_Weightless_ConcurrentReadsConsistentPrefix(t *testing.T) {
	t.Parallel()

	const N = 10_000
	numReaders := max(2, runtime.GOMAXPROCS(0)-1)

	a := New[int, int64](Config{Directed: true, Weightless: true})

	startCh := make(chan struct{})
	writerDone := make(chan struct{})
	done := make(chan struct{})

	var violations atomic.Int64
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(writerDone)
		<-startCh
		for k := 1; k <= N; k++ {
			if err := a.AddEdge(0, k, int64(k)); err != nil {
				violations.Add(1)
				return
			}
		}
	}()

	wg.Add(numReaders)
	for range numReaders {
		go func() {
			defer wg.Done()
			<-startCh
			for {
				select {
				case <-done:
					return
				default:
				}
				var seen []int
				for v, w := range a.Neighbours(0) {
					// Weightless contract: every yielded weight is the zero value.
					if w != 0 {
						violations.Add(1)
					}
					seen = append(seen, v)
				}
				if !isWeightlessConsistentPrefix(seen, N) {
					violations.Add(1)
				}
				seen = seen[:0:0]
				runtime.Gosched()
			}
		}()
	}

	timer := time.AfterFunc(120*time.Second, func() {
		select {
		case <-done:
		default:
			close(done)
		}
	})
	defer timer.Stop()

	close(startCh)

	select {
	case <-writerDone:
		select {
		case <-done:
		default:
			close(done)
		}
	case <-done:
		t.Error("timeout: writer did not finish within deadline")
	}

	wg.Wait()

	if v := violations.Load(); v != 0 {
		t.Errorf("weightless consistent-prefix/zero-weight violations: %d", v)
	}
}

// isWeightlessConsistentPrefix reports whether observed is a valid prefix of
// [1, 2, 3, …] not exceeding maxN in length.
func isWeightlessConsistentPrefix(observed []int, maxN int) bool {
	if len(observed) > maxN {
		return false
	}
	for i, v := range observed {
		if v != i+1 {
			return false
		}
	}
	return true
}

// itoa is a tiny base-10 itoa avoiding a strconv import in this test file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ensure graph import is used even if a future edit drops the only reference.
var _ = graph.NodeID(0)
