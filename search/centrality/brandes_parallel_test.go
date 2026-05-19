package centrality

import (
	"math"
	"math/rand/v2"
	"testing"

	"gograph/graph/adjlist"
	"gograph/graph/csr"
)

// TestBetweennessParallel_VsSerial asserts the parallel
// implementation produces bit-identical output to the serial
// Brandes on random graphs of varying topology.
func TestBetweennessParallel_VsSerial(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(197, 199)) //nolint:gosec // deterministic
	for seed := 0; seed < 5; seed++ {
		a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
		const n = 64
		for i := 0; i < 2*n; i++ {
			a.AddEdge(r.IntN(n), r.IntN(n), struct{}{})
		}
		c := csr.BuildFromAdjList(a)
		serial := Betweenness(c)
		parallel := BetweennessParallel(c, 4)
		for i, sv := range serial {
			// 1e-9 absolute tolerance covers the float-summation
			// reorder that comes with the parallel reduce; the
			// algorithmic result is identical, only the addition
			// order differs.
			if math.Abs(sv-parallel[i]) > 1e-9 {
				t.Fatalf("seed=%d, node %d: serial=%f parallel=%f", seed, i, sv, parallel[i])
			}
		}
	}
}

func BenchmarkBetweenness_Serial(b *testing.B) {
	c, _ := buildSerialBrandesFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Betweenness(c)
	}
}

func BenchmarkBetweenness_Parallel(b *testing.B) {
	c, _ := buildSerialBrandesFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BetweennessParallel(c, 0)
	}
}

func buildSerialBrandesFixture() (c *csr.CSR[struct{}], n int) {
	n = 512
	a := adjlist.New[int, struct{}](adjlist.Config{Directed: false})
	r := rand.New(rand.NewPCG(211, 223)) //nolint:gosec // deterministic
	for i := 0; i < 3*n; i++ {
		a.AddEdge(r.IntN(n), r.IntN(n), struct{}{})
	}
	c = csr.BuildFromAdjList(a)
	return c, n
}
