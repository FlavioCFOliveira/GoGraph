package csrfile

import (
	"fmt"
	"math/rand/v2"

	"github.com/FlavioCFOliveira/GoGraph/graph/adjlist"
	"github.com/FlavioCFOliveira/GoGraph/graph/csr"
)

// FixtureSpec parameterises [BuildFixture]. The same seed produces
// the same graph every time, making the harness deterministic.
type FixtureSpec struct {
	// Vertices is the number of pre-interned vertex IDs.
	Vertices uint64
	// Edges is the number of edges to add (uniformly random
	// (src, dst) over [0, Vertices)).
	Edges uint64
	// Seed is the PCG seed (any uint64).
	Seed uint64
	// Multigraph allows parallel edges; without it duplicates are
	// silently collapsed.
	Multigraph bool
}

// BuildFixture deterministically builds a CSR snapshot meeting spec.
// The graph is directed and uses uint32 vertex identifiers; weights
// are absent (struct{}). Suitable for Tier 2 benchmarks and the
// crash-recovery harness, where reproducibility matters more than
// realism.
//
// BuildFixture returns any error surfaced by the underlying
// [adjlist.AdjList]; with the default uncapped configuration the
// only failure mode is [adjlist.ErrShardFull], which cannot be
// reached because no [adjlist.Config.MaxShardCapacity] is set.
func BuildFixture(spec FixtureSpec) (*csr.CSR[struct{}], error) {
	a := adjlist.New[uint32, struct{}](adjlist.Config{Directed: true, Multigraph: spec.Multigraph})
	for i := uint64(0); i < spec.Vertices; i++ {
		if err := a.AddNode(uint32(i)); err != nil {
			return nil, fmt.Errorf("csrfile.BuildFixture: AddNode(%d): %w", i, err)
		}
	}
	r := rand.New(rand.NewPCG(spec.Seed, 0x9E3779B97F4A7C15)) //nolint:gosec // deterministic fixture RNG
	universe := uint32(spec.Vertices)
	for i := uint64(0); i < spec.Edges; i++ {
		src := uint32(r.Uint32() % universe)
		dst := uint32(r.Uint32() % universe)
		if err := a.AddEdge(src, dst, struct{}{}); err != nil {
			return nil, fmt.Errorf("csrfile.BuildFixture: AddEdge[%d]: %w", i, err)
		}
	}
	return csr.BuildFromAdjList(a), nil
}
