//go:build soak

package community

import (
	"testing"

	"github.com/FlavioCFOliveira/GoGraph/graph"
	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
	"github.com/FlavioCFOliveira/GoGraph/graph/lpg"
	"github.com/FlavioCFOliveira/GoGraph/internal/shapegen"
	"github.com/FlavioCFOliveira/GoGraph/internal/testlayers"
)

// lfrGroundTruth extracts the "community_id" property written by LFR
// into a NodeID-indexed slice aligned with the CSR MaxNodeID boundary.
// LFR uses "community_id" (not "block_id") as the property key.
// Slots with no mapping remain -1.
func lfrGroundTruth(g *lpg.Graph[int, int64], a *adjlist.AdjList[int, int64], n int) []int {
	gt := make([]int, n)
	for i := range gt {
		gt[i] = -1
	}
	a.Mapper().Walk(func(id graph.NodeID, key int) bool {
		prop, ok := g.GetNodeProperty(key, "community_id")
		if ok {
			v, ok2 := prop.Int64()
			if ok2 {
				gt[id] = int(v)
			}
		}
		return true
	})
	return gt
}

// TestLeiden_LFR_MuSweep sweeps the LFR mixing parameter mu from 0.10
// to 0.50 and asserts that Leiden's NMI against the planted ground
// truth meets decreasing thresholds as the community signal degrades.
//
// Parameters: n=2000, gamma=2.5, beta=1.5, avgDeg=20, maxDeg=80,
// minCom=20, maxCom=100, seed=42.
func TestLeiden_LFR_MuSweep(t *testing.T) {
	testlayers.RequireSoak(t)
	t.Parallel()

	cases := []struct {
		muPct  int
		minNMI float64
	}{
		{10, 0.95},
		{20, 0.90},
		{30, 0.80},
		{40, 0.65},
		{50, 0.45},
	}

	for _, tc := range cases {
		tc := tc
		t.Run("mu"+itoa(tc.muPct), func(t *testing.T) {
			t.Parallel()
			g, err := shapegen.LFR(
				2000,     // n
				250,      // gammaPercent (2.5)
				150,      // betaPercent  (1.5)
				20,       // avgDeg
				80,       // maxDeg
				20,       // minCom
				100,      // maxCom
				tc.muPct, // muPercent
				42,       // seed
			).Build(adjlist.Config{Directed: false})
			if err != nil {
				// LFR is a rejection sampler; some parameter combinations
				// can fail. Skip rather than fail hard so CI stays green.
				t.Skipf("LFR Build failed (mu=%d%%): %v", tc.muPct, err)
			}
			a := g.AdjList()
			c := csr.BuildFromAdjList(a)

			p := Leiden(c, DefaultLeidenOptions())

			n := int(c.MaxNodeID())
			gt := lfrGroundTruth(g, a, n)

			pred := make([]int, 0, 2000)
			gtLive := make([]int, 0, 2000)
			for i := 0; i < n; i++ {
				if p.Community[i] >= 0 && gt[i] >= 0 {
					pred = append(pred, p.Community[i])
					gtLive = append(gtLive, gt[i])
				}
			}

			// Count unique community IDs in gt to get k2.
			seen := make(map[int]struct{}, 128)
			for _, v := range gtLive {
				if v >= 0 {
					seen[v] = struct{}{}
				}
			}
			k2 := len(seen)
			if k2 == 0 {
				t.Skip("LFR produced no communities in ground truth")
			}

			nmiVal := nmi(pred, gtLive, p.NumCommunities, k2)
			if nmiVal < tc.minNMI {
				t.Errorf("mu=%d%%: NMI = %.4f, want >= %.2f (NumCommunities=%d, k_gt=%d)",
					tc.muPct, nmiVal, tc.minNMI, p.NumCommunities, k2)
			}
		})
	}
}

// itoa converts a non-negative int to its decimal string representation
// without importing strconv or fmt.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := [20]byte{}
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
