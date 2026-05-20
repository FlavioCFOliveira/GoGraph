package plan_test

import (
	"testing"

	"gograph/cypher/plan"
	"gograph/graph"
	"gograph/graph/index"
	"gograph/graph/index/btree"
	"gograph/graph/index/hash"
	"gograph/graph/index/label"
)

// TestIndexEstimator_LabelCount verifies that LabelCount delegates
// correctly to the underlying label.Index bitmap.
func TestIndexEstimator_LabelCount(t *testing.T) {
	t.Parallel()

	idx := label.NewNodeIndex()
	for i := graph.NodeID(0); i < 50; i++ {
		idx.Add(1, i)
	}
	for i := graph.NodeID(0); i < 30; i++ {
		idx.Add(2, i)
	}

	est := plan.NewIndexEstimator(idx, nil)

	tests := []struct {
		name  string
		label uint32
		want  uint64
	}{
		{"known label 1", 1, 50},
		{"known label 2", 2, 30},
		{"unknown label 99", 99, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := est.LabelCount(tc.label)
			if got != tc.want {
				t.Errorf("LabelCount(%d) = %d, want %d", tc.label, got, tc.want)
			}
		})
	}
}

// TestIndexEstimator_NilSafe verifies that every Estimator method is
// safe to call when the estimator has nil labelIdx and nil idxMgr.
func TestIndexEstimator_NilSafe(t *testing.T) {
	t.Parallel()

	est := plan.NewIndexEstimator(nil, nil)

	t.Run("LabelCount", func(t *testing.T) {
		t.Parallel()
		if got := est.LabelCount(1); got != 0 {
			t.Errorf("LabelCount on nil estimator = %d, want 0", got)
		}
	})
	t.Run("HashLookupCount", func(t *testing.T) {
		t.Parallel()
		if got := est.HashLookupCount(1, "x"); got != 0 {
			t.Errorf("HashLookupCount on nil estimator = %d, want 0", got)
		}
	})
	t.Run("BTreeRangeCount", func(t *testing.T) {
		t.Parallel()
		if got := est.BTreeRangeCount(1, "a", "z"); got != 0 {
			t.Errorf("BTreeRangeCount on nil estimator = %d, want 0", got)
		}
	})
	t.Run("AvgOutDegree", func(t *testing.T) {
		t.Parallel()
		if got := est.AvgOutDegree(1, 2, 3); got != 1.0 {
			t.Errorf("AvgOutDegree on nil estimator = %f, want 1.0", got)
		}
	})
}

// TestIndexEstimator_AvgOutDegree verifies the cache set/get round-trip
// and that the default for an uncached key is 1.0.
func TestIndexEstimator_AvgOutDegree(t *testing.T) {
	t.Parallel()

	est := plan.NewIndexEstimator(nil, nil)

	tests := []struct {
		src, rel, dst uint32
		avg           float64
	}{
		{1, 10, 2, 3.5},
		{2, 20, 3, 12.0},
		{0, 0, 0, 0.5},
	}

	// Initially all unknown → 1.0.
	for _, tc := range tests {
		got := est.AvgOutDegree(tc.src, tc.rel, tc.dst)
		if got != 1.0 {
			t.Errorf("AvgOutDegree before cache = %f, want 1.0", got)
		}
	}

	// Populate cache and verify round-trip.
	for _, tc := range tests {
		est.UpdateDegreeCache(tc.src, tc.rel, tc.dst, tc.avg)
	}
	for _, tc := range tests {
		got := est.AvgOutDegree(tc.src, tc.rel, tc.dst)
		if got != tc.avg {
			t.Errorf("AvgOutDegree(%d,%d,%d) = %f, want %f",
				tc.src, tc.rel, tc.dst, got, tc.avg)
		}
	}
}

// TestIndexEstimator_CacheHitRate verifies that calling LabelCount 100
// times on the same label returns a consistent value. This is a smoke
// test confirming the label.Index is read correctly on every call.
func TestIndexEstimator_CacheHitRate(t *testing.T) {
	t.Parallel()

	idx := label.NewNodeIndex()
	const nodeCount = 200
	for i := graph.NodeID(0); i < nodeCount; i++ {
		idx.Add(7, i)
	}
	est := plan.NewIndexEstimator(idx, nil)

	const iterations = 100
	for i := 0; i < iterations; i++ {
		got := est.LabelCount(7)
		if got != nodeCount {
			t.Fatalf("iteration %d: LabelCount(7) = %d, want %d", i, got, nodeCount)
		}
	}
}

// TestEstimatorAccuracy creates a label.Index with 100k nodes spread
// across 5 labels and verifies that LabelCount returns exact counts
// (within 0% error since the implementation reads the bitmap directly).
func TestEstimatorAccuracy(t *testing.T) {
	t.Parallel()

	const (
		labels     = 5
		perLabel   = 20_000
		totalNodes = labels * perLabel
	)

	idx := label.NewNodeIndex()
	for lbl := uint32(0); lbl < labels; lbl++ {
		base := graph.NodeID(lbl * perLabel)
		for i := graph.NodeID(0); i < perLabel; i++ {
			idx.Add(lbl, base+i)
		}
	}

	est := plan.NewIndexEstimator(idx, nil)

	for lbl := uint32(0); lbl < labels; lbl++ {
		got := est.LabelCount(lbl)
		if got != perLabel {
			t.Errorf("LabelCount(%d) = %d, want %d", lbl, got, perLabel)
		}
	}

	_ = totalNodes // declared for documentation clarity
}

// TestIndexEstimator_HashLookupCount verifies that HashLookupCount
// returns a non-zero estimate when a hash index is registered with
// the manager and the index contains entries.
func TestIndexEstimator_HashLookupCount(t *testing.T) {
	t.Parallel()

	mgr := index.NewManager()
	hi := hash.New[string]()
	// Insert 1000 distinct values, one node each.
	for i := 0; i < 1000; i++ {
		// Construct a short distinct key.
		key := string([]byte{byte(i >> 8), byte(i)})
		hi.Insert(key, graph.NodeID(i))
	}
	if err := mgr.CreateIndex("prop:name", hi); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	// Give the label index a total-node count under label 0.
	labelIdx := label.NewNodeIndex()
	for i := graph.NodeID(0); i < 5000; i++ {
		labelIdx.Add(0, i)
	}

	est := plan.NewIndexEstimator(labelIdx, mgr)
	got := est.HashLookupCount(42, "anything")

	// Expect total/distinct = 5000/1000 = 5.
	if got == 0 {
		t.Fatal("HashLookupCount returned 0; expected non-zero estimate")
	}
	const want uint64 = 5
	if got != want {
		t.Errorf("HashLookupCount = %d, want %d", got, want)
	}
}

// TestIndexEstimator_BTreeRangeCount verifies that BTreeRangeCount
// returns a 30% selectivity estimate when a btree index is registered.
func TestIndexEstimator_BTreeRangeCount(t *testing.T) {
	t.Parallel()

	mgr := index.NewManager()
	bi := btree.New[string]()
	// BulkLoad 100 distinct string values.
	vals := make([]string, 100)
	nodes := make([]graph.NodeID, 100)
	for i := 0; i < 100; i++ {
		vals[i] = string([]byte{byte(i)})
		nodes[i] = graph.NodeID(i)
	}
	if err := bi.BulkLoad(vals, nodes); err != nil {
		t.Fatalf("BulkLoad: %v", err)
	}
	if err := mgr.CreateIndex("prop:age", bi); err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}

	est := plan.NewIndexEstimator(nil, mgr)
	got := est.BTreeRangeCount(0, "a", "z")

	// 30% of 100 distinct values = 30.
	const want uint64 = 30
	if got != want {
		t.Errorf("BTreeRangeCount = %d, want %d", got, want)
	}
}
