package label

import (
	"sync"
	"testing"

	"pgregory.net/rapid"

	"github.com/FlavioCFOliveira/GoGraph/graph"
)

func TestIndex_AddHasCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		ops       func(idx *Index)
		label     uint32
		wantCount uint64
		probe     graph.NodeID
		wantHas   bool
	}{
		{
			name:      "empty index",
			ops:       func(*Index) {},
			label:     1,
			wantCount: 0,
			probe:     graph.NodeID(0),
			wantHas:   false,
		},
		{
			name: "single add",
			ops: func(i *Index) {
				i.Add(7, graph.NodeID(3))
			},
			label:     7,
			wantCount: 1,
			probe:     graph.NodeID(3),
			wantHas:   true,
		},
		{
			name: "duplicate add is idempotent",
			ops: func(i *Index) {
				i.Add(7, graph.NodeID(3))
				i.Add(7, graph.NodeID(3))
				i.Add(7, graph.NodeID(3))
			},
			label:     7,
			wantCount: 1,
			probe:     graph.NodeID(3),
			wantHas:   true,
		},
		{
			name: "two distinct nodes under one label",
			ops: func(i *Index) {
				i.Add(7, graph.NodeID(3))
				i.Add(7, graph.NodeID(11))
			},
			label:     7,
			wantCount: 2,
			probe:     graph.NodeID(11),
			wantHas:   true,
		},
		{
			name: "probe on unknown label",
			ops: func(i *Index) {
				i.Add(7, graph.NodeID(3))
			},
			label:     99,
			wantCount: 0,
			probe:     graph.NodeID(3),
			wantHas:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := NewIndex()
			tc.ops(idx)
			if got := idx.Count(tc.label); got != tc.wantCount {
				t.Fatalf("Count(%d) = %d, want %d", tc.label, got, tc.wantCount)
			}
			if got := idx.Has(tc.label, tc.probe); got != tc.wantHas {
				t.Fatalf("Has(%d, %d) = %v, want %v", tc.label, tc.probe, got, tc.wantHas)
			}
		})
	}
}

func TestIndex_Remove(t *testing.T) {
	t.Parallel()
	t.Run("remove present", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		idx.Add(1, graph.NodeID(3))
		idx.Remove(1, graph.NodeID(2))
		if idx.Has(1, graph.NodeID(2)) {
			t.Fatalf("Has(1, 2) should be false after Remove")
		}
		if !idx.Has(1, graph.NodeID(3)) {
			t.Fatalf("Has(1, 3) should still be true after Remove(1, 2)")
		}
		if got := idx.Count(1); got != 1 {
			t.Fatalf("Count(1) = %d, want 1", got)
		}
	})
	t.Run("remove absent is no-op", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		idx.Remove(1, graph.NodeID(99))
		if !idx.Has(1, graph.NodeID(2)) {
			t.Fatalf("Has(1, 2) should still be true after no-op Remove")
		}
	})
	t.Run("remove from unknown label is no-op", func(t *testing.T) {
		idx := NewIndex()
		idx.Remove(42, graph.NodeID(1))
		if got := idx.Count(42); got != 0 {
			t.Fatalf("Count(42) = %d, want 0", got)
		}
	})
	t.Run("removing last entry deletes the label bucket", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(7, graph.NodeID(3))
		idx.Remove(7, graph.NodeID(3))
		if got := idx.Count(7); got != 0 {
			t.Fatalf("Count(7) = %d, want 0 after removing last", got)
		}
		idx.mu.RLock()
		_, present := idx.bits[7]
		idx.mu.RUnlock()
		if present {
			t.Fatalf("label bucket 7 should be deleted after the last Remove emptied it")
		}
	})
}

func TestIndex_Intersect(t *testing.T) {
	t.Parallel()
	t.Run("no labels returns empty", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		got := idx.Intersect()
		if !got.IsEmpty() {
			t.Fatalf("Intersect() empty-labels should yield empty bitmap, got cardinality %d", got.GetCardinality())
		}
	})
	t.Run("single label returns clone of that label", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		idx.Add(1, graph.NodeID(3))
		got := idx.Intersect(1)
		if got.GetCardinality() != 2 {
			t.Fatalf("Intersect(1) cardinality = %d, want 2", got.GetCardinality())
		}
		got.Add(99) // mutating the result must not affect the index
		if idx.Count(1) != 2 {
			t.Fatalf("mutating Intersect result leaked into the index")
		}
	})
	t.Run("missing label short-circuits to empty", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		got := idx.Intersect(1, 99)
		if !got.IsEmpty() {
			t.Fatalf("Intersect with a missing label should be empty")
		}
	})
	t.Run("normal intersection", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		idx.Add(1, graph.NodeID(3))
		idx.Add(1, graph.NodeID(4))
		idx.Add(2, graph.NodeID(3))
		idx.Add(2, graph.NodeID(4))
		idx.Add(2, graph.NodeID(5))
		got := idx.Intersect(1, 2)
		if got.GetCardinality() != 2 {
			t.Fatalf("Intersect(1, 2) cardinality = %d, want 2", got.GetCardinality())
		}
		if !got.Contains(3) || !got.Contains(4) {
			t.Fatalf("Intersect(1, 2) should be {3, 4}, got %v", got.ToArray())
		}
	})
	t.Run("disjoint labels yield empty", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		idx.Add(2, graph.NodeID(3))
		got := idx.Intersect(1, 2)
		if !got.IsEmpty() {
			t.Fatalf("disjoint Intersect should be empty, got %v", got.ToArray())
		}
	})
}

func TestIndex_Union(t *testing.T) {
	t.Parallel()
	t.Run("no labels returns empty", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		got := idx.Union()
		if !got.IsEmpty() {
			t.Fatalf("Union() empty-labels should yield empty bitmap")
		}
	})
	t.Run("missing label is skipped", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		got := idx.Union(1, 99)
		if got.GetCardinality() != 1 || !got.Contains(2) {
			t.Fatalf("Union(1, missing) should be {2}, got %v", got.ToArray())
		}
	})
	t.Run("normal union", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		idx.Add(1, graph.NodeID(3))
		idx.Add(2, graph.NodeID(4))
		got := idx.Union(1, 2)
		if got.GetCardinality() != 3 {
			t.Fatalf("Union(1, 2) cardinality = %d, want 3", got.GetCardinality())
		}
		if !got.Contains(2) || !got.Contains(3) || !got.Contains(4) {
			t.Fatalf("Union(1, 2) should be {2, 3, 4}, got %v", got.ToArray())
		}
	})
	t.Run("result is independent of the index", func(t *testing.T) {
		idx := NewIndex()
		idx.Add(1, graph.NodeID(2))
		got := idx.Union(1)
		got.Add(99)
		if idx.Has(1, graph.NodeID(99)) {
			t.Fatalf("mutating Union result leaked into the index")
		}
	})
}

// buildRandomIndex draws a random sequence of Add/Remove operations and
// returns both the resulting Index and a baseline map[label][node]struct{}{}
// modelling the same state, so each property test can verify the Index
// against a simpler reference.
func buildRandomIndex(r *rapid.T) (idx *Index, baseline map[uint32]map[graph.NodeID]struct{}, nLabels, nNodes int) {
	idx = NewIndex()
	nLabels = rapid.IntRange(1, 4).Draw(r, "nLabels")
	nNodes = rapid.IntRange(1, 16).Draw(r, "nNodes")
	ops := rapid.IntRange(0, 32).Draw(r, "ops")
	baseline = map[uint32]map[graph.NodeID]struct{}{}
	for i := 0; i < ops; i++ {
		label := uint32(rapid.IntRange(0, nLabels-1).Draw(r, "label"))
		node := graph.NodeID(rapid.IntRange(0, nNodes-1).Draw(r, "node"))
		if rapid.Bool().Draw(r, "isRemove") {
			idx.Remove(label, node)
			if m, ok := baseline[label]; ok {
				delete(m, node)
				if len(m) == 0 {
					delete(baseline, label)
				}
			}
			continue
		}
		idx.Add(label, node)
		if baseline[label] == nil {
			baseline[label] = map[graph.NodeID]struct{}{}
		}
		baseline[label][node] = struct{}{}
	}
	return idx, baseline, nLabels, nNodes
}

func TestProperty_CountMatchesBaseline(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		idx, baseline, nLabels, _ := buildRandomIndex(r)
		for l := uint32(0); l < uint32(nLabels); l++ {
			want := uint64(len(baseline[l]))
			if got := idx.Count(l); got != want {
				t.Fatalf("Count(%d) = %d, want %d", l, got, want)
			}
		}
	})
}

func TestProperty_HasMatchesBaseline(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		idx, baseline, nLabels, nNodes := buildRandomIndex(r)
		for l := uint32(0); l < uint32(nLabels); l++ {
			for n := 0; n < nNodes; n++ {
				_, wantHas := baseline[l][graph.NodeID(n)]
				if got := idx.Has(l, graph.NodeID(n)); got != wantHas {
					t.Fatalf("Has(%d, %d) = %v, want %v", l, n, got, wantHas)
				}
			}
		}
	})
}

func TestProperty_UnionAllEqualsAllSeen(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		idx, baseline, nLabels, _ := buildRandomIndex(r)
		seen := map[graph.NodeID]struct{}{}
		for _, m := range baseline {
			for n := range m {
				seen[n] = struct{}{}
			}
		}
		labels := make([]uint32, 0, nLabels)
		for l := uint32(0); l < uint32(nLabels); l++ {
			labels = append(labels, l)
		}
		if got := idx.Union(labels...).GetCardinality(); got != uint64(len(seen)) {
			t.Fatalf("Union(all) cardinality = %d, want %d", got, len(seen))
		}
	})
}

func TestProperty_CommutativityAndIdempotence(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		idx, _, nLabels, _ := buildRandomIndex(r)
		if nLabels >= 2 {
			a, b := uint32(0), uint32(1)
			if !idx.Union(a, b).Equals(idx.Union(b, a)) {
				t.Fatalf("Union not commutative")
			}
			if !idx.Intersect(a, b).Equals(idx.Intersect(b, a)) {
				t.Fatalf("Intersect not commutative")
			}
		}
		l := uint32(0)
		if !idx.Union(l, l).Equals(idx.Union(l)) {
			t.Fatalf("Union not idempotent on label %d", l)
		}
		if !idx.Intersect(l, l).Equals(idx.Intersect(l)) {
			t.Fatalf("Intersect not idempotent on label %d", l)
		}
	})
}

func TestProperty_AddRemoveInverse(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(r *rapid.T) {
		idx := NewIndex()
		label := uint32(rapid.IntRange(0, 7).Draw(r, "label"))
		node := graph.NodeID(rapid.IntRange(0, 31).Draw(r, "node"))
		had := idx.Has(label, node)
		idx.Add(label, node)
		idx.Remove(label, node)
		if got := idx.Has(label, node); got != had {
			t.Fatalf("Add then Remove must restore membership: had=%v, got=%v", had, got)
		}
	})
}

func TestConcurrent_ReadersAgainstWriter(t *testing.T) {
	t.Parallel()
	const (
		readers = 8
		ops     = 2000
		labels  = 16
		nodes   = 256
	)
	idx := NewIndex()
	// Pre-populate to make readers meaningful.
	for l := uint32(0); l < labels; l++ {
		for n := 0; n < nodes; n++ {
			if (n+int(l))%3 == 0 {
				idx.Add(l, graph.NodeID(n))
			}
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	// Concurrent readers exercising every read path.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			counter := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				l := uint32(counter % labels)
				_ = idx.Count(l)
				_ = idx.Has(l, graph.NodeID((counter*7+seed)%nodes))
				_ = idx.Intersect(l, (l+1)%labels)
				_ = idx.Union(l, (l+2)%labels)
				counter++
			}
		}(r)
	}
	// Single writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < ops; i++ {
			l := uint32(i % labels)
			n := graph.NodeID(i % nodes)
			if i%2 == 0 {
				idx.Add(l, n)
			} else {
				idx.Remove(l, n)
			}
		}
		close(stop)
	}()
	wg.Wait()
}
