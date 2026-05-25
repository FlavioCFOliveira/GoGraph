package csr_test

import (
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// buildFragmentedGraph constructs a directed graph with n total nodes where
// only the first m nodes (0..m-1) participate in edges (directed cycle among
// them). The remaining n-m nodes are inserted via AddNode only and have no
// incident edges, making them invisible to LiveMask.
func buildFragmentedGraph(n, m int) (*adjlist.AdjList[int, int64], *csr.CSR[int64]) {
	a := adjlist.New[int, int64](adjlist.Config{Directed: true})
	for i := 0; i < n; i++ {
		if err := a.AddNode(i); err != nil {
			panic(err)
		}
	}
	for i := 0; i < m; i++ {
		if err := a.AddEdge(i, (i+1)%m, 1); err != nil {
			panic(err)
		}
	}
	return a, csr.BuildFromAdjList(a)
}

// TestCSR_LiveMask_Fragmentation_ZeroPercent verifies that when all n nodes
// form a directed cycle (0% fragmentation), LiveCount equals Order.
func TestCSR_LiveMask_Fragmentation_ZeroPercent(t *testing.T) {
	t.Parallel()
	const n = 30
	a, c := buildFragmentedGraph(n, n)

	if got := a.Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	if got := c.LiveCount(); got != n {
		t.Fatalf("LiveCount = %d, want %d", got, n)
	}
	if c.LiveCount() != int(a.Order()) {
		t.Fatalf("LiveCount %d != Order %d", c.LiveCount(), a.Order())
	}
}

// TestCSR_LiveMask_Fragmentation_ThirtyThreePercent verifies that when 10 of
// 30 nodes are isolated (33% fragmentation), LiveCount equals 20.
func TestCSR_LiveMask_Fragmentation_ThirtyThreePercent(t *testing.T) {
	t.Parallel()
	const (
		n = 30
		m = 20
	)
	a, c := buildFragmentedGraph(n, m)

	if got := a.Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	if got := c.LiveCount(); got != m {
		t.Fatalf("LiveCount = %d, want %d", got, m)
	}
}

// TestCSR_LiveMask_Fragmentation_SixtyPercent verifies that when 18 of 30
// nodes are isolated (60% fragmentation), LiveCount equals 12.
func TestCSR_LiveMask_Fragmentation_SixtyPercent(t *testing.T) {
	t.Parallel()
	const (
		n = 30
		m = 12
	)
	a, c := buildFragmentedGraph(n, m)

	if got := a.Order(); got != uint64(n) {
		t.Fatalf("Order = %d, want %d", got, n)
	}
	if got := c.LiveCount(); got != m {
		t.Fatalf("LiveCount = %d, want %d", got, m)
	}
}

// TestCSR_LiveMask_BitCount verifies that the number of true entries in
// LiveMask always matches LiveCount for each fragmentation level.
func TestCSR_LiveMask_BitCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		n, m int
	}{
		{"zero_pct", 30, 30},
		{"thirtythree_pct", 30, 20},
		{"sixty_pct", 30, 12},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, c := buildFragmentedGraph(tc.n, tc.m)
			mask := c.LiveMask()
			count := 0
			for _, live := range mask {
				if live {
					count++
				}
			}
			if count != c.LiveCount() {
				t.Errorf("LiveMask bit count %d != LiveCount %d", count, c.LiveCount())
			}
		})
	}
}

// TestCSR_LiveMask_SurvivingKeys verifies that every NodeID returned by
// LiveNodes resolves to one of the active keys (0..m-1) in the mapper.
func TestCSR_LiveMask_SurvivingKeys(t *testing.T) {
	t.Parallel()
	const (
		n = 30
		m = 20
	)
	a, c := buildFragmentedGraph(n, m)

	liveNodes := c.LiveNodes()
	if len(liveNodes) != m {
		t.Fatalf("len(LiveNodes) = %d, want %d", len(liveNodes), m)
	}
	for _, id := range liveNodes {
		key, ok := a.Mapper().Resolve(id)
		if !ok {
			t.Errorf("LiveNodes returned NodeID %d not in mapper", id)
			continue
		}
		if key >= m {
			t.Errorf("LiveNodes NodeID %d resolved to isolated key %d (>= m=%d)", id, key, m)
		}
	}
}

// TestCSR_LiveCount_TableDriven is a table-driven test covering all three
// fragmentation rates in one function.
func TestCSR_LiveCount_TableDriven(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		n, m          int
		wantLiveCount int
		wantOrder     int
	}{
		{
			name:          "zero_percent_fragmentation",
			n:             30,
			m:             30,
			wantLiveCount: 30,
			wantOrder:     30,
		},
		{
			name:          "thirtythree_percent_fragmentation",
			n:             30,
			m:             20,
			wantLiveCount: 20,
			wantOrder:     30,
		},
		{
			name:          "sixty_percent_fragmentation",
			n:             30,
			m:             12,
			wantLiveCount: 12,
			wantOrder:     30,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, c := buildFragmentedGraph(tc.n, tc.m)

			if got := int(a.Order()); got != tc.wantOrder {
				t.Errorf("Order = %d, want %d", got, tc.wantOrder)
			}
			if got := c.LiveCount(); got != tc.wantLiveCount {
				t.Errorf("LiveCount = %d, want %d", got, tc.wantLiveCount)
			}

			// Verify LiveMask bit count matches LiveCount.
			mask := c.LiveMask()
			bitCount := 0
			for _, live := range mask {
				if live {
					bitCount++
				}
			}
			if bitCount != c.LiveCount() {
				t.Errorf("LiveMask bit count %d != LiveCount %d", bitCount, c.LiveCount())
			}
		})
	}
}
